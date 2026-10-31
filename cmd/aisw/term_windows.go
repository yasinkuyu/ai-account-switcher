package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	enableProcessedInput     = 0x0001
	enableLineInput          = 0x0002
	enableEchoInput          = 0x0004
	enableVirtualTermInput   = 0x0200
	enableVirtualTermProcess = 0x0004
	enableProcessedOutput    = 0x0001
)

func consoleMode(f *os.File) (uint32, bool) {
	var m uint32
	r, _, _ := procGetConsoleMode.Call(f.Fd(), uintptr(unsafe.Pointer(&m)))
	return m, r != 0
}

func setConsoleMode(f *os.File, m uint32) { procSetConsoleMode.Call(f.Fd(), uintptr(m)) }

// enableVT turns on ANSI escape sequences for output (Windows 10+).
func enableVT() {
	if m, ok := consoleMode(os.Stdout); ok {
		setConsoleMode(os.Stdout, m|enableVirtualTermProcess|enableProcessedOutput)
	}
}

// rawMode reads keys one at a time, as VT sequences, without echo.
// Ctrl-C arrives as byte 3 and is handled by the menu.
func rawMode() (func(), error) {
	in, ok := consoleMode(os.Stdin)
	if !ok {
		return nil, syscall.EINVAL
	}
	setConsoleMode(os.Stdin, (in&^(enableLineInput|enableEchoInput|enableProcessedInput))|enableVirtualTermInput)
	return func() { setConsoleMode(os.Stdin, in) }, nil
}

func isTerminal(f *os.File) bool {
	_, ok := consoleMode(f)
	return ok
}
