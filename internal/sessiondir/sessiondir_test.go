package sessiondir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newDir(t *testing.T) *Dir {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id, err := NewID(time.Date(2026, 10, 22, 21, 34, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	d, err := Create(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
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
			t.Errorf("created a session directory for %q", bad)
		}
		if _, err := Open(bad); err == nil {
			t.Errorf("opened a session directory for %q", bad)
		}
	}
}

func TestCreateIsOwnerOnlyAndExclusive(t *testing.T) {
	d := newDir(t)
	state := os.Getenv("XDG_STATE_HOME")
	if d.Path() != filepath.Join(state, "geas", "sessions", d.ID()) {
		t.Fatalf("path %s", d.Path())
	}
	for _, path := range []string{filepath.Dir(d.Path()), d.Path()} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Errorf("%s: %v %v", path, info.Mode(), err)
		}
	}
	if _, err := Create(d.ID()); err == nil {
		t.Fatal("a second session took the same ID")
	}
	if opened, err := Open(d.ID()); err != nil || opened.Path() != d.Path() {
		t.Fatalf("open: %v %v", opened, err)
	}
	if err := os.Chmod(d.Path(), 0750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(d.ID()); err == nil {
		t.Fatal("opened a session directory others can enter")
	}
}

func TestFromEnv(t *testing.T) {
	d := newDir(t)
	t.Setenv("GEAS_SESSION_DIR", d.Path())
	if got, err := FromEnv(); err != nil || got.Path() != d.Path() || got.ID() != d.ID() {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "relative/" + d.ID(), filepath.Dir(d.Path()), filepath.Join(filepath.Dir(d.Path()), "20261022T213405Z-0011223344556677")} {
		t.Setenv("GEAS_SESSION_DIR", bad)
		if _, err := FromEnv(); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
}

// WriteFile replaces a file whole, owner-only, in directories it creates
// owner-only, and refuses a path outside the session.
func TestWriteFile(t *testing.T) {
	d := newDir(t)
	for _, content := range []string{"first", "second"} {
		if err := d.WriteFile("glab/config.yml", []byte(content)); err != nil {
			t.Fatal(err)
		}
		if raw, err := d.ReadFile("glab/config.yml"); err != nil || string(raw) != content {
			t.Fatalf("%q %v", raw, err)
		}
	}
	for path, want := range map[string]os.FileMode{"glab": 0700, "glab/config.yml": 0600} {
		info, err := os.Lstat(filepath.Join(d.Path(), path))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v %v", path, info.Mode(), err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(d.Path(), "glab"))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "..", "a/../../x", "./x", "a//b"} {
		if err := d.WriteFile(bad, nil); err == nil {
			t.Errorf("wrote %q", bad)
		}
	}
}

func TestMarkers(t *testing.T) {
	d := newDir(t)
	if _, err := d.Marker("gitlab:gitlab.com"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending provider: %v", err)
	}
	now := time.Date(2026, 10, 22, 21, 34, 5, 0, time.UTC)
	if err := d.SetMarker("gitlab:gitlab.com", Marker{State: Failed, Reason: "glab issuer request failed", Time: now}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetMarker("github:github.com", Marker{State: Ready, Time: now}); err != nil {
		t.Fatal(err)
	}
	if m, err := d.Marker("gitlab:gitlab.com"); err != nil || m.State != Failed || m.Reason != "glab issuer request failed" || !m.Time.Equal(now) {
		t.Fatalf("%+v %v", m, err)
	}
	if m, err := d.Marker("github:github.com"); err != nil || m.State != Ready {
		t.Fatalf("%+v %v", m, err)
	}
	raw, _ := os.ReadFile(filepath.Join(d.Path(), "markers", "github:github.com"))
	if string(raw) != `{"state":"ready","time":"2026-10-22T21:34:05Z"}` {
		t.Fatalf("marker file %s", raw)
	}
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "Gitlab"} {
		if err := d.SetMarker(bad, Marker{State: Ready}); err == nil {
			t.Errorf("marker for %q", bad)
		}
	}
	if err := d.SetMarker("ssh", Marker{State: "waiting"}); err == nil {
		t.Error("marker with an unknown state")
	}
	if err := d.SetMarker("ssh", Marker{State: Pending, Time: now}); err != nil {
		t.Fatal(err)
	}
	if m, err := d.Marker("ssh"); err != nil || m.State != Pending {
		t.Fatalf("%+v %v", m, err)
	}
	// All of them, without a write in progress.
	os.WriteFile(filepath.Join(d.Path(), "markers", ".auth-123"), []byte("{"), 0600)
	markers, err := d.Markers()
	if err != nil || len(markers) != 3 || markers["gitlab:gitlab.com"].State != Failed || markers["github:github.com"].State != Ready || markers["ssh"].State != Pending {
		t.Fatalf("%+v %v", markers, err)
	}
}

func TestRemove(t *testing.T) {
	d := newDir(t)
	if err := d.WriteFile("secrets/gitlab:gitlab.com", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.Path()); !os.IsNotExist(err) {
		t.Fatalf("session directory left: %v", err)
	}
}

// Remote reads the forms a session's Git asks the helper about, and
// refuses those it never would.
func TestRemote(t *testing.T) {
	for _, tc := range []struct {
		url, host, path string
	}{
		{"https://gitlab.com/group/project.git", "gitlab.com", "group/project.git"},
		{"https://oauth2@gitlab.example.com/group/sub/project", "gitlab.example.com", "group/sub/project"},
		{"https://gitlab.com:8443/group/project", "gitlab.com:8443", "group/project"},
		{"git@gitlab.com:group/project.git", "gitlab.com", "group/project.git"},
		{"ssh://git@github.com/nolith/geas.git", "github.com", "nolith/geas.git"},
		{"https://gitlab.com/group/%2e%2e/evil", "gitlab.com", "group/%2e%2e/evil"},
	} {
		host, path, err := Remote(tc.url)
		if err != nil || host != tc.host || path != tc.path {
			t.Errorf("%s: %q %q %v", tc.url, host, path, err)
		}
	}
	for _, url := range []string{
		"", "gitlab.com/group/project", "http://gitlab.com/group/project", "ftp://gitlab.com/x",
		"ssh://alice@gitlab.com/group/project", "ssh://gitlab.com/group/project", "ssh://git@gitlab.com:2222/group/project",
		"alice@gitlab.com:group/project", "git@gitlab.com", "git@:group/project", "git@GitLab.com:group/project",
		"https://gitlab.com/", "https://gitlab.com", "https://gitlab.com/group/project?x=1", "https://gitlab.com/group/project#x",
		"https://gitlab.com/group project", "file:///tmp/repo", "/tmp/repo",
	} {
		if host, path, err := Remote(url); err == nil {
			t.Errorf("%q: %q %q", url, host, path)
		}
	}
}
