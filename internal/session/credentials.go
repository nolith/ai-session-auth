package session

import (
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/provider/gitlab"
	"github.com/nolith/geas/internal/sessiondir"
)

// renderers are the kinds' renderers, by kind.
var renderers = map[string]provider.Renderer{}

func init() {
	for _, r := range []provider.Renderer{gitlab.Renderer{}, github.Renderer{}} {
		renderers[r.Kind()] = r
	}
}

// sessionVariable reports whether the session itself sets name, which no
// provider may then set.
func sessionVariable(name string) bool {
	switch name {
	case "GEAS_SESSION_DIR", "GEAS_SANDBOX_PROFILE", "GIT_TERMINAL_PROMPT", "GIT_CONFIG":
		return true
	}
	return strings.HasPrefix(name, "GIT_CONFIG_")
}

// providerEnv merges the providers' Setup.Env and expands $GEAS_SESSION_DIR
// in it.
func providerEnv(providers []provider.Provider, dir string) (map[string]string, error) {
	env, err := provider.MergeEnv(providers, sessionVariable)
	if err != nil {
		return nil, err
	}
	for name, value := range env {
		env[name] = os.Expand(value, func(variable string) string {
			if variable == "GEAS_SESSION_DIR" {
				return dir
			}
			return "$" + variable
		})
	}
	return env, nil
}

// credentials writes each credential the session holds into its directory:
// the secret, for the credential helper, the provider's files, and its
// kind's tool configuration, rendered from every credential of that kind.
type credentials struct {
	dir    *sessiondir.Dir
	mu     sync.Mutex
	byKind map[string]map[string]provider.Credential
}

func newCredentials(dir *sessiondir.Dir) *credentials {
	return &credentials{dir: dir, byKind: map[string]map[string]provider.Credential{}}
}

// put stores c as p's credential, replacing any earlier one; every file is
// replaced atomically.
func (s *credentials) put(p provider.Provider, c provider.Credential) error {
	renderer, ok := renderers[p.Kind()]
	if !ok {
		return errors.New("no renderer for provider kind " + p.Kind())
	}
	secret, err := sessiondir.ProviderFile("secrets", p.ID())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.dir.WriteFile(secret, []byte(c.Secret)); err != nil {
		return err
	}
	for rel, raw := range c.Files {
		if err = s.dir.WriteFile(rel, raw); err != nil {
			return err
		}
	}
	kind := s.byKind[p.Kind()]
	if kind == nil {
		kind = map[string]provider.Credential{}
		s.byKind[p.Kind()] = kind
	}
	kind[p.ID()] = c
	return renderer.Render(s.dir, kind)
}
