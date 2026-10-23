package sshgrant

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether fd is a terminal, as the shell's -t does.
func isTerminal(fd int) bool {
	var state syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), getTermios, uintptr(unsafe.Pointer(&state)))
	return errno == 0
}
