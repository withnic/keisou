package infra

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestAtomicWriteFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	path := filepath.Join(dir, "config.json")
	ctx := context.Background()
	if err := AtomicCreateFile(ctx, path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCreateFile(ctx, path, []byte("rejected"), 0600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create = %v, want existence error", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "old" {
		t.Fatalf("protected data = %q, %v", data, err)
	}
	if err := AtomicWriteFile(ctx, path, []byte("\xef\xbb\xbf日本語\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "日本語\n" {
		t.Fatalf("replacement data = %q, %v", data, err)
	}
	if err := AtomicWriteFile(ctx, dir, []byte("failed replacement"), 0600); err == nil {
		t.Fatal("replacing a directory unexpectedly succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.json" {
		t.Fatalf("write residue: %v, %v", entries, err)
	}
	parent, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(parent) != 1 || parent[0].Name() != "nested" {
		t.Fatalf("temporary file left after failed rename: %v, %v", parent, err)
	}
}

func TestJSONLReverse(t *testing.T) {
	split := "\"日本語" + strings.Repeat("x", jsonlChunkSize-3) + "\""
	for _, test := range []struct {
		name string
		file string
		stop bool
		want []string
	}{
		{"empty", "", false, nil},
		{"no newline", `{"a":1}`, false, []string{`{"a":1}`}},
		{"UTF-8 split mid-rune", split, false, []string{split}},
		{"oversize", "null\n\"" + strings.Repeat("x", maxJSONLLine) + "\"\ntrue\n", false, []string{"true", "null"}},
		{"early exit", "1\n2\n3\n", true, []string{"3"}},
		{"corrupt and CRLF", "\xef\xbb\xbf1\r\n{partial\n2\r\n{", false, []string{"2", "1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(test.file), 0600); err != nil {
				t.Fatal(err)
			}
			var got []string
			err := ReadJSONLReverse(context.Background(), path, func(line []byte) bool {
				got = append(got, string(line))
				return !test.stop
			})
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("line count = %d, want %d (content equal: %t); err = %v", len(got), len(test.want), reflect.DeepEqual(got, test.want), err)
			}
		})
	}
}

func TestAtomicCreateFileFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	ctx := context.Background()
	denyLink := func(string, string) error { return os.ErrPermission }

	if err := atomicCreateWithLink(ctx, path, []byte("first\r\n"), 0600, denyLink); err != nil {
		t.Fatalf("fallback create failed: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first\n" {
		t.Fatalf("fallback content = %q, %v", data, err)
	}
	if err := atomicCreateWithLink(ctx, path, []byte("overwrite"), 0600, denyLink); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback overwrite = %v, want os.ErrExist", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first\n" {
		t.Fatalf("protected content altered = %q, %v", data, err)
	}

	racePath := filepath.Join(dir, "race.json")
	const contenders = 8
	wins := make(chan int, contenders)
	var group sync.WaitGroup
	for i := 0; i < contenders; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			err := atomicCreateWithLink(ctx, racePath, []byte("winner"), 0600, denyLink)
			if err == nil {
				wins <- 1
			} else if !errors.Is(err, os.ErrExist) {
				t.Errorf("unexpected fallback race error: %v", err)
			}
		}()
	}
	group.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("concurrent fallback winners = %d, want 1", len(wins))
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected residue after fallback: %v, %v", entries, err)
	}
}

func TestReadJSONLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf{\r\n \"value\": 3\r\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var value map[string]int
	decode := func(d *json.Decoder) error { return d.Decode(&value) }
	if err := ReadJSON(context.Background(), path, decode); err != nil || value["value"] != 3 {
		t.Fatalf("decode = %v, %v", value, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ReadJSON(canceled, path, decode); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReadJSON = %v, want context.Canceled", err)
	}
	if err := os.Truncate(path, maxJSONSize+1); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(context.Background(), path, decode); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over-limit read = %v", err)
	}
}

func TestReadLinesReverse(t *testing.T) {
	split := "日本語" + strings.Repeat("x", jsonlChunkSize-3)
	path := filepath.Join(t.TempDir(), "telemetry.log")
	content := "\xef\xbb\xbfINFO startup\r\n" + split + "\n" + string([]byte{0xff, 0xfe}) + "\n" + strings.Repeat("y", maxJSONLLine+1) + "\n{\"event\":\"ok\"}\n\nWARN 429 RESOURCE_EXHAUSTED\r\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := ReadLinesReverse(context.Background(), path, func(line []byte) bool {
		got = append(got, string(line))
		return true
	})
	want := []string{"WARN 429 RESOURCE_EXHAUSTED", `{"event":"ok"}`, split, "INFO startup"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadLinesReverse count = %d (equal: %t), err = %v", len(got), reflect.DeepEqual(got, want), err)
	}
	got = nil
	err = ReadLinesReverse(context.Background(), path, func(line []byte) bool {
		got = append(got, string(line))
		return false
	})
	if err != nil || !reflect.DeepEqual(got, []string{"WARN 429 RESOURCE_EXHAUSTED"}) {
		t.Fatalf("ReadLinesReverse early exit = %v, %v", got, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ReadLinesReverse(canceled, path, func([]byte) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReadLinesReverse = %v, want context.Canceled", err)
	}
}
