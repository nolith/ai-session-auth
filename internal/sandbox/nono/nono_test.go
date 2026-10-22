package nono

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/testutil"
)

// The fragment has exactly deny, read and bypass_protection, and no
// extends: the launcher names the base profile.
func TestWrite(t *testing.T) {
	dir := testutil.SessionDir(t)
	session := "/home/alice/.local/state/geas/sessions/20261022T213405Z-1f0c2a9b3e4d5c6a"
	path, err := Write(dir, sandbox.Policy{
		Deny:   []string{"/home/alice/Library/Application Support/glab-cli", "/home/alice/.config/gh", "/home/alice/.local/state/geas"},
		Read:   []string{session},
		Reopen: []string{session},
	})
	if err != nil || path != filepath.Join(dir.Path(), "sandbox", "nono.json") {
		t.Fatalf("%q %v", path, err)
	}
	raw, err := dir.ReadFile(ProfileFile)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Golden(t, filepath.Join("testdata", "nono.json"), raw)
	var shape map[string]map[string]json.RawMessage
	if err = json.Unmarshal(raw, &shape); err != nil || len(shape) != 1 {
		t.Fatalf("top level: %v %v", shape, err)
	}
	var keys []string
	for key := range shape["filesystem"] {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"bypass_protection", "deny", "read"}) {
		t.Fatalf("filesystem keys: %v", keys)
	}
	// An empty policy still has the three lists, never null.
	if _, err = Write(dir, sandbox.Policy{}); err != nil {
		t.Fatal(err)
	}
	if raw, _ = dir.ReadFile(ProfileFile); string(raw) != "{\n  \"filesystem\": {\n    \"deny\": [],\n    \"read\": [],\n    \"bypass_protection\": []\n  }\n}\n" {
		t.Fatalf("empty: %s", raw)
	}
}
