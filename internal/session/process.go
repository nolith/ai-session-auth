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

// UnwrappedPath is the variable a packaging wrapper that puts geas's own
// tools first on PATH, such as the flake's ssh-keygen with FIDO2 support,
// saves the caller's PATH in: the harness gets that PATH back, unchanged.
const UnwrappedPath = "GEAS_UNWRAPPED_PATH"

// unwrapPath restores the PATH a packaging wrapper saved in env.
func unwrapPath(env map[string]string) {
	if path, ok := env[UnwrappedPath]; ok {
		env["PATH"] = path
		delete(env, UnwrappedPath)
	}
}

// sessionEnvironment is the harness's environment: base without inherited
// credentials, with the providers' exported variables, the session's own,
// and Git configured for the session.
func sessionEnvironment(base []string, dir *sessiondir.Dir, exported map[string]string) ([]string, error) {
	env := procenv.Map(base)
	unwrapPath(env)
	for _, key := range inheritedCredentials {
		delete(env, key)
	}
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

func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}

// Nested runs args in place of geas, inside the session GEAS_SESSION_DIR
// names: an agent starting a helper from its own shell shares its session
// instead of starting another one.
func Nested(args []string) error {
	if len(args) == 0 {
		return errors.New("specify a harness command after run --")
	}
	if _, err := sessiondir.FromEnv(); err != nil {
		// Not wrapped: a path error would print as a bare permission error.
		return errors.New("GEAS_SESSION_DIR names no usable session directory; unset GEAS_SESSION_DIR to start a new session")
	}
	env := procenv.Map(os.Environ())
	unwrapPath(env)
	os.Setenv("PATH", env["PATH"])
	executable, err := exec.LookPath(args[0])
	if err != nil {
		return errors.New("cannot start harness; check executable and arguments")
	}
	return syscall.Exec(executable, args, procenv.List(env))
}
