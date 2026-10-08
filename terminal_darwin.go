//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

func getTerminalState(fd int) (syscall.Termios, error) {
	var state syscall.Termios
	err := ioctl(fd, syscall.TIOCGETA, unsafe.Pointer(&state))
	return state, err
}
func setTerminalState(fd int, state *syscall.Termios) error {
	return ioctl(fd, syscall.TIOCSETA, unsafe.Pointer(state))
}
