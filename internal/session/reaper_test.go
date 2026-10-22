package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

// reaperSession writes a glab-issuer configuration and a ledger holding entry,
// with the fake glab on PATH, and returns the configuration path, the ledger
// and the fake glab's directory.
func reaperSession(t *testing.T, entry ledger.Entry) (string, *ledger.Ledger, string) {
	t.Helper()
	glab := testutil.FakeGlab(t)
	t.Setenv("PATH", glab+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := config.Config{SessionHours: 1, GitHub: config.GitHubConfig{Enabled: testutil.Bool(false)}, GitLab: testutil.GlabConfig()}
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := sessiondir.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := sessiondir.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	book, err := ledger.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { book.Close() })
	if err = book.Record(entry); err != nil {
		t.Fatal(err)
	}
	return path, book, glab
}

// The launcher revoked everything: the reaper calls nothing, and leaves the
// ledger to the launcher.
func TestReaperTrustsTheLauncher(t *testing.T) {
	path, book, glab := reaperSession(t, ledger.Entry{Provider: "gitlab:gitlab.com", Name: "geas-x", ID: 42})
	if err := watch(context.Background(), strings.NewReader(reaperDone), path, book.ID()); err != nil {
		t.Fatal(err)
	}
	if calls := testutil.Calls(t, glab); len(calls) != 0 {
		t.Fatalf("glab calls: %q", calls)
	}
	if open, err := book.Unrevoked(); err != nil || len(open) != 1 {
		t.Fatalf("ledger: %+v %v", open, err)
	}
}

// A session with nothing to revoke, such as GitHub's alone, still leaves its
// directory, and the credentials in it, to the reaper.
func TestReaperRemovesASessionWithNothingToRevoke(t *testing.T) {
	path, book, glab := reaperSession(t, ledger.Entry{Provider: "gitlab:gitlab.com", Name: "geas-x", ID: 42, Revoked: true})
	dir, err := sessiondir.Open(book.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err = dir.WriteFile("secrets/github:github.com", []byte("ghu_secret")); err != nil {
		t.Fatal(err)
	}
	if err = watch(context.Background(), strings.NewReader(""), path, book.ID()); err != nil {
		t.Fatal(err)
	}
	if calls := testutil.Calls(t, glab); len(calls) != 0 {
		t.Fatalf("glab calls: %q", calls)
	}
	if _, err := os.Stat(dir.Path()); !os.IsNotExist(err) {
		t.Fatalf("session directory left: %v", err)
	}
}

// A name no PAT has, every attempt long, was never minted: the reaper closes
// its entry after the last attempt and removes the session's directory.
func TestReaperClosesANameNeverMinted(t *testing.T) {
	pauses := reaperPauses
	reaperPauses = []time.Duration{0, 0, 0}
	t.Cleanup(func() { reaperPauses = pauses })
	path, book, glab := reaperSession(t, ledger.Entry{Provider: "gitlab:gitlab.com", Name: "geas-never"})
	if err := watch(context.Background(), strings.NewReader(""), path, book.ID()); err != nil {
		t.Fatal(err)
	}
	calls := testutil.Calls(t, glab)
	if len(calls) != 3 {
		t.Fatalf("glab calls: %q", calls)
	}
	for _, call := range calls {
		if !strings.Contains(call[len(call)-1], `personalAccessTokens(search: "geas-never")`) {
			t.Errorf("not a search for the name: %q", call)
		}
	}
	state, _ := os.LookupEnv("XDG_STATE_HOME")
	if dirs, err := os.ReadDir(filepath.Join(state, "geas", "sessions")); err != nil || len(dirs) != 0 {
		t.Fatalf("session directories: %v %v", dirs, err)
	}
	log, err := os.ReadFile(reaperLogPath())
	if err != nil || strings.Count(string(log), "geas-never: revocation failed: no credential has this name\n") != 2 || !strings.Contains(string(log), "geas-never: launcher gone; no PAT has this name, so none was created\n") {
		t.Fatalf("reaper log: %q %v", log, err)
	}
}

// An entry naming a provider the configuration no longer has is refused at
// once and stays in the ledger.
func TestReaperRefusesAnUnknownProvider(t *testing.T) {
	path, book, glab := reaperSession(t, ledger.Entry{Provider: "gitlab:ops.gitlab.net", Name: "geas-x", ID: 42})
	if err := watch(context.Background(), strings.NewReader(""), path, book.ID()); err == nil {
		t.Fatal("no error for an entry nobody can revoke")
	}
	if calls := testutil.Calls(t, glab); len(calls) != 0 {
		t.Fatalf("glab calls: %q", calls)
	}
	if open, err := book.Unrevoked(); err != nil || len(open) != 1 {
		t.Fatalf("ledger: %+v %v", open, err)
	}
	// Kept as evidence, and for whoever can revoke it.
	if _, err := sessiondir.Open(book.ID()); err != nil {
		t.Fatalf("session directory: %v", err)
	}
}
