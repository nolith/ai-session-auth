// Package nono writes a session's sandbox policy as a nono profile fragment.
// The fragment has no base profile: the launcher that runs nono names its
// own with extends, since only it knows what the harness itself needs.
package nono

import (
	"encoding/json"

	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/sessiondir"
)

// ProfileFile is the fragment in the session directory, GEAS_SANDBOX_PROFILE.
const ProfileFile = "sandbox/nono.json"

type profile struct {
	Filesystem filesystem `json:"filesystem"`
}

// filesystem has nono's keys: bypass_protection re-opens a path inside a
// denied directory, which read alone does not.
type filesystem struct {
	Deny             []string `json:"deny"`
	Read             []string `json:"read"`
	BypassProtection []string `json:"bypass_protection"`
}

// Write writes p as the fragment, and returns its path.
func Write(dir *sessiondir.Dir, p sandbox.Policy) (string, error) {
	raw, err := json.MarshalIndent(profile{filesystem{Deny: list(p.Deny), Read: list(p.Read), BypassProtection: list(p.Reopen)}}, "", "  ")
	if err != nil {
		return "", err
	}
	if err = dir.WriteFile(ProfileFile, append(raw, '\n')); err != nil {
		return "", err
	}
	return dir.Join(ProfileFile)
}

// list writes nil as an empty list: nono reads null as a missing key.
func list(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
