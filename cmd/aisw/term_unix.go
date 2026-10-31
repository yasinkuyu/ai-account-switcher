//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"
)

// rawMode switches the terminal to key-at-a-time input without echo, using
// the stty command every Unix ships with. It returns a restore function.
func rawMode() (func(), error) {
	get := exec.Command("stty", "-g")
	get.Stdin = os.Stdin
	saved, err := get.Output()
	if err != nil {
		return nil, err
	}
	set := exec.Command("stty", "-icanon", "-echo", "min", "1", "time", "0")
	set.Stdin = os.Stdin
	if err := set.Run(); err != nil {
		return nil, err
	}
	restore := func() {
		r := exec.Command("stty", strings.TrimSpace(string(saved)))
		r.Stdin = os.Stdin
		_ = r.Run()
	}
	// Ctrl-C still raises SIGINT in this mode: put the terminal back first.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		os.Stdout.WriteString(screenLeave)
		restore()
		os.Exit(130)
	}()
	return func() { signal.Stop(sig); restore() }, nil
}

func enableVT() {}

// isTerminal reports whether f is an interactive terminal (not a pipe,
// file or /dev/null).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return e == 0
}
