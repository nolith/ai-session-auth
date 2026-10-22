package gitlab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sessiondir"
)

// configDir is glab's configuration directory in the session, GLAB_CONFIG_DIR.
const configDir = "glab"

// Renderer writes glab's config.yml, one entry per GitLab host, each with its
// session PAT. glab keeps every host in that one file, so it is rendered from
// every GitLab credential at once.
//
// The format is glab 1.123's, as `glab config set --host` writes it, minus
// the comments and the keys left at their defaults. check_update and
// show_whats_new are off: otherwise glab writes its update check and the
// last version it announced back into this file on every run, and announces
// them on stderr.
type Renderer struct{}

var _ provider.Renderer = Renderer{}

func (Renderer) Kind() string { return "gitlab" }

// defaultHost is glab's default host when the session has it.
const defaultHost = "gitlab.com"

// hostname is what a host may look like as a YAML key, unquoted.
var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)

func (Renderer) Render(dir *sessiondir.Dir, creds map[string]provider.Credential) error {
	hosts := map[string]provider.Credential{}
	for id, c := range creds {
		h, ok := strings.CutPrefix(id, "gitlab:")
		if !ok || !hostname.MatchString(h) {
			return errors.New("invalid GitLab provider ID")
		}
		hosts[h] = c
	}
	names := slices.Sorted(maps.Keys(hosts))
	if len(names) == 0 {
		return errors.New("no GitLab credential to render")
	}
	// The default host, for commands run outside a repository.
	primary := names[0]
	if _, ok := hosts[defaultHost]; ok {
		primary = defaultHost
	}
	var b bytes.Buffer
	b.WriteString("# Written by geas for this session; replaced whole when a credential changes.\n")
	b.WriteString("git_protocol: https\ncheck_update: false\nshow_whats_new: false\nno_prompt: true\n")
	fmt.Fprintf(&b, "host: %s\nhosts:\n", primary)
	for _, name := range names {
		c := hosts[name]
		fmt.Fprintf(&b, "    %s:\n", name)
		fmt.Fprintf(&b, "        api_host: %s\n        api_protocol: https\n        git_protocol: https\n", name)
		fmt.Fprintf(&b, "        token: %s\n", yamlString(c.Secret))
		if c.User != "" {
			fmt.Fprintf(&b, "        user: %s\n", yamlString(c.User))
		}
	}
	return dir.WriteFile(configDir+"/config.yml", b.Bytes())
}

// yamlString writes value as a double-quoted YAML scalar: JSON's escaping is
// a subset of YAML's, so no token or name can change the document's shape.
func yamlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
