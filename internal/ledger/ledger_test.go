package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/sessiondir"
)

func newLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	id, err := sessiondir.NewID(time.Date(2026, 10, 22, 21, 34, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := sessiondir.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, filepath.Join(state, "geas", "sessions", id)
}

func TestCreateIsOwnerOnlyAndExclusive(t *testing.T) {
	l, dir := newLedger(t)
	for path, want := range map[string]os.FileMode{filepath.Dir(dir): 0700, dir: 0700, filepath.Join(dir, "ledger"): 0600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v", path, info.Mode(), err)
		}
	}
	if _, err := Create(l.dir); err == nil {
		t.Fatal("a second ledger in the same session")
	}
}

// An entry is updated by appending it again; the last line wins, and the
// entries keep the order they were first written in.
func TestLaterLinesWin(t *testing.T) {
	l, dir := newLedger(t)
	created := time.Date(2026, 10, 22, 21, 34, 6, 0, time.UTC)
	first := Entry{Provider: "gitlab:gitlab.com", Name: "geas-a", Created: created}
	second := Entry{Provider: "gitlab:ops.gitlab.net", Name: "geas-a", Created: created}
	for _, e := range []Entry{first, second, {Provider: first.Provider, Name: first.Name, ID: 42, Created: created}} {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Record(Entry{Provider: second.Provider, Name: second.Name, Created: created, Revoked: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := l.Entries()
	if err != nil || len(entries) != 2 || entries[0].ID != 42 || entries[0].Revoked || entries[1].Provider != second.Provider || !entries[1].Revoked {
		t.Fatalf("%+v %v", entries, err)
	}
	open, err := l.Unrevoked()
	if err != nil || len(open) != 1 || open[0].Name != "geas-a" || open[0].Provider != first.Provider {
		t.Fatalf("unrevoked: %+v %v", open, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ledger"))
	want := `{"provider":"gitlab:gitlab.com","name":"geas-a","created":"2026-10-22T21:34:06Z"}` + "\n"
	if len(raw) < len(want) || string(raw[:len(want)]) != want {
		t.Fatalf("first line:\n%s", raw)
	}
	if err = l.Record(Entry{Provider: "gitlab:gitlab.com"}); err == nil {
		t.Fatal("recorded an entry without a name")
	}
}

// A write cut short leaves a line without its newline: Entries skips it, and
// the next process to open the ledger ends it before appending.
func TestTornLineIsSkipped(t *testing.T) {
	l, dir := newLedger(t)
	if err := l.Record(Entry{Provider: "gitlab:gitlab.com", Name: "geas-a"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	file, err := os.OpenFile(filepath.Join(dir, "ledger"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"provider":"gitlab:gitlab.com","na`)
	file.Close()
	reaper, err := Open(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reaper.Close()
	if err = reaper.Record(Entry{Provider: "gitlab:gitlab.com", Name: "geas-a", ID: 42, Revoked: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := reaper.Entries()
	if err != nil || len(entries) != 1 || entries[0].ID != 42 || !entries[0].Revoked {
		t.Fatalf("%+v %v", entries, err)
	}
}

func TestOpenRefusesAnUnsafeLedger(t *testing.T) {
	l, dir := newLedger(t)
	if err := os.Chmod(filepath.Join(dir, "ledger"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(l.dir); err == nil {
		t.Fatal("opened a world-readable ledger")
	}
}

// revokerFunc revokes as its function says.
type revokerFunc func(context.Context, Entry) error

func (f revokerFunc) Revoke(ctx context.Context, e Entry) error { return f(ctx, e) }

// Providers revoke in parallel, each its own entries in order; an entry is
// closed once revoked or gone, and stays open otherwise.
func TestRevokeAcrossProviders(t *testing.T) {
	l, _ := newLedger(t)
	entries := []Entry{
		{Provider: "a", Name: "a1", ID: 1},
		{Provider: "b", Name: "b1"},
		{Provider: "a", Name: "a2", ID: 2},
		{Provider: "a", Name: "a3", ID: 3},
		{Provider: "gone", Name: "c1"},
	}
	for _, e := range entries {
		if err := l.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	// a waits for b to start: revoking one provider after the other would
	// never get past a1.
	started := make(chan struct{})
	var order []string
	failed := errors.New("issuer down")
	providers := map[string]Revoker{
		"a": revokerFunc(func(ctx context.Context, e Entry) error {
			if e.Name == "a1" {
				select {
				case <-started:
				case <-time.After(10 * time.Second):
					return errors.New("not in parallel")
				}
			}
			order = append(order, e.Name)
			switch e.Name {
			case "a2":
				return ErrGone
			case "a3":
				return failed
			}
			return nil
		}),
		"b": revokerFunc(func(ctx context.Context, e Entry) error {
			close(started)
			return ErrNotFound
		}),
	}
	results := l.Revoke(context.Background(), entries, providers)
	want := []error{nil, ErrNotFound, ErrGone, failed, ErrNoProvider}
	for i, r := range results {
		if r.Entry.Name != entries[i].Name || !errors.Is(r.Err, want[i]) || (r.Err == nil) != (want[i] == nil) {
			t.Errorf("result %d: %+v, want %v", i, r, want[i])
		}
	}
	if strings.Join(order, " ") != "a1 a2 a3" {
		t.Errorf("provider a revoked %v", order)
	}
	open, err := l.Unrevoked()
	var names []string
	for _, e := range open {
		names = append(names, e.Name)
	}
	if err != nil || strings.Join(names, " ") != "b1 a3 c1" {
		t.Fatalf("unrevoked: %v %v", names, err)
	}
}
