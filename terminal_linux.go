//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

func getTerminalState(fd int) (syscall.Termios, error) {
	var state syscall.Termios
	err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&state))
	return state, err
}
func setTerminalState(fd int, state *syscall.Termios) error {
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(state))
}
