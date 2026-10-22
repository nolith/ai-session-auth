package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/procenv"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func appendGitConfig(env map[string]string, entries [][2]string) error {
	count := 0
	if value := env["GIT_CONFIG_COUNT"]; value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 0 || count > 256 {
			return errors.New("invalid inherited GIT_CONFIG_COUNT")
		}
	}
	for _, entry := range entries {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", count)] = entry[0]
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)] = entry[1]
		count++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	return nil
}

// inheritedCredentials never reach the harness or the reaper.
var inheritedCredentials = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "SSH_AUTH_SOCK", "GH_DEBUG", "GLAB_DEBUG", "GLAB_DEBUG_HTTP"}

func sessionEnvironment(settings config.Config, directory, address, binary string, base []string, lookup func(string) (string, error)) ([]string, error) {
	env := procenv.Map(base)
	for _, key := range inheritedCredentials {
		delete(env, key)
	}
	// A session started from inside another session's gh or glab is not a loop.
	delete(env, proxyMark)
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		return nil, err
	}
	for _, provider := range []struct {
		service, cli string
		active       bool
	}{{"github", "gh", config.Enabled(settings.GitHub.Enabled)}, {"gitlab", "glab", config.Enabled(settings.GitLab.Enabled)}} {
		if !provider.active {
			continue
		}
		executable, err := lookup(provider.cli)
		if err != nil {
			return nil, fmt.Errorf("install %s and add it to PATH", provider.cli)
		}
		absolute, err := filepath.Abs(executable)
		if err != nil {
			return nil, err
		}
		wrapper := "#!/bin/sh\nexec " + shellQuote(binary) + " _proxy " + shellQuote(provider.service) + " " + shellQuote(absolute) + " \"$@\"\n"
		if err = os.WriteFile(filepath.Join(bin, provider.cli), []byte(wrapper), 0700); err != nil {
			return nil, err
		}
	}
	env["PATH"] = bin + string(os.PathListSeparator) + env["PATH"]
	env["GEAS_SOCKET"] = address
	env["GH_PROMPT_DISABLED"] = "1"
	env["GIT_TERMINAL_PROMPT"] = "0"
	for _, cli := range []string{"gh", "glab"} {
		path := filepath.Join(directory, cli)
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, err
		}
		content := "git_protocol: https\n"
		key := "GH_CONFIG_DIR"
		if cli == "glab" {
			key = "GLAB_CONFIG_DIR"
			content = "host: gitlab.com\nhosts:\n  gitlab.com:\n    git_protocol: https\n    api_protocol: https\n    api_host: gitlab.com\n"
		}
		if err := os.WriteFile(filepath.Join(path, "config.yml"), []byte(content), 0600); err != nil {
			return nil, err
		}
		env[key] = path
	}
	entries := [][2]string{{"credential.helper", ""}, {"credential.helper", "!" + shellQuote(binary) + " _credential"}, {"credential.useHttpPath", "true"}}
	for _, provider := range []struct {
		host   string
		active bool
	}{{"github.com", config.Enabled(settings.GitHub.Enabled)}, {"gitlab.com", config.Enabled(settings.GitLab.Enabled)}} {
		if provider.active {
			for _, prefix := range []string{"git@" + provider.host + ":", "ssh://git@" + provider.host + "/"} {
				entries = append(entries, [2]string{"url.https://" + provider.host + "/.insteadOf", prefix})
			}
		}
	}
	if err := appendGitConfig(env, entries); err != nil {
		return nil, err
	}
	return procenv.List(env), nil
}

// proxyMark is set for the real CLI and everything it starts. Entering the
// proxy again for the same service means the CLI led back to its wrapper.
const proxyMark = "GEAS_PROXY"

// pathWithout drops dir from a PATH value, however the entry spells it.
func pathWithout(value, dir string) string {
	target, targetErr := os.Stat(dir)
	var kept []string
	for _, entry := range filepath.SplitList(value) {
		if entry != "" && filepath.Clean(entry) == filepath.Clean(dir) {
			continue
		}
		if info, err := os.Stat(entry); entry != "" && targetErr == nil && err == nil && os.SameFile(info, target) {
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

// ProxyCLI execs the real gh or glab with a session token in its environment.
// A session's wrapper calls it as `geas _proxy`, and it is going away with the
// wrappers themselves.
func ProxyCLI(service, executable string, args []string) error {
	cli := map[string]string{"github": "gh", "gitlab": "glab"}[service]
	if cli == "" {
		return errors.New("invalid provider")
	}
	if os.Getenv(proxyMark) == service {
		return fmt.Errorf("%s wrapper loop: %s leads back to the session wrapper; check PATH shims for %s, such as mise", cli, executable, cli)
	}
	response, err := rpc(brokerRequest{Service: service})
	if err != nil {
		return err
	}
	if response.Token == "" {
		return errors.New("broker did not return a token")
	}
	environment := procenv.Map(os.Environ())
	key := "GH_TOKEN"
	if service == "gitlab" {
		key = "GITLAB_TOKEN"
	}
	environment[key] = response.Token
	environment[proxyMark] = service
	// The recorded CLI may be a shim, such as mise's, that runs the next one on
	// PATH: without the wrappers there, that is never the wrapper again.
	environment["PATH"] = pathWithout(environment["PATH"], filepath.Join(filepath.Dir(os.Getenv("GEAS_SOCKET")), "bin"))
	return syscall.Exec(executable, append([]string{executable}, args...), procenv.List(environment))
}

// Credential answers one Git credential request, as `geas _credential` runs
// it, with the session broker the only source of answers.
func Credential(operation string, input io.Reader, output io.Writer) error {
	return credentialWith(operation, input, output, rpc)
}
func credentialWith(operation string, input io.Reader, output io.Writer, request func(brokerRequest) (brokerResponse, error)) error {
	if operation != "get" {
		return nil
	}
	scanner := bufio.NewScanner(io.LimitReader(input, 8192))
	values := map[string]string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("invalid Git credential request")
	}
	response, err := request(brokerRequest{Service: "git", Protocol: values["protocol"], Host: values["host"], Path: values["path"]})
	if response.Quit {
		// Git then fails at once, with no askpass or terminal prompt.
		fmt.Fprint(output, "quit=1\n\n")
	}
	if err != nil {
		return err
	}
	if response.Token != "" {
		if strings.ContainsAny(response.Token+response.Username, "\r\n") {
			return errors.New("invalid credential response")
		}
		_, err = fmt.Fprintf(output, "username=%s\npassword=%s\n\n", response.Username, response.Token)
	}
	return err
}
func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}
func executableLookup(value string) (string, error) { return exec.LookPath(value) }
