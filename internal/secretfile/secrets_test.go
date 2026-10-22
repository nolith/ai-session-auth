package secretfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateReadAndAtomicRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	raw, err := Read(path)
	if err != nil || string(raw) != "second" {
		t.Fatalf("%s %v", raw, err)
	}
	link := path + "link"
	os.Symlink(path, link)
	if _, err = Read(link); err == nil {
		t.Fatal("followed symlink")
	}
	os.Chmod(path, 0644)
	if _, err = Read(path); err == nil {
		t.Fatal("read world-readable secret")
	}
}
func TestFileLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithFileLock(context.Background(), path, func() error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := WithFileLock(ctx, path, func() error { t.Error("entered held lock"); return nil })
	close(release)
	if other := <-done; other != nil {
		t.Fatal(other)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
