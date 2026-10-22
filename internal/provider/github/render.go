package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sessiondir"
)

// configDir is gh's configuration directory in the session, GH_CONFIG_DIR.
const configDir = "gh"

// Renderer writes gh's hosts.yml, one entry per GitHub host with its App
// token, and a config.yml that keeps gh on HTTPS and off prompts.
//
// The format is gh 2.102's with the token in the file instead of the
// keyring, as `gh auth login --insecure-storage` writes it: the token and the
// active user at the host's level, and again under users, keyed by login.
// Without users, gh migrates the file on its first run, which needs the
// network to learn the login. config.yml carries version 1 for the same
// reason.
type Renderer struct{}

var _ provider.Renderer = Renderer{}

func (Renderer) Kind() string { return "github" }

func (Renderer) Render(dir *sessiondir.Dir, creds map[string]provider.Credential) error {
	hosts := map[string]provider.Credential{}
	for id, c := range creds {
		h, ok := strings.CutPrefix(id, "github:")
		if !ok || h != host {
			return errors.New("invalid GitHub provider ID")
		}
		if c.User == "" {
			return errors.New("GitHub credential without a user")
		}
		hosts[h] = c
	}
	if len(hosts) == 0 {
		return errors.New("no GitHub credential to render")
	}
	var b bytes.Buffer
	b.WriteString("# Written by geas for this session; replaced whole when a credential changes.\n")
	for _, name := range slices.Sorted(maps.Keys(hosts)) {
		c := hosts[name]
		fmt.Fprintf(&b, "%s:\n", name)
		fmt.Fprintf(&b, "    git_protocol: https\n    user: %s\n    oauth_token: %s\n", yamlString(c.User), yamlString(c.Secret))
		fmt.Fprintf(&b, "    users:\n        %s:\n            oauth_token: %s\n", yamlString(c.User), yamlString(c.Secret))
	}
	settings := "# Written by geas for this session.\nversion: 1\ngit_protocol: https\nprompt: disabled\n"
	if err := dir.WriteFile(configDir+"/config.yml", []byte(settings)); err != nil {
		return err
	}
	return dir.WriteFile(configDir+"/hosts.yml", b.Bytes())
}

// yamlString writes value as a double-quoted YAML scalar: JSON's escaping is
// a subset of YAML's, so no token or name can change the document's shape.
func yamlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
