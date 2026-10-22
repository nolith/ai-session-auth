package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

// Refused before reading stdin or touching any file.
func TestSaveGitLabIssuerRefusedWithGlabIssuer(t *testing.T) {
	c := testutil.Config()
	c.GitLab = testutil.GlabConfig()
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	code, err := execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--stdin"}, nil)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "glab") {
		t.Fatalf("save-gitlab-issuer: %d %v", code, err)
	}
}

// Outside any sandbox the user's logins are readable, so the check, run
// inside a session, fails and names them; outside a session it refuses.
func TestSandboxCheck(t *testing.T) {
	dir := testutil.SessionDir(t)
	login := filepath.Join(t.TempDir(), "glab-cli")
	os.Mkdir(login, 0700)
	os.WriteFile(filepath.Join(login, "config.yml"), []byte("token: x"), 0600)
	policy := sandbox.Policy{Deny: []string{login}, Read: []string{dir.Path()}, Reopen: []string{dir.Path()}}
	if err := policy.Write(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEAS_SESSION_DIR", dir.Path())
	var out bytes.Buffer
	if code, err := sandboxCheck(&out); code != 1 || err != nil || !strings.HasPrefix(out.String(), "readable: "+login+"\n") {
		t.Fatalf("%d %v:\n%s", code, err, out.String())
	}
	os.Chmod(login, 0000)
	t.Cleanup(func() { os.Chmod(login, 0700) })
	out.Reset()
	if code, err := sandboxCheck(&out); os.Getuid() != 0 && (code != 0 || err != nil) {
		t.Fatalf("%d %v:\n%s", code, err, out.String())
	}
	t.Setenv("GEAS_SESSION_DIR", "")
	if code, err := execute(context.Background(), []string{"sandbox", "check"}, nil); code != 1 || err == nil {
		t.Fatalf("outside a session: %d %v", code, err)
	}
	if code, err := execute(context.Background(), []string{"sandbox"}, nil); code != 2 || err == nil {
		t.Fatalf("no subcommand: %d %v", code, err)
	}
}

// geas status lists every provider's marker, and its exit code says whether
// all are ready, one failed, or one is still pending.
func TestStatus(t *testing.T) {
	dir := testutil.SessionDir(t)
	t.Setenv("GEAS_SESSION_DIR", dir.Path())
	now := time.Now()
	set := func(id, state, reason string) {
		t.Helper()
		if err := dir.SetMarker(id, sessiondir.Marker{State: state, Reason: reason, Time: now}); err != nil {
			t.Fatal(err)
		}
	}
	set("gitlab:gitlab.com", sessiondir.Pending, "")
	set("github:github.com", sessiondir.Pending, "")
	for _, tc := range []struct {
		id, state, reason string
		code              int
		want              string
	}{
		{"", "", "", 75, "github:github.com  pending\ngitlab:gitlab.com  pending\n"},
		{"gitlab:gitlab.com", sessiondir.Ready, "", 75, "github:github.com  pending\ngitlab:gitlab.com  ready\n"},
		{"github:github.com", sessiondir.Ready, "", 0, "github:github.com  ready\ngitlab:gitlab.com  ready\n"},
		{"github:github.com", sessiondir.Failed, "run github-login before starting a session", 1, "github:github.com  failed: run github-login before starting a session\ngitlab:gitlab.com  ready\n"},
		{"gitlab:gitlab.com", sessiondir.Pending, "", 1, "github:github.com  failed: run github-login before starting a session\ngitlab:gitlab.com  pending\n"},
	} {
		if tc.id != "" {
			set(tc.id, tc.state, tc.reason)
		}
		var out bytes.Buffer
		if code, err := status(&out); code != tc.code || err != nil || out.String() != tc.want {
			t.Errorf("after %s %s: exit %d %v:\n%s", tc.id, tc.state, code, err, out.String())
		}
	}
	if code, err := execute(context.Background(), []string{"status", "extra"}, nil); code != 2 || err == nil {
		t.Fatalf("with an argument: %d %v", code, err)
	}
	t.Setenv("GEAS_SESSION_DIR", "")
	if code, err := execute(context.Background(), []string{"status"}, nil); code != 1 || err == nil || !strings.Contains(err.Error(), "not inside a geas session") {
		t.Fatalf("outside a session: %d %v", code, err)
	}
}
