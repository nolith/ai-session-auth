//go:build darwin

package sshgrant

import "syscall"

const getTermios = syscall.TIOCGETA
