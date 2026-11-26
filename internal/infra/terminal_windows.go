package infra

import (
	"io"
	"os"
	"syscall"
)

var setConsoleMode = kernel32.NewProc("SetConsoleMode")

// EnableTerminalSequences enables ANSI redraw on a Windows console and
// returns a function that restores its original mode.
func EnableTerminalSequences(w io.Writer) func() {
	const virtualTerminalProcessing = 0x0004
	noop := func() {}
	file, ok := w.(*os.File)
	if !ok {
		return noop
	}
	handle := file.Fd()
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Handle(handle), &mode); err != nil || mode&virtualTerminalProcessing != 0 {
		return noop
	}
	if ok, _, _ := setConsoleMode.Call(handle, uintptr(mode|virtualTerminalProcessing)); ok == 0 {
		return noop
	}
	return func() { setConsoleMode.Call(handle, uintptr(mode)) }
}
