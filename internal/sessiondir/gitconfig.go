package sessiondir

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// GitConfigFile is the session's gitconfig, which the harness's Git includes
// last, through GIT_CONFIG_* (`include.path`), so that it wins over the
// user's own configuration.
const GitConfigFile = "gitconfig"

// SSHCommand keeps Git's SSH away from the user's agent and from prompts:
// only keys on disk, never a passphrase or a host-key question.
const SSHCommand = "ssh -o IdentityAgent=none -o BatchMode=yes"

// gitHost is what a host may look like in a URL.
var gitHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?$`)

// GitConfig renders the session's gitconfig: core.sshCommand, and for each
// host the SSH-to-HTTPS rewrite and helper as the only credential helper,
// with the repository path, which the helper's allowlist needs.
func GitConfig(helper string, hosts []string) ([]byte, error) {
	if strings.ContainsAny(helper, "\n\x00") {
		return nil, errors.New("invalid credential helper")
	}
	var b bytes.Buffer
	b.WriteString("# Written by geas for this session.\n")
	fmt.Fprintf(&b, "[core]\n\tsshCommand = %s\n", gitValue(SSHCommand))
	for _, host := range hosts {
		if !gitHost.MatchString(host) {
			return nil, errors.New("invalid Git host")
		}
		fmt.Fprintf(&b, "[url %s]\n", gitValue("https://"+host+"/"))
		fmt.Fprintf(&b, "\tinsteadOf = %s\n\tinsteadOf = %s\n", gitValue("git@"+host+":"), gitValue("ssh://git@"+host+"/"))
		// The empty helper drops every helper configured before this one for
		// the host, the user's keychain included.
		fmt.Fprintf(&b, "[credential %s]\n", gitValue("https://"+host))
		fmt.Fprintf(&b, "\thelper = \"\"\n\thelper = %s\n\tuseHttpPath = true\n", gitValue(helper))
	}
	return b.Bytes(), nil
}

// gitValue quotes a value or a subsection name for a gitconfig file.
func gitValue(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}
