package github

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/testutil"
)

func TestRenderer(t *testing.T) {
	dir := testutil.SessionDir(t)
	creds := map[string]provider.Credential{"github:github.com": {Secret: "ghu_session", User: "Alice", Expires: time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)}}
	if err := (Renderer{}).Render(dir, creds); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hosts.yml", "config.yml"} {
		raw, err := dir.ReadFile("gh/" + name)
		if err != nil {
			t.Fatal(err)
		}
		testutil.Golden(t, filepath.Join("testdata", name), raw)
	}
	for _, bad := range []map[string]provider.Credential{
		{},
		{"github:ghe.example.com": {Secret: "x", User: "alice"}},
		{"github:github.com": {Secret: "x"}},
	} {
		if err := (Renderer{}).Render(dir, bad); err == nil {
			t.Errorf("rendered %v", bad)
		}
	}
}

func TestSetup(t *testing.T) {
	c := testutil.Config().GitHub
	c.Owners = []string{"nolith"}
	s := New(c, nil).Setup()
	if len(s.Env) != 2 || s.Env["GH_CONFIG_DIR"] != "$GEAS_SESSION_DIR/gh" || s.Env["GH_PROMPT_DISABLED"] != "1" {
		t.Fatalf("env %v", s.Env)
	}
	if len(s.Git) != 1 {
		t.Fatalf("git %+v", s.Git)
	}
	g := s.Git[0]
	if g.Host != "github.com" || g.Username != "x-access-token" || !g.FoldCase || len(g.Namespaces) != 1 || g.Namespaces[0] != "nolith" || len(g.Repositories) != 1 || g.Repositories[0] != "alice/repo" {
		t.Fatalf("git host %+v", g)
	}
}

// gh's directory follows gh's own resolution: each override, then
// ~/.config/gh on macOS as on Linux.
func TestGhConfigDir(t *testing.T) {
	home := func() (string, error) { return "/home/alice", nil }
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"GH_CONFIG_DIR": "/elsewhere/gh", "XDG_CONFIG_HOME": "/xdg"}, "/elsewhere/gh"},
		{map[string]string{"XDG_CONFIG_HOME": "/xdg"}, "/xdg/gh"},
		{nil, "/home/alice/.config/gh"},
	} {
		got, err := ghConfigDir(func(name string) string { return tc.env[name] }, home)
		if err != nil || got != tc.want {
			t.Errorf("%v: %q %v", tc.env, got, err)
		}
	}
}

func TestProtect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GH_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	c := testutil.Config().GitHub
	c.StateFile = "/state/geas/github.json"
	got := New(c, nil).Setup().Protect
	if len(got) != 3 || got[0] != "/state/geas/github.json" || got[1] != "/state/geas/github.json.lock" || got[2] != filepath.Join(home, ".config", "gh") {
		t.Fatalf("%q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := New(c, nil).Setup().Protect; got[2] != "/xdg/gh" {
		t.Fatalf("%q", got)
	}
}
