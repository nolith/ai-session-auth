// Package provider holds what every credential source shares: the Provider
// interface the session drives, and the credential it hands back. A provider
// kind is one package below this one; this package imports none of them.
package provider

import (
	"context"
	"time"
)

// A Provider is one credential source for one host, e.g. GitLab on
// gitlab.com. The session acquires its credential before the harness starts
// and revokes what it created when the session ends.
type Provider interface {
	// ID is unique within a session, e.g. "gitlab:gitlab.com".
	ID() string
	// Kind groups providers of one kind: "gitlab", "github".
	Kind() string
	// Acquire returns the credential.
	Acquire(ctx context.Context) (Credential, error)
	// Refresh returns a replacement before Credential.Expires, or the same
	// credential when it never expires within the session.
	Refresh(ctx context.Context, c Credential) (Credential, error)
	// Revoke undoes every credential this provider created.
	Revoke(ctx context.Context) error
}

// Credential is what a provider hands the session: the secret a tool sends,
// and when it stops working.
type Credential struct {
	Secret  string
	Expires time.Time
}

// CreatedError is an Acquire failure that came after the credential was
// created: the provider has something to revoke, so the session does not skip
// it as it skips a provider that failed before creating anything.
type CreatedError struct{ Err error }

func (e CreatedError) Error() string { return e.Err.Error() }
func (e CreatedError) Unwrap() error { return e.Err }
