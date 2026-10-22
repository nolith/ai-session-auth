package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/nolith/geas/internal/testutil"
)

func TestCheck(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every file")
	}
	dir := testutil.SessionDir(t)
	files := t.TempDir()
	open := filepath.Join(files, "open.yml")
	closed := filepath.Join(files, "closed.yml")
	empty := filepath.Join(files, "empty")
	closedDir := filepath.Join(files, "closed-dir")
	os.WriteFile(open, []byte("token"), 0600)
	os.WriteFile(closed, []byte("token"), 0000)
	os.WriteFile(empty, nil, 0600)
	os.Mkdir(closedDir, 0000)
	t.Cleanup(func() { os.Chmod(closedDir, 0700) })
	policy := Policy{Deny: []string{open, closed, empty, closedDir, filepath.Join(files, "absent"), files}, Read: []string{dir.Path()}, Reopen: []string{dir.Path()}}
	if err := policy.Write(dir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ok, err := Check(dir, &out)
	want := "readable: " + open + "\nreadable: " + empty + "\nreadable: " + files + "\nsandbox check: 3 of 6 protected paths readable (1 absent); the session directory is readable\n"
	if err != nil || ok || out.String() != want {
		t.Fatalf("%v %v:\n%s", ok, err, out.String())
	}
	// Everything denied, the session directory readable: the check passes.
	policy.Deny = []string{closed, closedDir, filepath.Join(files, "absent")}
	policy.Write(dir)
	out.Reset()
	if ok, err = Check(dir, &out); err != nil || !ok || out.String() != "sandbox check: 0 of 3 protected paths readable (1 absent); the session directory is readable\n" {
		t.Fatalf("%v %v:\n%s", ok, err, out.String())
	}
	// An unreadable session directory fails it.
	if err = os.Chmod(dir.Path(), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir.Path(), 0700) })
	out.Reset()
	if ok, err = Check(dir, &out); err != nil || ok || out.String() != "sandbox check: the session directory is not readable\n" {
		t.Fatalf("%v %v:\n%s", ok, err, out.String())
	}
}
