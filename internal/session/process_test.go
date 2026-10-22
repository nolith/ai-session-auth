package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nolith/geas/internal/credhelper"
	"github.com/nolith/geas/internal/procenv"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/provider/gitlab"
	"github.com/nolith/geas/internal/testutil"
)

func TestBrokerSkippedProviders(t *testing.T) {
	now := time.Now()
	b := &broker{Config: testutil.Config(), Now: func() time.Time { return now }, Deadline: now.Add(time.Hour), GitHubSkipped: "no login", GitLabSkipped: "issuer failed"}
	for _, tc := range []struct {
		request brokerRequest
		want    string
	}{
		{brokerRequest{Service: "github"}, "GitHub is not available in this session: no login"},
		{brokerRequest{Service: "gitlab"}, "GitLab is not available in this session: issuer failed"},
	} {
		r, err := b.answer(context.Background(), tc.request)
		if err == nil || err.Error() != tc.want || r.Token != "" || !errors.As(err, new(unavailableError)) {
			t.Errorf("%+v: %+v %v", tc.request, r, err)
		}
	}
}

func TestSessionEnvironmentAndGitRewrites(t *testing.T) {
	dir := t.TempDir()
	binary := "/tmp/test 'auth'"
	base := append(os.Environ(), "GH_TOKEN=old-secret", "GITLAB_TOKEN=old-secret", "SSH_AUTH_SOCK=old-socket", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Alice")
	session := testutil.SessionDir(t)
	c := testutil.Config()
	hosts, err := credhelper.Hosts([]provider.Provider{&gitlab.Provider{Config: c.GitLab}, github.New(c.GitHub, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err = writeGitConfig(session, binary, hosts); err != nil {
		t.Fatal(err)
	}
	values, err := sessionEnvironment(c, dir, "/tmp/broker.sock", binary, base, func(cli string) (string, error) { return "/usr/bin/" + cli, nil }, session, map[string]string{"GLAB_CONFIG_DIR": session.Path() + "/glab"})
	if err != nil {
		t.Fatal(err)
	}
	env := procenv.Map(values)
	if env["GEAS_SESSION_DIR"] != session.Path() || env["GLAB_CONFIG_DIR"] != session.Path()+"/glab" || env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Fatalf("session variables: %v", env)
	}
	for _, key := range []string{"GH_TOKEN", "GITLAB_TOKEN", "SSH_AUTH_SOCK"} {
		if _, ok := env[key]; ok {
			t.Errorf("inherited %s", key)
		}
	}
	// One include, after what the harness inherited.
	if env["GIT_CONFIG_COUNT"] != "2" || env["GIT_CONFIG_KEY_0"] != "user.name" || env["GIT_CONFIG_KEY_1"] != "include.path" || env["GIT_CONFIG_VALUE_1"] != filepath.Join(session.Path(), "gitconfig") {
		t.Fatalf("Git configuration: %v", env)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "bin", "gh"))
	if !strings.Contains(string(raw), shellQuote(binary)) {
		t.Fatal("wrapper path not escaped")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	cmd := exec.Command(git, "ls-remote", "--get-url", "git@github.com:alice/repo.git")
	cmd.Env = values
	output, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(output)) != "https://github.com/alice/repo.git" {
		t.Fatalf("rewrite: %s %v", output, err)
	}
	cmd = exec.Command(git, "config", "--get-all", "credential.https://gitlab.com.helper")
	cmd.Env = values
	output, err = cmd.Output()
	if err != nil || string(output) != "\n!"+shellQuote(binary)+" credential\n" {
		t.Fatalf("helper: %q %v", output, err)
	}
}

func TestBrokerSocketRPC(t *testing.T) {
	now := time.Now()
	b := &broker{Config: testutil.Config(), Now: time.Now, Deadline: now.Add(time.Hour), GitLabToken: "test-secret"}
	// Keep socket paths below the macOS UNIX-domain path length limit.
	dir, err := os.MkdirTemp("/tmp", "auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "broker.sock")
	s, err := startBroker(context.Background(), address, b)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skip("runtime prohibits UNIX sockets")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	info, _ := os.Stat(address)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe socket permissions")
	}
	t.Setenv("GEAS_SOCKET", address)
	r, err := rpc(brokerRequest{Service: "gitlab"})
	if err != nil || r.Token != "test-secret" {
		t.Fatalf("%+v %v", r, err)
	}
	connection, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.Write([]byte("invalid\n"))
	var malformed brokerResponse
	if err = json.NewDecoder(connection).Decode(&malformed); err != nil || malformed.Error == "" {
		t.Fatal("malformed input accepted")
	}
}

func TestHarnessExitAndTimeout(t *testing.T) {
	code, err := runHarness(context.Background(), []string{"/bin/sh", "-c", "exit 7"}, os.Environ(), time.Minute)
	if code != 7 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	code, err = runHarness(context.Background(), []string{"/bin/sh", "-c", "sleep 30"}, os.Environ(), 30*time.Millisecond)
	if code != 124 || err == nil {
		t.Fatalf("%d %v", code, err)
	}
}

func TestHarnessTimeoutKillsChildrenAfterLeaderExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "trap 'exit 0' TERM; (trap '' TERM; exec sleep 30) & child=$!; printf '%s' \"$child\" > " + shellQuote(pidFile) + "; wait"
	code, err := runHarness(context.Background(), []string{"/bin/sh", "-c", script}, os.Environ(), 100*time.Millisecond)
	if code != 124 || err == nil {
		t.Fatalf("%d %v", code, err)
	}
	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(raw))
	if parseErr != nil {
		t.Fatalf("invalid child pid %q: %v", raw, parseErr)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			// Avoid leaking the test child if the assertion fails.
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child process %d survived session shutdown: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestIssuerReadCancellation(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ReadIssuer(ctx, read, new(bytes.Buffer), true); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}

// shimSession runs `glab api --method GET user` through the session's glab
// wrapper, with lookup recording shim as the real glab, as mise's shim is when
// it is first on PATH. PATH is the wrappers, then the shim's directory, then a
// fake glab.
func shimSession(t *testing.T, shim string) (string, string, error) {
	t.Helper()
	real := testutil.FakeGlab(t)
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "glab"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	// Keep socket paths below the macOS UNIX-domain path length limit.
	dir, err := os.MkdirTemp("/tmp", "auth-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	address := filepath.Join(dir, "broker.sock")
	now := time.Now()
	server, err := startBroker(context.Background(), address, &broker{Config: testutil.Config(), Now: time.Now, Deadline: now.Add(time.Hour), GitLabToken: "session-secret"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skip("runtime prohibits UNIX sockets")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.close)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+real+string(os.PathListSeparator)+os.Getenv("PATH"))
	values, err := sessionEnvironment(testutil.Config(), dir, address, binary, base, func(cli string) (string, error) { return filepath.Join(shims, cli), nil }, testutil.SessionDir(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Every hop of a loop execs in place, so killing the one process ends it.
	command := exec.CommandContext(ctx, filepath.Join(dir, "bin", "glab"), "api", "--method", "GET", "user")
	command.Env = values
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if ctx.Err() != nil {
		t.Fatalf("glab did not terminate: %s", stderr.String())
	}
	return real, string(output) + stderr.String(), err
}

// mise with an inactive glab runs the next glab on PATH, skipping its own
// directory: inside a session, that used to be the wrapper again.
func TestProxyShimReachesRealCLI(t *testing.T) {
	real, output, err := shimSession(t, `#!/bin/sh
self=$(cd "$(dirname "$0")" && pwd)
IFS=:
for dir in $PATH; do
	[ "$dir" = "$self" ] && continue
	[ -x "$dir/glab" ] && exec "$dir/glab" "$@"
done
exit 127
`)
	if err != nil || strings.TrimSpace(output) != `{"username":"alice"}` {
		t.Fatalf("%v: %s", err, output)
	}
	calls := testutil.Calls(t, real)
	if len(calls) != 1 {
		t.Fatalf("real glab called %d times", len(calls))
	}
	env, _ := os.ReadFile(filepath.Join(real, "env.1"))
	for _, want := range []string{"\nGITLAB_TOKEN=session-secret\n", "\n" + proxyMark + "=gitlab\n"} {
		if !strings.Contains("\n"+string(env), want) {
			t.Errorf("real glab environment lacks %q", strings.TrimSpace(want))
		}
	}
}

// A shim that calls the wrapper by its path cannot be fixed through PATH; the
// guard stops it at the second entry instead of spinning.
func TestProxyLoopFailsFast(t *testing.T) {
	_, output, err := shimSession(t, `#!/bin/sh
exec "$(dirname "$GEAS_SOCKET")/bin/glab" "$@"
`)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(output, "glab wrapper loop") {
		t.Fatalf("%v: %s", err, output)
	}
}

func TestPathWithout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	value := strings.Join([]string{bin, "/usr/bin", bin + "/", link, "", "/bin"}, string(os.PathListSeparator))
	if got, want := pathWithout(value, bin), strings.Join([]string{"/usr/bin", "", "/bin"}, string(os.PathListSeparator)); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestMain lets the wrapper tests re-execute this binary as the session's
// proxy, which is what a session's glab or gh wrapper does. main does the same
// dispatch with the same exit code.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_proxy" {
		if err := ProxyCLI(os.Args[2], os.Args[3], os.Args[4:]); err != nil {
			fmt.Fprintln(os.Stderr, "geas:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The providers' variables reach the harness with the session's directory
// expanded; a conflict, or a session variable, stops the session first.
func TestProviderEnv(t *testing.T) {
	c := testutil.Config()
	providers := []provider.Provider{&gitlab.Provider{Config: c.GitLab}, github.New(c.GitHub, nil)}
	env, err := providerEnv(providers, "/state/geas/sessions/x")
	if err != nil || len(env) != 3 || env["GLAB_CONFIG_DIR"] != "/state/geas/sessions/x/glab" || env["GH_CONFIG_DIR"] != "/state/geas/sessions/x/gh" || env["GH_PROMPT_DISABLED"] != "1" {
		t.Fatalf("%v %v", env, err)
	}
	for _, name := range []string{"GEAS_SESSION_DIR", "GEAS_SANDBOX_PROFILE", "GIT_TERMINAL_PROMPT", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG"} {
		if !sessionVariable(name) {
			t.Errorf("%s is not the session's", name)
		}
	}
	for _, name := range []string{"GLAB_CONFIG_DIR", "GIT_DIR", "PATH"} {
		if sessionVariable(name) {
			t.Errorf("%s is the session's", name)
		}
	}
}
