package gitlab

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/testutil"
)

// Two instances in one config.yml, gitlab.com the default host, whatever
// the order.
func TestRendererTwoHosts(t *testing.T) {
	dir := testutil.SessionDir(t)
	expires := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	creds := map[string]provider.Credential{
		"gitlab:ops.gitlab.net": {Secret: "glpat-ops-secret", User: "alice", Expires: expires},
		"gitlab:gitlab.com":     {Secret: "glpat-\"quoted\"\nsecret", User: "alice", Expires: expires},
	}
	if err := (Renderer{}).Render(dir, creds); err != nil {
		t.Fatal(err)
	}
	raw, err := dir.ReadFile("glab/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "config.yml"), raw)
	info, err := os.Stat(filepath.Join(dir.Path(), "glab"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("glab directory: %v %v", info.Mode(), err)
	}
	for _, bad := range []map[string]provider.Credential{
		{},
		{"github:github.com": {Secret: "x"}},
		{"gitlab:evil.com\nhosts:": {Secret: "x"}},
	} {
		if err := (Renderer{}).Render(dir, bad); err == nil {
			t.Errorf("rendered %v", bad)
		}
	}
}

func TestSetup(t *testing.T) {
	p := &Provider{Config: testutil.NamespacedConfig()}
	s := p.Setup()
	if len(s.Env) != 1 || s.Env["GLAB_CONFIG_DIR"] != "$GEAS_SESSION_DIR/glab" {
		t.Fatalf("env %v", s.Env)
	}
	if len(s.Git) != 1 {
		t.Fatalf("git %+v", s.Git)
	}
	g := s.Git[0]
	if g.Host != "gitlab.com" || g.Username != "oauth2" || g.FoldCase || len(g.Namespaces) != 3 || g.Namespaces[2] != "alice" || len(g.Repositories) != 1 || g.Repositories[0] != "elsewhere/project" {
		t.Fatalf("git host %+v", g)
	}
	// Projects alone are exact repositories.
	p = &Provider{Config: testutil.GitLab()}
	if g := p.Setup().Git[0]; len(g.Namespaces) != 0 || len(g.Repositories) != 1 || g.Repositories[0] != "group/project" {
		t.Fatalf("projects alone: %+v", g)
	}
}

// glab's directory follows glab's own resolution: each override, then the
// OS default, which differs between macOS and Linux.
func TestGlabConfigDir(t *testing.T) {
	darwin := func() (string, error) { return "/Users/alice/Library/Application Support", nil }
	linux := func() (string, error) { return "/home/alice/.config", nil }
	for _, tc := range []struct {
		env         map[string]string
		configDir   func() (string, error)
		want        string
		description string
	}{
		{map[string]string{"GLAB_CONFIG_DIR": "/elsewhere/glab", "XDG_CONFIG_HOME": "/xdg"}, darwin, "/elsewhere/glab", "GLAB_CONFIG_DIR"},
		{map[string]string{"XDG_CONFIG_HOME": "/xdg"}, darwin, "/xdg/glab-cli", "XDG_CONFIG_HOME on macOS"},
		{map[string]string{"XDG_CONFIG_HOME": "/xdg"}, linux, "/xdg/glab-cli", "XDG_CONFIG_HOME on Linux"},
		{nil, darwin, "/Users/alice/Library/Application Support/glab-cli", "macOS default"},
		{nil, linux, "/home/alice/.config/glab-cli", "Linux default"},
	} {
		got, err := glabConfigDir(func(name string) string { return tc.env[name] }, tc.configDir)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v", tc.description, got, err)
		}
	}
}

// Setup resolves from the environment geas runs in, before the session's
// own GLAB_CONFIG_DIR exists, and adds a stored issuer PAT.
func TestProtect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GLAB_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	want := filepath.Join(home, ".config", "glab-cli")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "glab-cli")
	}
	if got := (&Provider{Config: testutil.GlabConfig()}).Setup().Protect; len(got) != 1 || got[0] != want {
		t.Fatalf("glab issuer: %q, want %q", got, want)
	}
	t.Setenv("GLAB_CONFIG_DIR", "/elsewhere/glab")
	c := testutil.GitLab()
	c.Issuer.TokenFile = "/state/issuer.txt"

	if got := (&Provider{Config: c}).Setup().Protect; len(got) != 2 || got[0] != "/elsewhere/glab" || got[1] != "/state/issuer.txt" {
		t.Fatalf("bearer issuer: %q", got)
	}
}
