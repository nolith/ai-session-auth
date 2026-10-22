package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/nolith/geas/internal/ledger"
)

// fake is a provider that only declares its Setup.
type fake struct {
	id  string
	env map[string]string
}

func (f fake) ID() string   { return f.id }
func (f fake) Kind() string { return strings.Split(f.id, ":")[0] }
func (f fake) Setup() Setup { return Setup{Env: f.env} }
func (f fake) Acquire(context.Context, *ledger.Ledger) (Credential, error) {
	return Credential{}, nil
}
func (f fake) Refresh(_ context.Context, c Credential) (Credential, error) { return c, nil }
func (f fake) Revoke(context.Context, ledger.Entry) error                  { return nil }

func reserved(name string) bool {
	return name == "GEAS_SESSION_DIR" || strings.HasPrefix(name, "GIT_CONFIG_")
}

func TestMergeEnv(t *testing.T) {
	glab := map[string]string{"GLAB_CONFIG_DIR": "$GEAS_SESSION_DIR/glab"}
	merged, err := MergeEnv([]Provider{
		fake{"gitlab:gitlab.com", glab},
		fake{"gitlab:ops.gitlab.net", glab},
		fake{"github:github.com", map[string]string{"GH_CONFIG_DIR": "$GEAS_SESSION_DIR/gh", "GH_PROMPT_DISABLED": "1"}},
	}, reserved)
	if err != nil || len(merged) != 3 || merged["GLAB_CONFIG_DIR"] != "$GEAS_SESSION_DIR/glab" || merged["GH_PROMPT_DISABLED"] != "1" {
		t.Fatalf("%v %v", merged, err)
	}
	for _, tc := range []struct {
		providers []Provider
		want      string
	}{
		{[]Provider{fake{"gitlab:gitlab.com", glab}, fake{"gitlab:ops.gitlab.net", map[string]string{"GLAB_CONFIG_DIR": "/elsewhere"}}}, "providers gitlab:gitlab.com and gitlab:ops.gitlab.net set GLAB_CONFIG_DIR to different values"},
		{[]Provider{fake{"ssh", map[string]string{"GEAS_SESSION_DIR": "/tmp"}}}, "provider ssh sets GEAS_SESSION_DIR, which the session owns"},
		{[]Provider{fake{"ssh", map[string]string{"GIT_CONFIG_COUNT": "0"}}}, "provider ssh sets GIT_CONFIG_COUNT, which the session owns"},
		{[]Provider{fake{"ssh", map[string]string{"A=B": "x"}}}, "provider ssh exports an invalid variable"},
		{[]Provider{fake{"ssh", map[string]string{"A": "x\x00y"}}}, "provider ssh exports an invalid variable"},
	} {
		if merged, err := MergeEnv(tc.providers, reserved); err == nil || err.Error() != tc.want {
			t.Errorf("%v %v, want %q", merged, err, tc.want)
		}
	}
}

// GitLab's allowlist: groups and the user's namespace by whole segments,
// projects exactly, case-sensitive.
func TestAllowsGitLab(t *testing.T) {
	h := GitHost{Host: "gitlab.com", Namespaces: []string{"group", "other/sub", "alice"}, Repositories: []string{"elsewhere/project"}}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"group/project.git", true},
		{"/group/project.git", true},
		{"group/sub/deeper/project", true},
		{"other/sub/project", true},
		{"alice/dotfiles.git", true},
		{"elsewhere/project", true},
		{"elsewhere/project.git", true},
		{"group-foo/project", false},
		{"groupx/project", false},
		{"other/subway/project", false},
		{"other/project", false},
		{"elsewhere/project-2", false},
		{"elsewhere/project/sub", false},
		{"alice-bot/project", false},
		{"GROUP/project", false},
		{"Elsewhere/Project", false},
		{"group", false},
		{"group/", false},
		{"", false},
		{"group/../evil/project", false},
		{"group/%2e%2e/evil", false},
		{"group/./project", false},
		{"group//project", false},
	} {
		if got := h.Allows(tc.path); got != tc.want {
			t.Errorf("%q: %v", tc.path, got)
		}
	}
}

// GitHub's allowlist: owners by whole segment and repositories exactly, in
// any letter case.
func TestAllowsGitHub(t *testing.T) {
	h := GitHost{Host: "github.com", Namespaces: []string{"nolith", "Some-Org"}, Repositories: []string{"alice/repo"}, FoldCase: true}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"nolith/anything.git", true},
		{"NOLITH/Anything", true},
		{"some-org/repo", true},
		{"alice/repo", true},
		{"/ALICE/repo.git", true},
		{"ALICE/REPO.git", true},
		{"nolith-foo/repo", false},
		{"nolithx/repo", false},
		{"no/lith/repo", false},
		{"nolith", false},
		{"alice/other", false},
		{"someone-else/x", false},
		{"nolith/../evil/x", false},
		{"nolith/%2e%2e/x", false},
		{"alice/%72epo", false},
	} {
		if got := h.Allows(tc.path); got != tc.want {
			t.Errorf("%q: %v", tc.path, got)
		}
	}
	// Owners alone, without repositories.
	h.Repositories = nil
	if h.Allows("alice/repo") || !h.Allows("nolith/repo") {
		t.Fatal("owners without repositories")
	}
}
