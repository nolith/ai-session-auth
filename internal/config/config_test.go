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
func namespacedConfig() GitLabConfig {
	return GitLabConfig{ExpectedUser: "alice", IssuerTokenFile: "issuer.txt", Groups: []string{"group", "other/sub"}, PersonalProjects: true, Projects: []string{"elsewhere/project"}, Permissions: []string{"read_project", "push_code"}}
}
func testConfig() Config {
	return Config{SessionHours: 24, GitHub: GitHubConfig{ClientID: "app", ExpectedUser: "alice", StateFile: "state.json", Repositories: []string{"alice/repo"}}, GitLab: GitLabConfig{ExpectedUser: "alice", IssuerTokenFile: "issuer.txt", Repositories: []string{"group/project"}, ProjectIDs: []int64{42}, Scopes: []Scope{{Access: "USER", Permissions: []string{"read_user"}}, {Access: "SELECTED_MEMBERSHIPS", ResourceIDs: []string{"gid://gitlab/Project/42"}, Permissions: []string{"read_project", "push_code"}}}}}
}
func glabConfig() GitLabConfig {
	c := testConfig().GitLab
	c.Issuer, c.IssuerTokenFile = "glab", ""
	return c
}

func TestConfigRejectsBroaderScopeAndMissingPaths(t *testing.T) {
	c := testConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.GitLab.Scopes[1].ResourceIDs[0] = "gid://gitlab/Project/99"
	if c.validate() == nil {
		t.Fatal("accepted another project")
	}
	c = testConfig()
	c.GitLab.Scopes[1].Permissions = []string{"create_personal_access_token"}
	if c.validate() == nil {
		t.Fatal("accepted issuer permissions for agent")
	}
	c = testConfig()
	c.GitHub.StateFile = ""
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, raw, 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("missing path expanded into directory")
	}
	c = testConfig()
	raw, _ = json.Marshal(c)
	os.WriteFile(path, append(raw, []byte(" {}")...), 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	// Optional skips only credentials that fail at start, never the checks.
	c = testConfig()
	c.GitHub.Optional, c.GitLab.Optional = true, true
	raw, _ = json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	if _, err := Load(path); err != nil || !strings.Contains(string(raw), `"optional":true`) {
		t.Fatalf("optional providers: %v", err)
	}
	c.GitHub.Repositories = nil
	if c.validate() == nil {
		t.Fatal("optional GitHub accepted without an allowlist")
	}
	c = testConfig()
	c.GitLab.Optional, c.GitLab.ExpectedUser = true, ""
	if c.validate() == nil {
		t.Fatal("optional GitLab accepted without expected_user")
	}
}
func TestGitHubOwnersConfig(t *testing.T) {
	c := testConfig()
	c.GitHub.Repositories, c.GitHub.Owners = nil, []string{"nolith", "some-org"}
	if err := c.validate(); err != nil {
		t.Fatalf("owners alone: %v", err)
	}
	c.GitHub.Repositories = []string{"alice/repo"}
	if err := c.validate(); err != nil {
		t.Fatalf("owners and repositories: %v", err)
	}
	c.GitHub.Repositories, c.GitHub.Owners = nil, nil
	if err := c.validate(); err == nil || err.Error() != "configure GitHub repositories or owners" {
		t.Fatalf("no allowlist: %v", err)
	}
	for _, owner := range []string{"", "nolith/repo", "nolith/", "/nolith", "*", "noli*", "nolith:x", ".", "..", "no lith\n", "nolith.git", "nolith%2f"} {
		c.GitHub.Owners = []string{owner}
		if c.validate() == nil {
			t.Errorf("accepted owner %q", owner)
		}
	}
	c.GitHub.Owners = []string{"nolith", "Nolith"}
	if c.validate() == nil {
		t.Fatal("accepted the same owner twice in another case")
	}
	// Loaded from JSON, as written in a configuration.
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"github":{"client_id":"app","expected_user":"alice","state_file":"state.json","owners":["nolith"]},"gitlab":{"enabled":false}}`)
	os.WriteFile(path, raw, 0600)
	if loaded, err := Load(path); err != nil || !slices.Equal(loaded.GitHub.Owners, []string{"nolith"}) {
		t.Fatalf("%+v %v", loaded.GitHub, err)
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

// TestGlabIssuerConfig checks the issuer that has no file at all: "glab" and
// issuer_token_file are mutually exclusive, and nothing is expanded for it.
func TestGlabIssuerConfig(t *testing.T) {
	c := testConfig()
	c.GitLab = glabConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	loaded, err := Load(path)
	if err != nil || loaded.GitLab.IssuerTokenFile != "" {
		t.Fatalf("%q %v", loaded.GitLab.IssuerTokenFile, err)
	}
	for _, mutate := range []func(*GitLabConfig){
		func(g *GitLabConfig) { g.IssuerTokenFile = "issuer.txt" },
		func(g *GitLabConfig) { g.Issuer = "file" },
		func(g *GitLabConfig) { g.Issuer = "" },
	} {
		c = testConfig()
		c.GitLab = glabConfig()
		mutate(&c.GitLab)
		if c.validate() == nil {
			t.Fatalf("accepted issuer %q with issuer_token_file %q", c.GitLab.Issuer, c.GitLab.IssuerTokenFile)
		}
	}
}

func TestGitLabNamespacedConfig(t *testing.T) {
	c := testConfig()
	c.GitLab = namespacedConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*GitLabConfig){
		"mixed with repositories":     func(g *GitLabConfig) { g.Repositories = []string{"group/project"} },
		"mixed with project_ids":      func(g *GitLabConfig) { g.ProjectIDs = []int64{42} },
		"mixed with granular_scopes":  func(g *GitLabConfig) { g.Scopes = testConfig().GitLab.Scopes },
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
		c.GitLab = namespacedConfig()
		mutate(&c.GitLab)
		if c.validate() == nil {
			t.Errorf("accepted %s", name)
		}
	}
	// The earlier format can never ask for every membership or the instance.
	for _, access := range []string{"ALL_MEMBERSHIPS", "INSTANCE", "PERSONAL_PROJECTS"} {
		c := testConfig()
		c.GitLab.Scopes = append(c.GitLab.Scopes, Scope{Access: access, Permissions: []string{"read_project"}})
		if c.validate() == nil {
			t.Errorf("accepted %s", access)
		}
	}
	// The shipped example must load as it is.
	if _, err := Load(filepath.Join("..", "..", "config.example.json")); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
}
