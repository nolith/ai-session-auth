package gitlab

import (
	"os"
	"path/filepath"
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
	// The earlier format lists exact repositories only.
	p = &Provider{Config: testutil.Config().GitLab}
	if g := p.Setup().Git[0]; len(g.Namespaces) != 0 || len(g.Repositories) != 1 || g.Repositories[0] != "group/project" {
		t.Fatalf("earlier format: %+v", g)
	}
}
