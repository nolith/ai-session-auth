package sshcert

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/sshgrant"
)

// Renderer writes ssh/config, the ssh_config an agent passes with
// ssh -F "$GEAS_SESSION_DIR/ssh/config": the key and certificate in the
// session directory, the user agent, nothing else.
type Renderer struct{}

var _ provider.Renderer = Renderer{}

func (Renderer) Kind() string { return ID }

// Render writes the configuration for the one ssh credential, and removes
// it when there is none, once the certificate expired or was dropped, so
// that ssh -F fails at once rather than offer a key nothing accepts.
func (Renderer) Render(dir *sessiondir.Dir, creds map[string]provider.Credential) error {
	if len(creds) == 0 {
		return dir.RemoveFile(sshgrant.SessionConfig)
	}
	if _, ok := creds[ID]; !ok || len(creds) != 1 {
		return errors.New("invalid ssh provider ID")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	key, err := dir.Join(sshgrant.SessionKey)
	if err != nil {
		return err
	}
	cert, err := dir.Join(sshgrant.SessionCertificate)
	if err != nil {
		return err
	}
	var paths []string
	for _, path := range []string{key, cert, filepath.Join(home, ".ssh", "known_hosts")} {
		quoted, err := quote(path)
		if err != nil {
			return err
		}
		paths = append(paths, quoted)
	}
	config := `# Written by geas for this session; replaced whole when the certificate
# changes. Replaces ~/.ssh/config entirely: no agent forwarding, no shared
# connections, no other keys, no new host keys.
Host *
  User agent
  IdentityFile ` + paths[0] + `
  CertificateFile ` + paths[1] + `
  IdentitiesOnly yes
  IdentityAgent none
  ForwardAgent no
  ControlMaster no
  BatchMode yes
  StrictHostKeyChecking yes
  UserKnownHostsFile ` + paths[2] + `
  UpdateHostKeys no
`
	return dir.WriteFile(sshgrant.SessionConfig, []byte(config))
}

// quote writes path as one ssh_config argument: in double quotes, which a
// path cannot contain, with ssh's % tokens escaped.
func quote(path string) (string, error) {
	if strings.ContainsAny(path, "\"\n\r\x00") {
		return "", errors.New("a path ssh_config cannot quote")
	}
	return `"` + strings.ReplaceAll(path, "%", "%%") + `"`, nil
}
