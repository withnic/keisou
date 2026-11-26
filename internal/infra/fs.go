package infra

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

func NormalizeText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func ReadTextFile(ctx context.Context, path string) (string, error) {
	data, err := ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%q must contain UTF-8 text", path)
	}
	return NormalizeText(string(data)), nil
}

// ReadFile bounds reads from regular files, preserving the original bytes.
func ReadFile(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, maxJSONSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if len(data) > maxJSONSize {
		return nil, fmt.Errorf("%q exceeds %d bytes", path, maxJSONSize)
	}
	return data, ctx.Err()
}

const (
	fileTimeout    = 30 * time.Second
	jsonlChunkSize = 64 << 10
	maxJSONLLine   = 10 << 20
	maxJSONSize    = 50 << 20
)

func AtomicWriteFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	return writeFile(ctx, path, data, perm, os.Rename)
}

// AtomicCreateFile publishes the completed file only if the target does not exist.
func AtomicCreateFile(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	return atomicCreateWithLink(ctx, path, data, perm, os.Link)
}

func atomicCreateWithLink(ctx context.Context, path string, data []byte, perm os.FileMode, link func(string, string) error) error {
	return writeFile(ctx, path, data, perm, func(tempPath, targetPath string) error {
		err := link(tempPath, targetPath)
		if err == nil || errors.Is(err, os.ErrExist) {
			return err
		}
		// Some filesystems and restricted Windows environments disallow hard links.
		// Reserve the target exclusively with O_CREATE|O_EXCL, then replace it atomically.
		dst, createErr := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if createErr != nil {
			return createErr
		}
		if closeErr := dst.Close(); closeErr != nil {
			return errors.Join(closeErr, os.Remove(targetPath))
		}
		if renameErr := os.Rename(tempPath, targetPath); renameErr != nil {
			return errors.Join(renameErr, os.Remove(targetPath))
		}
		return nil
	})
}

func writeFile(ctx context.Context, path string, data []byte, perm os.FileMode, publish func(string, string) error) error {
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".keisou-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(perm); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := publish(file.Name(), path); err != nil {
		return fmt.Errorf("publish %q: %w", path, err)
	}
	return nil
}

// ReadLinesReverse visits non-empty valid UTF-8 lines from newest to oldest.
func ReadLinesReverse(ctx context.Context, path string, visit func([]byte) bool) error {
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file for reverse read: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file for reverse read: %w", err)
	}

	var fragments [][]byte
	size := 0
	discardUntilNewline := false
	add := func(fragment []byte) {
		if discardUntilNewline || len(fragment) == 0 {
			return
		}
		if size+len(fragment) > maxJSONLLine {
			fragments, size, discardUntilNewline = nil, 0, true
			return
		}
		fragments = append(fragments, bytes.Clone(fragment))
		size += len(fragment)
	}
	emit := func(first bool) bool {
		if discardUntilNewline || size == 0 {
			return true
		}
		line := make([]byte, 0, size)
		for i := len(fragments) - 1; i >= 0; i-- {
			line = append(line, fragments[i]...)
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if first {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(line) == 0 || !utf8.Valid(line) {
			return true
		}
		return visit(line)
	}

	buf := make([]byte, jsonlChunkSize)
	for end := info.Size(); end > 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := max(0, end-jsonlChunkSize)
		chunk := buf[:end-start]
		if _, err := file.ReadAt(chunk, start); err != nil {
			return fmt.Errorf("read file at %d: %w", start, err)
		}
		for {
			newline := bytes.LastIndexByte(chunk, '\n')
			add(chunk[newline+1:])
			if newline < 0 {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !emit(false) {
				return nil
			}
			fragments, size, discardUntilNewline = fragments[:0], 0, false
			chunk = chunk[:newline]
		}
		end = start
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	emit(true)
	return ctx.Err()
}

// ReadJSONLReverse visits valid JSON records from newest to oldest.
func ReadJSONLReverse(ctx context.Context, path string, visit func([]byte) bool) error {
	return ReadLinesReverse(ctx, path, func(line []byte) bool {
		if !json.Valid(line) {
			return true
		}
		return visit(line)
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// ReadJSON provides a streaming decoder with a size cap, even if the file grows
// after being opened. The callback controls how the JSON is decoded.
func ReadJSON(ctx context.Context, path string, decode func(*json.Decoder) error) error {
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open JSON: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat JSON: %w", err)
	}
	if info.Size() > maxJSONSize {
		return fmt.Errorf("JSON exceeds %d bytes", maxJSONSize)
	}
	limited := &io.LimitedReader{R: contextReader{ctx, file}, N: maxJSONSize + 1}
	reader := bufio.NewReader(limited)
	if prefix, _ := reader.Peek(3); bytes.Equal(prefix, []byte("\xef\xbb\xbf")) {
		_, _ = reader.Discard(3)
	}
	err = decode(json.NewDecoder(reader))
	if limited.N == 0 {
		return fmt.Errorf("JSON exceeds %d bytes", maxJSONSize)
	}
	if err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}
	return ctx.Err()
}
