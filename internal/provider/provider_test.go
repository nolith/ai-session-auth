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
