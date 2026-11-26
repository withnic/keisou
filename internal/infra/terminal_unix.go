//go:build !windows

package infra

import "io"

func EnableTerminalSequences(io.Writer) func() { return func() {} }
