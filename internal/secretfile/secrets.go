// Package secretfile reads and writes geas's private files: owner-only,
// created atomically, and locked while a tool that rotates a credential holds
// them. It also names the state directory they live in.
package secretfile

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/timing"
)

// StateDirectory is $XDG_STATE_HOME/geas, where geas keeps the files only its
// own user may read: the GitHub login, the glab lock and the reaper's log.
func StateDirectory() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		return filepath.Join(state, "geas"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "geas"), nil
}

// Read returns a secret file's contents, once it is owned by the caller, a
// regular file and chmod 600: a symlink, another user's file or a
// world-readable one is refused.
func Read(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("secret file must be owned by you, regular and chmod 600")
	}
	// Read from the validated descriptor, not from the filename again.
	raw, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return nil, errors.New("cannot read secret file")
	}
	if len(raw) > 1024*1024 {
		return nil, errors.New("secret file is too large")
	}
	return raw, nil
}

// Write replaces a file atomically: a reader sees the old contents or the new
// ones, never half of a rotated credential.
func Write(path string, raw []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".auth-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	// Persist the rename as well as the token contents.
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

// WriteJSON writes value to path as Write does.
func WriteJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return Write(path, raw)
}

// WithFileLock runs operation while holding path's lock, which is a file next
// to it: a credential that rotates on every write, such as glab's OAuth
// refresh token, cannot be rotated twice at once. The wait ends with ctx.
func WithFileLock(ctx context.Context, path string, operation func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path+".lock")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe lock file")
	}
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		if err = timing.SleepContext(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return operation()
}
