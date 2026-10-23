//go:build linux

package session

import (
	"errors"
	"fmt"
	"syscall"
)

// prSetNoNewPrivs is prctl(2)'s PR_SET_NO_NEW_PRIVS operation, from
// linux/prctl.h. The syscall package declares PR_SET_NO_NEW_PRIVS on the
// architectures its tables happen to carry it for, and amd64 is not among
// them, so name the kernel's ABI here; the value is the same everywhere.
const prSetNoNewPrivs = 0x26

// setNoNewPrivs sets no_new_privs on every thread of this process, so that
// nothing it runs, the harness included, can gain privileges through a
// setuid or file-capability program. Linux keeps the flag per thread, and a
// Go process has many, so a plain prctl would leave the others without it.
// AllThreadsSyscall refuses in a binary built with cgo, where some threads
// are not Go's.
func setNoNewPrivs() error {
	_, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0)
	switch {
	case errno == 0:
		return nil
	case errors.Is(errno, syscall.ENOTSUP):
		return errors.New("cannot set no_new_privs: this geas was built with cgo; build it with CGO_ENABLED=0")
	}
	return fmt.Errorf("cannot set no_new_privs: %v", errno)
}
