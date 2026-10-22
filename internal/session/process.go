package session

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/credhelper"
	"github.com/nolith/geas/internal/procenv"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/sandbox/nono"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/sessiondir"
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

// sessionEnvironment is the harness's environment: base without inherited
// credentials, with the providers' exported variables, the session's
// directory, and Git configured for the session.
func sessionEnvironment(settings config.Config, directory, address, binary string, base []string, lookup func(string) (string, error), dir *sessiondir.Dir, exported map[string]string) ([]string, error) {
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
	maps.Copy(env, exported)
	env["GEAS_SESSION_DIR"] = dir.Path()
	env["GEAS_SANDBOX_PROFILE"] = filepath.Join(dir.Path(), nono.ProfileFile)
	env["GIT_TERMINAL_PROMPT"] = "0"
	// Last, after anything inherited: the session's gitconfig wins.
	if err := appendGitConfig(env, [][2]string{{"include.path", filepath.Join(dir.Path(), sessiondir.GitConfigFile)}}); err != nil {
		return nil, err
	}
	return procenv.List(env), nil
}

// writeGitConfig writes the session's gitconfig, naming this binary as the
// credential helper of every host, and the hosts the helper answers for.
func writeGitConfig(dir *sessiondir.Dir, binary string, hosts []credhelper.Host) error {
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Host)
	}
	raw, err := sessiondir.GitConfig("!"+shellQuote(binary)+" credential", names)
	if err != nil {
		return err
	}
	if err = dir.WriteFile(sessiondir.GitConfigFile, raw); err != nil {
		return err
	}
	return credhelper.WriteHosts(dir, hosts)
}

// writeSandbox writes the session's sandbox policy, from what the providers
// protect, and the nono fragment a launcher's sandbox applies.
func writeSandbox(dir *sessiondir.Dir, providers []provider.Provider) error {
	state, err := secretfile.StateDirectory()
	if err != nil {
		return err
	}
	policy := sandbox.Merge(providers, state, dir.Path())
	if err = policy.Write(dir); err != nil {
		return err
	}
	_, err = nono.Write(dir, policy)
	return err
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

func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}
func executableLookup(value string) (string, error) { return exec.LookPath(value) }
