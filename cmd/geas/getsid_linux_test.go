//go:build linux

package main

import "syscall"

// The linux syscall package has no Getsid wrapper.
func getsid(pid int) (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}
