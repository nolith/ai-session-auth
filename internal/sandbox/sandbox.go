// Package sandbox is the policy a session asks of whatever sandbox runs the
// harness: the providers' protected paths and geas's own state denied, the
// session's directory readable. It knows no sandbox; a renderer below it
// writes the policy in one sandbox's terms. The session also writes the
// policy as it is, for `geas sandbox check`.
package sandbox

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sessiondir"
)

// PolicyFile is the policy in the session directory, provider- and
// sandbox-neutral.
const PolicyFile = "sandbox/policy.json"

// Policy is what the harness must not read, and what it must.
type Policy struct {
	// Deny lists files and directories the harness must not read.
	Deny []string `json:"deny"`
	// Read lists what it must read, such as the session directory.
	Read []string `json:"read"`
	// Reopen lists paths inside a denied directory that Read re-opens: the
	// session directory, inside the denied state directory.
	Reopen []string `json:"reopen"`
}

// Merge builds a session's policy: it denies what every provider protects
// and geas's state directory as a whole (the ledger, the locks, the other
// sessions), and reads and re-opens the session's own directory inside it.
func Merge(providers []provider.Provider, state, session string) Policy {
	var deny []string
	for _, p := range providers {
		deny = append(deny, p.Setup().Protect...)
	}
	deny = append(deny, state)
	return Policy{Deny: paths(deny), Read: paths([]string{session}), Reopen: paths([]string{session})}
}

// paths cleans absolute paths and drops duplicates and anything relative,
// in order.
func paths(values []string) []string {
	result := []string{}
	for _, value := range values {
		if !filepath.IsAbs(value) {
			continue
		}
		value = filepath.Clean(value)
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

// Write writes the policy into the session directory.
func (p Policy) Write(dir *sessiondir.Dir) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return dir.WriteFile(PolicyFile, append(raw, '\n'))
}

// Load reads the policy the session wrote.
func Load(dir *sessiondir.Dir) (Policy, error) {
	raw, err := dir.ReadFile(PolicyFile)
	if err != nil {
		return Policy{}, err
	}
	var p Policy
	if err = json.Unmarshal(raw, &p); err != nil {
		return Policy{}, errors.New("invalid sandbox policy")
	}
	return p, nil
}
