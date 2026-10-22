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
