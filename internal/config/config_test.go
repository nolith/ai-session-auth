package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fixtures other packages share live in internal/testutil; a test of this
// package cannot import it without a cycle, so it keeps its own.
func gitlabConfig() GitLabConfig {
	return GitLabConfig{Host: "gitlab.com", Issuer: Issuer{Type: "bearer", TokenFile: "issuer.txt"}, ExpectedUser: "alice", Projects: []string{"group/project"}, Permissions: []string{"read_project", "push_code"}}
}
func namespacedConfig() GitLabConfig {
	c := gitlabConfig()
	c.Groups, c.PersonalProjects, c.Projects = []string{"group", "other/sub"}, true, []string{"elsewhere/project"}
	return c
}
func testConfig() Config {
	return Config{SessionHours: 24, GitHub: []GitHubConfig{{Host: "github.com", ClientID: "app", ExpectedUser: "alice", StateFile: "state.json", Repositories: []string{"alice/repo"}}}, GitLab: []GitLabConfig{gitlabConfig()}}
}

// write writes raw as a configuration file and returns its path.
func write(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// load loads c as written by MarshalJSON.
func load(t *testing.T, c Config) (Config, error) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return Load(write(t, string(raw)))
}

func TestLoadVersion2(t *testing.T) {
	path := write(t, `{
  "version": 2,
  "session_hours": 12,
  "providers": [
    {"type": "gitlab", "host": "gitlab.com", "issuer": {"type": "bearer", "token_file": "issuer.txt"}, "expected_user": "alice", "groups": ["group"], "permissions": ["read_project"]},
    {"type": "github", "host": "github.com", "client_id": "app", "expected_user": "alice", "state_file": "~/state/github.json", "owners": ["alice"]}
  ]
}`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.SessionHours != 12 || len(c.GitLab) != 1 || len(c.GitHub) != 1 || c.Source() != path {
		t.Fatalf("%+v", c)
	}
	if c.GitLab[0].Issuer.TokenFile != filepath.Join(filepath.Dir(path), "issuer.txt") || c.GitHub[0].StateFile != filepath.Join(home, "state", "github.json") {
		t.Fatalf("paths not expanded: %q %q", c.GitLab[0].Issuer.TokenFile, c.GitHub[0].StateFile)
	}
	// session_hours defaults to 24, and what MarshalJSON writes loads back.
	c.SessionHours = 0
	again, err := load(t, c)
	if err != nil || again.SessionHours != 24 || !slices.Equal(again.GitLab[0].Groups, []string{"group"}) || !slices.Equal(again.GitHub[0].Owners, []string{"alice"}) {
		t.Fatalf("%+v %v", again, err)
	}
	// The shipped example must load as it is.
	if _, err := Load(filepath.Join("..", "..", "config.example.json")); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
}

// A version 1 file is refused, naming its first version 1 field in the
// file's order; so is a version 1 field left in a version 2 provider.
func TestVersion1Refused(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{`{"session_hours": 24, "github": {"enabled": false}, "gitlab": {"issuer": "glab"}}`, `"github" is a version 1 field: each provider is an entry of "providers" now`},
		{`{"gitlab": {"issuer": "glab"}, "github": {}}`, `"gitlab" is a version 1 field: each provider is an entry of "providers" now, with "type": "gitlab" and a "host"`},
		{`{"version": 2, "providers": [{"type": "github", "host": "github.com", "optional": true}]}`, `providers[0]: "optional" is a version 1 field: a provider that fails never stops the session`},
		{`{"version": 2, "providers": [{"type": "github", "host": "github.com"}, {"type": "gitlab", "enabled": true, "optional": true}]}`, `providers[1]: "enabled" is a version 1 field: leave the provider out instead`},
		{`{"version": 2, "providers": [{"type": "gitlab", "host": "gitlab.com", "issuer": "glab"}]}`, `providers[0]: "issuer" is a version 1 field: write "issuer": {"type": "glab"}`},
		{`{"version": 2, "providers": [{"type": "gitlab", "issuer_token_file": "x"}]}`, `providers[0]: "issuer_token_file" is a version 1 field: write "issuer": {"type": "bearer", "token_file": ...}`},
		{`{"version": 2, "providers": [{"type": "gitlab", "project_ids": [42]}]}`, `providers[0]: "project_ids" is a version 1 field`},
		{`{"session_hours": 24}`, `the configuration has no "version": write "version": 2`},
	} {
		_, err := Load(write(t, tc.raw))
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) || !strings.HasSuffix(err.Error(), `see "Migrating from version 1" in the README`) {
			t.Errorf("%s: %v", tc.raw, err)
		}
	}
}

func TestVersion2Refusals(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{`{"version": 3, "providers": []}`, "configuration version 3 is not supported: geas reads version 2"},
		{`{"version": 2, "providers": []}`, "configure at least one provider"},
		{`{"version": 2, "providers": [{"type": "ssh"}]}`, `providers[0]: provider type "ssh" is not supported yet`},
		{`{"version": 2, "providers": [{"type": "bitbucket"}]}`, `providers[0]: unknown provider type "bitbucket"`},
		{`{"version": 2, "providers": [{"host": "gitlab.com"}]}`, `providers[0] needs a "type": gitlab or github`},
		{`{"version": 2, "providers": ["gitlab"]}`, `providers[0] must be an object`},
		{`{"version": 2, "providers": [{"type": "github", "host": "github.com", "groups": ["x"]}]}`, `providers[0]: unknown field "groups"`},
		{`{"version": 2, "providers": [], "unknown": true}`, `unknown field "unknown"`},
		{`{"version": 2, "providers": []} {}`, "configuration must contain one JSON object"},
		{`[]`, "invalid configuration JSON"},
	} {
		if _, err := Load(write(t, tc.raw)); err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", tc.raw, err, tc.want)
		}
	}
}

func TestConfigRejectsMissingFields(t *testing.T) {
	c := testConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.GitHub[0].StateFile = ""
	if _, err := load(t, c); err == nil {
		t.Fatal("missing path expanded into directory")
	}
	for name, mutate := range map[string]func(*Config){
		"no session_hours":     func(c *Config) { c.SessionHours = 0 },
		"too long":             func(c *Config) { c.SessionHours = 49 },
		"no providers":         func(c *Config) { c.GitLab, c.GitHub = nil, nil },
		"GitHub elsewhere":     func(c *Config) { c.GitHub[0].Host = "github.example.com" },
		"GitHub without host":  func(c *Config) { c.GitHub[0].Host = "" },
		"two GitHubs":          func(c *Config) { c.GitHub = append(c.GitHub, c.GitHub[0]) },
		"two gitlab.com":       func(c *Config) { c.GitLab = append(c.GitLab, c.GitLab[0]) },
		"GitLab without host":  func(c *Config) { c.GitLab[0].Host = "" },
		"GitLab without user":  func(c *Config) { c.GitLab[0].ExpectedUser = "" },
		"GitHub without users": func(c *Config) { c.GitHub[0].ExpectedUser = "" },
	} {
		c := testConfig()
		mutate(&c)
		if c.validate() == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestGitHubOwnersConfig(t *testing.T) {
	c := testConfig()
	g := &c.GitHub[0]
	g.Repositories, g.Owners = nil, []string{"nolith", "some-org"}
	if err := c.validate(); err != nil {
		t.Fatalf("owners alone: %v", err)
	}
	g.Repositories = []string{"alice/repo"}
	if err := c.validate(); err != nil {
		t.Fatalf("owners and repositories: %v", err)
	}
	g.Repositories, g.Owners = nil, nil
	if err := c.validate(); err == nil || err.Error() != "configure GitHub repositories or owners" {
		t.Fatalf("no allowlist: %v", err)
	}
	for _, owner := range []string{"", "nolith/repo", "nolith/", "/nolith", "*", "noli*", "nolith:x", ".", "..", "no lith\n", "nolith.git", "nolith%2f"} {
		g.Owners = []string{owner}
		if c.validate() == nil {
			t.Errorf("accepted owner %q", owner)
		}
	}
	g.Owners = []string{"nolith", "Nolith"}
	if c.validate() == nil {
		t.Fatal("accepted the same owner twice in another case")
	}
	// Loaded from JSON, as written in a configuration.
	raw := `{"version": 2, "providers": [{"type": "github", "host": "github.com", "client_id": "app", "expected_user": "alice", "state_file": "state.json", "owners": ["nolith"]}]}`
	if loaded, err := Load(write(t, raw)); err != nil || len(loaded.GitLab) != 0 || !slices.Equal(loaded.GitHub[0].Owners, []string{"nolith"}) {
		t.Fatalf("%+v %v", loaded, err)
	}
}

func TestExpiryCoversSessionAtUTCMidnight(t *testing.T) {
	for _, tc := range []struct {
		now   string
		hours time.Duration
		want  string
	}{
		{"2026-10-08T10:00:00Z", 24, "2026-10-10"},
		{"2026-10-08T00:00:00Z", 24, "2026-10-09"},
		{"2026-10-08T23:30:00-02:00", 48, "2026-10-12"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := ExpiryForDuration(now, tc.hours*time.Hour); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.now, got, tc.want)
		}
	}
}

// TestIssuerConfig checks both issuers: glab, which has no file at all and
// nothing to expand, and bearer, which needs its token_file.
func TestIssuerConfig(t *testing.T) {
	c := testConfig()
	c.GitLab[0].Issuer = Issuer{Type: "glab"}
	loaded, err := load(t, c)
	if err != nil || loaded.GitLab[0].Issuer.TokenFile != "" {
		t.Fatalf("%q %v", loaded.GitLab[0].Issuer.TokenFile, err)
	}
	for _, issuer := range []Issuer{
		{Type: "glab", TokenFile: "issuer.txt"},
		{Type: "bearer"},
		{Type: "file", TokenFile: "issuer.txt"},
		{TokenFile: "issuer.txt"},
	} {
		c = testConfig()
		c.GitLab[0].Issuer = issuer
		if c.validate() == nil {
			t.Errorf("accepted issuer %+v", issuer)
		}
	}
}

func TestGitLabNamespacedConfig(t *testing.T) {
	c := testConfig()
	c.GitLab[0] = namespacedConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*GitLabConfig){
		"no namespace":                func(g *GitLabConfig) { g.Groups, g.PersonalProjects, g.Projects = nil, false, nil },
		"no permissions":              func(g *GitLabConfig) { g.Permissions = nil },
		"PAT management":              func(g *GitLabConfig) { g.Permissions = append(g.Permissions, "revoke_personal_access_token") },
		"own namespace as a group":    func(g *GitLabConfig) { g.Groups = append(g.Groups, "alice") },
		"group with a trailing slash": func(g *GitLabConfig) { g.Groups = []string{"group/"} },
		"group with dot segments":     func(g *GitLabConfig) { g.Groups = []string{"group/../other"} },
		"duplicate group":             func(g *GitLabConfig) { g.Groups = []string{"group", "group"} },
		"project without namespace":   func(g *GitLabConfig) { g.Projects = []string{"project"} },
	} {
		c := testConfig()
		c.GitLab[0] = namespacedConfig()
		mutate(&c.GitLab[0])
		if c.validate() == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

// The default path follows XDG_CONFIG_HOME, else ~/.config, on macOS too.
func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, err := DefaultPath(); err != nil || got != "/xdg/geas/config.json" {
		t.Fatalf("%q %v", got, err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "relative"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if got, err := DefaultPath(); err != nil || got != filepath.Join(home, ".config", "geas", "config.json") {
			t.Fatalf("XDG_CONFIG_HOME %q: %q %v", xdg, got, err)
		}
	}
}
