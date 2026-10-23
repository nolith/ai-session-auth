package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nolith/geas/internal/sessiondir"
)

// Check is `geas sandbox check`: from inside a session, it tries to read
// every path the session's policy denies, and the session directory, as the
// harness would. It names each denied path it could read, and reports false
// when one was readable or the session directory was not. Outside any
// sandbox, it fails: the user's own logins are readable there.
//
// A session with a wrapper runs it in the background after the harness
// starts, through the wrapper, and records the verdict in a marker for geas
// status, from this output, which never reaches the terminal.
func Check(dir *sessiondir.Dir, output io.Writer) (bool, error) {
	policy, err := Load(dir)
	if err != nil {
		// The policy is in the session directory: either fails the check,
		// and the summary says which, since a policy the session wrote
		// wrongly is a different bug from a sandbox that hides the session.
		if readable(dir.Path()) {
			fmt.Fprintln(output, "sandbox check: cannot load the sandbox policy, "+PolicyFile+": "+loadError(err))
		} else {
			fmt.Fprintln(output, "sandbox check: the session directory is not readable")
		}
		return false, nil
	}
	if len(policy.Deny) == 0 {
		return false, errors.New("the session's sandbox policy denies nothing")
	}
	readableCount, absent := 0, 0
	for _, path := range policy.Deny {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			absent++
			continue
		}
		if readable(path) {
			readableCount++
			fmt.Fprintln(output, "readable: "+path)
		}
	}
	session := readable(dir.Path())
	verdict := "the session directory is readable"
	if !session {
		verdict = "the session directory is NOT readable"
	}
	fmt.Fprintf(output, "sandbox check: %d of %d protected paths readable (%d absent); %s\n", readableCount, len(policy.Deny), absent, verdict)
	return readableCount == 0 && session, nil
}

// loadError is why the policy did not load, without its path, which the
// summary names already.
func loadError(err error) string {
	var path *os.PathError
	if errors.As(err, &path) {
		return path.Err.Error()
	}
	return err.Error()
}

// readable reports whether path's contents can be read: a file's first byte,
// or a directory's first entry.
func readable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	if info.IsDir() {
		_, err = file.ReadDir(1)
	} else {
		_, err = file.Read(make([]byte, 1))
	}
	return err == nil || errors.Is(err, io.EOF)
}
