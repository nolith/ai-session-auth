// Package provider holds what every credential source shares: the Provider
// interface the session drives, what a provider adds to the session, the
// credential it hands back, and the Renderer that turns credentials into a
// tool's configuration. A provider kind is one package below this one; this
// package imports none of them.
package provider

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/sessiondir"
)

// A Provider is one credential source for one host, e.g. GitLab on
// gitlab.com. The session acquires its credential before the harness starts
// and revokes what the ledger lists when the session ends.
type Provider interface {
	// ID is unique within a session, e.g. "gitlab:gitlab.com".
	ID() string
	// Kind groups providers for their kind's Renderer: "gitlab", "github".
	Kind() string
	// Setup declares what the provider adds to the session before the
	// harness starts. No network call; it must not block.
	Setup() Setup
	// Acquire returns the credential. Anything revocable is recorded in the
	// ledger before the call that creates it.
	Acquire(ctx context.Context, l *ledger.Ledger) (Credential, error)
	// Refresh returns a replacement before Credential.Expires, or the same
	// credential when it never expires within the session.
	Refresh(ctx context.Context, c Credential) (Credential, error)
	// Revoke undoes one ledger entry this provider created. The reaper calls
	// it from a fresh process, so it must not rely on in-memory state. It
	// returns ledger.ErrGone when nothing is left to revoke, and
	// ledger.ErrNotFound when no credential has the entry's name.
	Revoke(ctx context.Context, e ledger.Entry) error
}

// Setup is the part of a provider the session merges with the others.
// Everything specific to one forge or tool stays in the provider's package;
// only what several providers share, or the session itself acts on, is here.
type Setup struct {
	// Env is exported to the harness, e.g. GLAB_CONFIG_DIR and
	// GH_PROMPT_DISABLED. "$GEAS_SESSION_DIR" expands to the session's
	// directory. Two providers may set the same variable only to the same
	// value, and none may set one of the session's own; see MergeEnv.
	Env map[string]string
	// Git lists the hosts git reaches over HTTPS with this provider's token.
	Git []GitHost
	// Protect lists the user's own credential files and directories that
	// the agent must never read: the issuer login, refresh tokens. Resolved
	// the way the tool itself resolves them at run time (environment
	// overrides first, then the OS default), from the user's environment,
	// never hard-coded, so that the sandbox follows the tool.
	Protect []string
}

// GitHost is all the session gitconfig and the credential helper need to
// know about one host.
type GitHost struct {
	Host string
	// Username is sent with the token; each forge defines its own.
	Username string
	// Namespaces are allowlist prefixes, by whole path segments: "group"
	// covers "group/project" and "group/sub/project".
	Namespaces []string
	// Repositories are single repositories, matched exactly.
	Repositories []string
	// FoldCase compares paths in any letter case, as GitHub does.
	FoldCase bool
}

// Allows reports whether a repository path, as Git sends it, is in the
// allowlist: one of Repositories exactly, or inside one of Namespaces by
// whole leading segments, so "gitlab-org" never matches "gitlab-org-foo/x".
// A path that could name another repository (empty segments, "..", escapes)
// is refused.
func (h GitHost) Allows(path string) bool {
	path, ok := config.NormalizeGitPath(path)
	if !ok {
		return false
	}
	same := func(a, b string) bool {
		if h.FoldCase {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	if slices.ContainsFunc(h.Repositories, func(repo string) bool { return same(repo, path) }) {
		return true
	}
	return slices.ContainsFunc(h.Namespaces, func(namespace string) bool {
		prefix := namespace + "/"
		return len(path) > len(prefix) && same(path[:len(prefix)], prefix)
	})
}

// Credential is what a provider hands the session: the secret a tool sends,
// the account it acts as, when it stops working, and any files that go with
// it.
type Credential struct {
	Secret string
	// User is the account the secret authenticates, for tools that record
	// it next to the token, such as gh.
	User    string
	Expires time.Time
	// Files are written into the session directory, by path relative to it,
	// e.g. an ssh certificate.
	Files map[string][]byte
}

// A Renderer writes the tool configuration that all providers of one kind
// share, such as glab's config.yml with one entry per GitLab host. Each
// kind's package supplies one; the session calls it whenever a credential of
// that kind arrives or is refreshed, with every credential of the kind, by
// provider ID. It replaces its files whole, through dir's atomic writes.
type Renderer interface {
	Kind() string
	Render(dir *sessiondir.Dir, creds map[string]Credential) error
}

// envName is what an exported variable's name may look like.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MergeEnv merges the Setup.Env of every provider. Two providers may set one
// variable only to the same value (every GitLab instance sets
// GLAB_CONFIG_DIR alike); a different value, or a variable the session owns,
// as reserved reports, is a configuration error.
func MergeEnv(providers []Provider, reserved func(name string) bool) (map[string]string, error) {
	merged := map[string]string{}
	setBy := map[string]string{}
	for _, p := range providers {
		env := p.Setup().Env
		for _, name := range slices.Sorted(maps.Keys(env)) {
			value := env[name]
			if !envName.MatchString(name) || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("provider %s exports an invalid variable", p.ID())
			}
			if reserved(name) {
				return nil, fmt.Errorf("provider %s sets %s, which the session owns", p.ID(), name)
			}
			if other, ok := setBy[name]; ok && merged[name] != value {
				return nil, fmt.Errorf("providers %s and %s set %s to different values", other, p.ID(), name)
			}
			if _, ok := setBy[name]; !ok {
				setBy[name] = p.ID()
			}
			merged[name] = value
		}
	}
	return merged, nil
}

// CreatedError is an Acquire failure that came after the credential was
// created: the provider has something to revoke, so the session does not skip
// it as it skips a provider that failed before creating anything.
type CreatedError struct{ Err error }

func (e CreatedError) Error() string { return e.Err.Error() }
func (e CreatedError) Unwrap() error { return e.Err }
