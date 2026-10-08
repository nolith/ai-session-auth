package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func ioctl(fd int, request uintptr, pointer unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(pointer))
	if errno != 0 {
		return errno
	}
	return nil
}
func terminalProcessGroup(fd int) (int, error) {
	var group int32
	err := ioctl(fd, syscall.TIOCGPGRP, unsafe.Pointer(&group))
	return int(group), err
}
func setTerminalProcessGroup(fd, group int) error {
	value := int32(group)
	return ioctl(fd, syscall.TIOCSPGRP, unsafe.Pointer(&value))
}
func readIssuer(ctx context.Context, input *os.File, output io.Writer, fromStdin bool) (string, error) {
	if !fromStdin {
		state, err := getTerminalState(int(input.Fd()))
		if err != nil {
			return "", errors.New("issuer prompt requires a terminal; use save-gitlab-issuer --stdin for a pipe")
		}
		hidden := state
		hidden.Lflag &^= syscall.ECHO
		if err = setTerminalState(int(input.Fd()), &hidden); err != nil {
			return "", err
		}
		defer setTerminalState(int(input.Fd()), &state)
		fmt.Fprint(output, "GitLab issuer PAT (not the agent PAT): ")
		defer fmt.Fprintln(output)
	}
	type result struct {
		text string
		ok   bool
	}
	resultChannel := make(chan result, 1)
	go func() {
		scanner := bufio.NewScanner(io.LimitReader(input, 8192))
		ok := scanner.Scan()
		resultChannel <- result{scanner.Text(), ok}
	}()
	var value result
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case value = <-resultChannel:
	}
	if !value.ok {
		return "", errors.New("cannot read issuer PAT")
	}
	token := strings.TrimSpace(value.text)
	if token == "" {
		return "", errors.New("empty issuer PAT")
	}
	return token, nil
}
