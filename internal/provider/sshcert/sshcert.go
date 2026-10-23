// Package sshcert hands an agent the SSH certificate the human granted it
// with geas ssh grant: it copies the agent's key and certificate into the
// session directory, so that a sandbox need only grant that directory, and
// writes an ssh_config that uses them. It watches the agent's directory, so
// that a grant made while the session runs reaches the agent within seconds.
// A certificate expires on its own, within 12 hours: there is nothing to
// revoke.
package sshcert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/sshgrant"
	"github.com/nolith/geas/internal/timing"
)

// ID is the provider's ID and kind: a session has one at most.
const ID = "ssh"

// pollInterval is how often Watch looks at the agent's directory: a stat of
// three files, cheap enough to run every few seconds for a whole session.
const pollInterval = 3 * time.Second

// Provider is the agent's certificate, for one session.
type Provider struct {
	// Name is the agent's name; empty when New could not find it.
	Name string
	// agents is the agents' directory, $XDG_STATE_HOME/geas/agents.
	agents string
	poll   time.Duration
	now    func() time.Time
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Watcher  = (*Provider)(nil)
)

// New prepares the certificate of the agent harness runs: its name is the
// harness's tool, the base name of its first word, and its workspace, as
// geas ssh name finds them. When the name cannot be found, the provider is
// returned all the same, for its Setup, with the reason.
func New(ctx context.Context, harness []string) (*Provider, error) {
	p := &Provider{poll: pollInterval, now: time.Now}
	agents, err := sshgrant.AgentsDir()
	if err != nil {
		return p, err
	}
	p.agents = agents
	if len(harness) == 0 {
		return p, errors.New("no harness to name the agent after")
	}
	p.Name, err = sshgrant.Name(ctx, filepath.Base(harness[0]))
	return p, err
}

func (p *Provider) ID() string   { return ID }
func (p *Provider) Kind() string { return ID }

// Setup exports the agent's name as AGENT_SSH_NAME, and protects every
// agent's key: the agent reads its own copy in the session directory.
func (p *Provider) Setup() provider.Setup {
	s := provider.Setup{Env: map[string]string{}}
	if p.Name != "" {
		s.Env["AGENT_SSH_NAME"] = p.Name
	}
	if p.agents != "" {
		s.Protect = []string{p.agents}
	}
	return s
}

// Acquire copies the agent's key and certificate, once the certificate is
// valid and for that key.
func (p *Provider) Acquire(context.Context, *ledger.Ledger) (provider.Credential, error) {
	return p.load()
}

// Refresh returns c: the session watches the agent's directory instead.
func (p *Provider) Refresh(_ context.Context, c provider.Credential) (provider.Credential, error) {
	return c, nil
}

// Revoke has nothing to do: a certificate is never in the ledger.
func (p *Provider) Revoke(context.Context, ledger.Entry) error { return ledger.ErrGone }

// ask is what the agent should do about a missing or unusable certificate.
func (p *Provider) ask() string {
	return "ask the human: geas ssh grant " + p.Name + " <host>"
}

// load reads the agent's key and certificate, as a credential whose files
// go into the session directory.
func (p *Provider) load() (provider.Credential, error) {
	dir := filepath.Join(p.agents, p.Name)
	key, err := secretfile.Read(filepath.Join(dir, sshgrant.KeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return provider.Credential{}, errors.New(p.Name + " has no key yet: " + p.ask())
	}
	if err != nil {
		return provider.Credential{}, err
	}
	pub, err := secretfile.Read(filepath.Join(dir, sshgrant.PublicKeyFile))
	if err != nil {
		return provider.Credential{}, err
	}
	raw, err := secretfile.Read(filepath.Join(dir, sshgrant.CertificateFile))
	if errors.Is(err, os.ErrNotExist) {
		return provider.Credential{}, errors.New(p.Name + " has no certificate yet: " + p.ask())
	}
	if err != nil {
		return provider.Credential{}, err
	}
	cert, err := sshgrant.ParseCertificate(raw)
	if err != nil {
		return provider.Credential{}, fmt.Errorf("the certificate of %s is unusable, %v: %s", p.Name, err, p.ask())
	}
	if !cert.ForKey(pub) {
		return provider.Credential{}, errors.New("the certificate of " + p.Name + " is not for its key: " + p.ask())
	}
	if now := p.now(); !cert.ValidAt(now) {
		if expires := cert.Expires(); !expires.IsZero() && !now.Before(expires) {
			return provider.Credential{}, errors.New("the certificate of " + p.Name + " expired at " + expires.Format("2006-01-02 15:04 MST") + ": " + p.ask())
		}
		return provider.Credential{}, errors.New("the certificate of " + p.Name + " is not valid yet: " + p.ask())
	}
	return provider.Credential{
		Expires: cert.Expires(),
		Files:   map[string][]byte{sshgrant.SessionKey: key, sshgrant.SessionCertificate: raw},
	}, nil
}

// stamp is what Watch compares between two looks at the agent's directory:
// each file's identity, size and modification time.
type stamp [3]struct {
	ino, size int64
	mod       time.Time
}

func (p *Provider) stamp() stamp {
	var s stamp
	dir := filepath.Join(p.agents, p.Name)
	for i, name := range []string{sshgrant.KeyFile, sshgrant.PublicKeyFile, sshgrant.CertificateFile} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		s[i].size, s[i].mod = info.Size(), info.ModTime()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			s[i].ino = int64(stat.Ino)
		}
	}
	return s
}

// Watch looks at the agent's directory every few seconds, and reads it
// again whenever a file there changed: a new grant returns the new
// certificate; while the session holds one, a certificate that expired or
// was dropped returns why.
func (p *Provider) Watch(ctx context.Context, held provider.Credential) (provider.Credential, error) {
	holding := held.Files != nil
	var seen stamp
	for first := true; ; first = false {
		if holding && !held.Expires.IsZero() && !p.now().Before(held.Expires) {
			return provider.Credential{}, errors.New("the certificate of " + p.Name + " expired at " + held.Expires.Format("2006-01-02 15:04 MST") + ": " + p.ask())
		}
		if s := p.stamp(); first || s != seen {
			seen = s
			c, err := p.load()
			switch {
			case err == nil && !same(c, held):
				return c, nil
			case err != nil && holding:
				return provider.Credential{}, err
			}
		}
		if err := timing.SleepContext(ctx, p.poll); err != nil {
			return provider.Credential{}, err
		}
	}
}

// same reports whether a and b carry the same files.
func same(a, b provider.Credential) bool {
	if len(a.Files) != len(b.Files) {
		return false
	}
	for name, raw := range a.Files {
		if other, ok := b.Files[name]; !ok || !bytes.Equal(raw, other) {
			return false
		}
	}
	return true
}
