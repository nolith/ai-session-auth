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

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

// Refused before reading stdin or touching any file.
func TestSaveGitLabIssuerRefusedWithGlabIssuer(t *testing.T) {
	c := testutil.Config()
	c.GitLab = []config.GitLabConfig{testutil.GlabConfig()}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	code, err := execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--stdin"}, nil)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "glab") {
		t.Fatalf("save-gitlab-issuer: %d %v", code, err)
	}
	code, err = execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--host", "gitlab.com", "--stdin"}, nil)
	if code != 1 || err == nil || err.Error() != `the issuer of gitlab.com is "glab": your glab login, with no PAT to save` {
		t.Fatalf("save-gitlab-issuer --host: %d %v", code, err)
	}
	code, err = execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--host", "ops.gitlab.net"}, nil)
	if code != 1 || err == nil || err.Error() != "no gitlab provider for ops.gitlab.net in the configuration" {
		t.Fatalf("save-gitlab-issuer for another host: %d %v", code, err)
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

// geas config check lists a configuration's providers, and names the first
// version 1 field of a configuration in the earlier format; geas run refuses
// that one with exit code 125. Without --config, the configuration is under
// XDG_CONFIG_HOME.
func TestConfigCheck(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	path := filepath.Join(xdg, "geas", "config.json")
	os.MkdirAll(filepath.Dir(path), 0700)
	raw, _ := json.Marshal(testutil.Config())
	os.WriteFile(path, raw, 0600)
	stdout := os.Stdout
	captured, _ := os.CreateTemp(t.TempDir(), "stdout")
	os.Stdout = captured
	code, err := execute(context.Background(), []string{"config", "check"}, nil)
	os.Stdout = stdout
	out, _ := os.ReadFile(captured.Name())
	want := path + ": version 2, session_hours 24\n  gitlab:gitlab.com  issuer bearer, as alice\n  github:github.com  as alice\n"
	if code != 0 || err != nil || string(out) != want {
		t.Fatalf("%d %v:\n%s", code, err, out)
	}
	if code, err := execute(context.Background(), []string{"config", "show"}, nil); code != 2 || err == nil {
		t.Fatalf("config show: %d %v", code, err)
	}
	v1 := filepath.Join(t.TempDir(), "v1.json")
	os.WriteFile(v1, []byte(`{"session_hours": 24, "gitlab": {"issuer": "glab"}, "github": {"enabled": false}}`), 0600)
	code, err = execute(context.Background(), []string{"--config", v1, "config", "check"}, nil)
	if code != 1 || err == nil || !strings.HasPrefix(err.Error(), `"gitlab" is a version 1 field`) {
		t.Fatalf("version 1: %d %v", code, err)
	}
	code, err = execute(context.Background(), []string{"--config", v1, "run", "--", "true"}, nil)
	if code != 125 || err == nil || !strings.HasPrefix(err.Error(), `"gitlab" is a version 1 field`) {
		t.Fatalf("run with version 1: %d %v", code, err)
	}
}

// captureStdout returns what f writes to os.Stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	stdout := os.Stdout
	captured, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = captured
	defer func() { os.Stdout = stdout }()
	f()
	raw, _ := os.ReadFile(captured.Name())
	return string(raw)
}

// geas allowed answers from the configuration alone, outside any session:
// exit 0 and the provider's ID when a session's Git would get a credential
// for the remote, 1 otherwise. No issuer is prepared: the bearer issuer's
// token file does not exist.
func TestAllowed(t *testing.T) {
	t.Setenv("GEAS_SESSION_DIR", "")
	second := testutil.GlabConfig()
	second.Host, second.Namespaces = "gitlab.example.com", []string{"ops"}
	c := testutil.Config()
	c.GitLab = []config.GitLabConfig{testutil.NamespacedConfig(), second}
	c.GitHub[0].Owners = []string{"nolith"}
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, raw, 0600)
	for _, tc := range []struct {
		url     string
		code    int
		want    string
		invalid bool // not a remote a session's Git asks the helper about
	}{
		{"https://gitlab.com/group/project.git", 0, "gitlab:gitlab.com\n", false},
		{"git@gitlab.com:other/sub/deeper/project.git", 0, "gitlab:gitlab.com\n", false},
		{"ssh://git@gitlab.com/alice/dotfiles.git", 0, "gitlab:gitlab.com\n", false},
		{"https://gitlab.com/elsewhere/project", 0, "gitlab:gitlab.com\n", false},
		{"git@gitlab.example.com:ops/runbooks.git", 0, "gitlab:gitlab.example.com\n", false},
		{"https://github.com/Nolith/geas", 0, "github:github.com\n", false},
		{"git@github.com:alice/repo.git", 0, "github:github.com\n", false},
		{"https://gitlab.com/other/project", 1, "", false},
		{"https://gitlab.com/elsewhere/other", 1, "", false},
		{"git@gitlab.example.com:group/project.git", 1, "", false},
		{"https://gitlab.com/group/../evil/project", 1, "", false},
		{"https://ops.gitlab.net/group/project", 1, "", false},
		{"http://gitlab.com/group/project", 1, "", true},
		{"ssh://git@gitlab.com:2222/group/project", 1, "", true},
	} {
		var code int
		var err error
		out := captureStdout(t, func() { code, err = execute(context.Background(), []string{"--config", path, "allowed", tc.url}, nil) })
		if code != tc.code || out != tc.want || (err != nil) != tc.invalid {
			t.Errorf("%s: exit %d, %q, %v", tc.url, code, out, err)
		}
	}
	if code, err := execute(context.Background(), []string{"--config", path, "allowed"}, nil); code != 2 || err == nil {
		t.Fatalf("no URL: %d %v", code, err)
	}
}
