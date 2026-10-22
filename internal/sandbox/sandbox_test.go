package sandbox

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/testutil"
)

// protects is a provider that only protects paths.
type protects []string

func (p protects) ID() string                                 { return "fake" }
func (p protects) Kind() string                               { return "fake" }
func (p protects) Setup() provider.Setup                      { return provider.Setup{Protect: p} }
func (p protects) Revoke(context.Context, ledger.Entry) error { return nil }
func (p protects) Acquire(context.Context, *ledger.Ledger) (provider.Credential, error) {
	return provider.Credential{}, nil
}
func (p protects) Refresh(_ context.Context, c provider.Credential) (provider.Credential, error) {
	return c, nil
}

// TestMerge pins policy.json for a gitlab and a github provider's paths.
func TestMerge(t *testing.T) {
	session := "/home/alice/.local/state/geas/sessions/20261022T213405Z-1f0c2a9b3e4d5c6a"
	p := Merge([]provider.Provider{
		protects{"/home/alice/Library/Application Support/glab-cli", "/home/alice/.config/geas/issuer.txt"},
		protects{"/home/alice/.local/state/geas/github.json", "/home/alice/.local/state/geas/github.json.lock", "/home/alice/.config/gh/", "relative/ignored", "/home/alice/.config/gh"},
	}, "/home/alice/.local/state/geas", session)
	want := Policy{
		Deny: []string{
			"/home/alice/Library/Application Support/glab-cli",
			"/home/alice/.config/geas/issuer.txt",
			"/home/alice/.local/state/geas/github.json",
			"/home/alice/.local/state/geas/github.json.lock",
			"/home/alice/.config/gh",
			"/home/alice/.local/state/geas",
		},
		Read:   []string{session},
		Reopen: []string{session},
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("%+v", p)
	}
	dir := testutil.SessionDir(t)
	if err := p.Write(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := dir.ReadFile(PolicyFile)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "policy.json"), raw)
	if loaded, err := Load(dir); err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("load: %+v %v", loaded, err)
	}
}
