package credhelper

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

// session records gitlab.com and github.com, with GitLab's credential ready
// and GitHub's failed.
func session(t *testing.T) *sessiondir.Dir {
	t.Helper()
	dir := testutil.SessionDir(t)
	hosts := []Host{
		{Provider: "gitlab:gitlab.com", Host: "gitlab.com", Username: "oauth2", Namespaces: []string{"group", "alice"}, Repositories: []string{"elsewhere/project"}},
		{Provider: "github:github.com", Host: "github.com", Username: "x-access-token", Namespaces: []string{"nolith"}, FoldCase: true},
	}
	if err := WriteHosts(dir, hosts); err != nil {
		t.Fatal(err)
	}
	if err := dir.WriteFile("secrets/gitlab:gitlab.com", []byte("glpat-session")); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetMarker("gitlab:gitlab.com", sessiondir.Marker{State: sessiondir.Ready, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := dir.SetMarker("github:github.com", sessiondir.Marker{State: sessiondir.Failed, Reason: "run github-login before starting a session", Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGet(t *testing.T) {
	dir := session(t)
	for _, tc := range []struct {
		request, want, err string
	}{
		{"protocol=https\nhost=gitlab.com\npath=group/project.git\n\n", "username=oauth2\npassword=glpat-session\n\n", ""},
		{"protocol=https\nhost=gitlab.com\npath=elsewhere/project.git\n", "username=oauth2\npassword=glpat-session\n\n", ""},
		// Outside the allowlist, another host or protocol: no answer.
		{"protocol=https\nhost=gitlab.com\npath=group-foo/project.git\n\n", "", ""},
		{"protocol=https\nhost=gitlab.com\n\n", "", ""},
		{"protocol=http\nhost=gitlab.com\npath=group/project.git\n\n", "", ""},
		{"protocol=https\nhost=gitlab.com.evil\npath=group/project.git\n\n", "", ""},
		{"protocol=https\nhost=ops.gitlab.net\npath=group/project.git\n\n", "", ""},
		// A failed provider stops Git, with the reason.
		{"protocol=https\nhost=github.com\npath=NOLITH/geas\n\n", "quit=1\n\n", "github.com is not available in this session: run github-login before starting a session"},
		{"protocol=https\nhost=github.com\npath=other/geas\n\n", "", ""},
	} {
		var out bytes.Buffer
		err := Get(dir, strings.NewReader(tc.request), &out)
		if out.String() != tc.want || fmt.Sprint(err) != map[bool]string{true: "<nil>", false: tc.err}[tc.err == ""] {
			t.Errorf("%q: %q %v", tc.request, out.String(), err)
		}
	}
	// Still pending: a provider without a marker never falls through.
	os.Remove(filepath.Join(dir.Path(), "markers", "gitlab:gitlab.com"))
	var out bytes.Buffer
	if err := Get(dir, strings.NewReader("protocol=https\nhost=gitlab.com\npath=group/project\n"), &out); err == nil || out.String() != "quit=1\n\n" || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("pending: %q %v", out.String(), err)
	}
}

func TestRunNeedsASession(t *testing.T) {
	t.Setenv("GEAS_SESSION_DIR", "")
	var out bytes.Buffer
	if err := Run("get", strings.NewReader("protocol=https\nhost=gitlab.com\npath=group/project\n"), &out); err == nil || out.Len() != 0 {
		t.Fatalf("answered outside a session: %q %v", out.String(), err)
	}
	// Store and erase have nothing to do, session or not.
	for _, operation := range []string{"store", "erase"} {
		if err := Run(operation, strings.NewReader("password=secret\n"), &out); err != nil || out.Len() != 0 {
			t.Fatalf("%s: %q %v", operation, out.String(), err)
		}
	}
}

// Git itself, with the session's gitconfig included last as the session
// does: SSH remotes become HTTPS, and this helper answers alone, although the
// user configured helpers of their own.
func TestGitUsesTheHelper(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	dir := session(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sessiondir.GitConfig("!'"+binary+"' credential", []string{"gitlab.com", "github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err = dir.WriteFile(sessiondir.GitConfigFile, raw); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(global, []byte("[credential]\n\thelper = \"!f() { echo password=from-global; }; f\"\n[core]\n\tsshCommand = ssh -i elsewhere\n"), 0600)
	env := append(os.Environ(), "GEAS_SESSION_DIR="+dir.Path(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+global, "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.https://gitlab.com.helper", "GIT_CONFIG_VALUE_0=!f() { echo password=from-env; }; f",
		"GIT_CONFIG_KEY_1=include.path", "GIT_CONFIG_VALUE_1="+filepath.Join(dir.Path(), sessiondir.GitConfigFile))
	run := func(input string, args ...string) (string, error) {
		command := exec.Command(git, args...)
		command.Env = env
		command.Dir = t.TempDir()
		command.Stdin = strings.NewReader(input)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		return string(output) + stderr.String(), err
	}
	if output, err := run("", "ls-remote", "--get-url", "git@gitlab.com:group/project.git"); err != nil || output != "https://gitlab.com/group/project.git\n" {
		t.Fatalf("rewrite: %q %v", output, err)
	}
	if output, err := run("", "ls-remote", "--get-url", "ssh://git@github.com/nolith/geas.git"); err != nil || output != "https://github.com/nolith/geas.git\n" {
		t.Fatalf("rewrite: %q %v", output, err)
	}
	if output, err := run("", "config", "core.sshCommand"); err != nil || output != sessiondir.SSHCommand+"\n" {
		t.Fatalf("sshCommand: %q %v", output, err)
	}
	output, err := run("protocol=https\nhost=gitlab.com\npath=group/project.git\n\n", "credential", "fill")
	if err != nil || !strings.Contains(output, "username=oauth2\npassword=glpat-session\n") {
		t.Fatalf("fill: %q %v", output, err)
	}
	// The failed provider: Git stops at once instead of asking anyone else.
	output, err = run("protocol=https\nhost=github.com\npath=nolith/geas.git\n\n", "credential", "fill")
	if err == nil || strings.Contains(output, "from-") || !strings.Contains(output, "github.com is not available in this session") {
		t.Fatalf("fill for a failed provider: %q %v", output, err)
	}
	// Outside the allowlist nobody answers, the user's helpers included.
	output, err = run("protocol=https\nhost=gitlab.com\npath=other/project.git\n\n", "credential", "fill")
	if err == nil || strings.Contains(output, "password=") {
		t.Fatalf("fill outside the allowlist: %q %v", output, err)
	}
}

func TestGitConfigGolden(t *testing.T) {
	raw, err := sessiondir.GitConfig(`!'/nix/store/x-geas/bin/geas' credential`, []string{"gitlab.com", "ops.gitlab.net", "github.com"})
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "gitconfig"), raw)
	for _, bad := range []string{"gitlab.com\n[core]", "", "gitlab.com/x", `gitlab.com"`} {
		if _, err := sessiondir.GitConfig("!geas credential", []string{bad}); err == nil {
			t.Errorf("rendered host %q", bad)
		}
	}
}

// TestMain lets Git run this binary as the session's credential helper.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "credential" {
		if err := Run(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "geas:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
