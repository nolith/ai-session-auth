//go:build linux

package sshgrant

import "syscall"

const getTermios = syscall.TCGETS
