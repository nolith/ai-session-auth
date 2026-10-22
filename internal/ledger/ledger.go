// Package ledger records every revocable credential a session creates, on
// disk, before the call that creates it: a session that dies at any point
// leaves enough behind for the reaper to revoke what it created.
//
// The ledger is the file ledger in the session's directory, an append-only
// file of JSON lines, one Entry each. An entry is never rewritten:
// a change appends the whole entry again, and the last line for a provider and
// a name wins. Appending keeps every line that was ever synced, whatever
// happens during a later write, and lets the launcher and the reaper, two
// processes, add lines without a lock or a rename between them.
package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/sessiondir"
)

// Entry is written before the provider creates the credential, so a crash
// between the create and the bookkeeping leaves a name the reaper can find.
type Entry struct {
	Provider string    `json:"provider"` // Provider.ID()
	Name     string    `json:"name"`     // e.g. geas-<hex>, chosen before the mint
	ID       int64     `json:"id,omitempty"`
	Created  time.Time `json:"created"`
	Revoked  bool      `json:"revoked,omitempty"`
}

// A Ledger is one session's ledger, open for appending.
type Ledger struct {
	dir  *sessiondir.Dir
	mu   sync.Mutex
	file *os.File
}

// Create writes an empty ledger, owner-only, into a new session's directory,
// and syncs it, so that the first entry lands in a file that survives a
// crash.
func Create(dir *sessiondir.Dir) (*Ledger, error) {
	path, err := dir.Join("ledger")
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syncDirectory(dir.Path()); err != nil {
		file.Close()
		return nil, err
	}
	return &Ledger{dir: dir, file: file}, nil
}

// Open opens an existing session's ledger, as the reaper does once the
// launcher is gone.
func Open(dir *sessiondir.Dir) (*Ledger, error) {
	path, err := dir.Join("ledger")
	if err != nil {
		return nil, err
	}
	// Validates the owner and the mode as for any secret file.
	raw, err := dir.ReadFile("ledger")
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	// A write cut short leaves a line without its newline; end it, so that the
	// next entry starts a line of its own.
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		if _, err = file.Write([]byte("\n")); err != nil {
			file.Close()
			return nil, err
		}
	}
	return &Ledger{dir: dir, file: file}, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ID is the session's ID.
func (l *Ledger) ID() string { return l.dir.ID() }

// Record appends e and syncs it to disk before it returns: a provider calls it
// before the request that creates the credential, and again to complete or
// close the entry.
func (l *Ledger) Record(e Entry) error {
	if e.Provider == "" || e.Name == "" {
		return errors.New("ledger entry without a provider or a name")
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// One write per line: with O_APPEND, a line from another process never
	// lands inside this one.
	if _, err = l.file.Write(append(line, '\n')); err != nil {
		return err
	}
	return l.file.Sync()
}

// Entries returns every entry, as its last line has it, in the order the
// entries were first written. A line that does not parse is skipped: only a
// write cut short leaves one, and Record had not returned for it, so the
// credential it was about to guard was never requested.
func (l *Ledger) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := l.dir.ReadFile("ledger")
	if err != nil {
		return nil, err
	}
	type key struct{ provider, name string }
	index := map[key]int{}
	var entries []Entry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var e Entry
		if json.Unmarshal(line, &e) != nil || e.Provider == "" || e.Name == "" {
			continue
		}
		k := key{e.Provider, e.Name}
		if i, ok := index[k]; ok {
			entries[i] = e
			continue
		}
		index[k] = len(entries)
		entries = append(entries, e)
	}
	return entries, nil
}

// Unrevoked returns the entries still to revoke.
func (l *Ledger) Unrevoked() ([]Entry, error) {
	entries, err := l.Entries()
	if err != nil {
		return nil, err
	}
	var open []Entry
	for _, e := range entries {
		if !e.Revoked {
			open = append(open, e)
		}
	}
	return open, nil
}

// Close closes the ledger and leaves it on disk: the session's directory,
// and the ledger in it, go only once nothing is left to revoke.
func (l *Ledger) Close() error { return l.file.Close() }
