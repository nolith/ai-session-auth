//go:build darwin

package main

import "syscall"

func getsid(pid int) (int, error) { return syscall.Getsid(pid) }
