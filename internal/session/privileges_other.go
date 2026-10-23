//go:build !linux

package session

import "errors"

// setNoNewPrivs exists only on Linux; the configuration refuses
// no_new_privs elsewhere.
func setNoNewPrivs() error { return errors.New("no_new_privs is supported on Linux only") }
