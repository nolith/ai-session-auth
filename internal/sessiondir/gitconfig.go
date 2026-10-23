package sessiondir

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
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

// Remote returns the host and the repository path Git asks the session's
// credential helper about for a remote URL: https://host/path itself, and
// git@host:path and ssh://git@host/path, which the session's gitconfig
// rewrites to it. Any other form never reaches the helper, and is refused:
// http, another SSH user, an SSH port, a query or a fragment.
func Remote(raw string) (host, path string, err error) {
	invalid := errors.New("not a Git remote a session hands its credentials to: use https://HOST/PATH, git@HOST:PATH or ssh://git@HOST/PATH")
	if strings.ContainsAny(raw, " \t\r\n\x00") {
		return "", "", invalid
	}
	if rest, ok := strings.CutPrefix(raw, "git@"); ok && !strings.Contains(raw, "://") {
		host, path, ok = strings.Cut(rest, ":")
		if !ok || !gitHost.MatchString(host) || strings.Contains(host, ":") || path == "" {
			return "", "", invalid
		}
		return host, path, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || !gitHost.MatchString(parsed.Host) {
		return "", "", invalid
	}
	switch parsed.Scheme {
	case "https":
	case "ssh":
		// The rewrite is for git@ on the default port only.
		if parsed.User == nil || parsed.User.Username() != "git" || parsed.Port() != "" {
			return "", "", invalid
		}
	default:
		return "", "", invalid
	}
	// Escaped, as Git sends it: the allowlist refuses an escape outright.
	path = strings.TrimPrefix(parsed.EscapedPath(), "/")
	if path == "" {
		return "", "", invalid
	}
	return parsed.Host, path, nil
}
