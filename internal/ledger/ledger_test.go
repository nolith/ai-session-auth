package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newLedger(t *testing.T) (*Ledger, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	id, err := NewID(time.Date(2026, 10, 22, 21, 34, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Create(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, filepath.Join(state, "geas", "sessions", id)
}

func TestSessionID(t *testing.T) {
	id, err := NewID(time.Date(2026, 10, 22, 23, 34, 5, 0, time.FixedZone("CEST", 2*3600)))
	if err != nil || !sessionID.MatchString(id) || id[:17] != "20261022T213405Z-" {
		t.Fatalf("%q %v", id, err)
	}
	other, _ := NewID(time.Date(2026, 10, 22, 21, 34, 5, 0, time.UTC))
	if other == id {
		t.Fatal("two sessions in one second share an ID")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, bad := range []string{"", "..", "../20261022T213405Z-0011223344556677", "20261022T213405Z-00112233", "20261022T213405Z-0011223344556677/x"} {
		if _, err := Create(bad); err == nil {
			t.Errorf("created a ledger for %q", bad)
		}
		if _, err := Open(bad); err == nil {
			t.Errorf("opened a ledger for %q", bad)
		}
	}
}

func TestCreateIsOwnerOnlyAndExclusive(t *testing.T) {
	l, dir := newLedger(t)
	for path, want := range map[string]os.FileMode{filepath.Dir(dir): 0700, dir: 0700, filepath.Join(dir, "ledger"): 0600} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v", path, info.Mode(), err)
		}
	}
	if _, err := Create(l.ID()); err == nil {
		t.Fatal("a second session took the same ID")
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
	reaper, err := Open(l.ID())
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
	if _, err := Open(l.ID()); err == nil {
		t.Fatal("opened a world-readable ledger")
	}
}

func TestRemove(t *testing.T) {
	l, dir := newLedger(t)
	if err := l.Record(Entry{Provider: "gitlab:gitlab.com", Name: "geas-a", Revoked: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("session directory left: %v", err)
	}
}
