package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nolith/geas/internal/config"
)

func TestBrokerRepositoryBoundaries(t *testing.T) {
	now := time.Now()
	b := &Broker{Config: testConfig(), Now: func() time.Time { return now }, Deadline: now.Add(time.Hour), GitHubToken: func(context.Context) (string, error) { return "ghu_test", nil }, GitLabToken: "gl-test"}
	for _, tc := range []struct{ host, path, protocol, want string }{
		{"github.com", "/ALICE/repo.git", "https", "ghu_test"},
		{"gitlab.com", "group/project.git", "https", "gl-test"},
		{"github.com", "alice/other", "https", ""},
		{"github.com.evil", "alice/repo", "https", ""},
		{"github.com", "alice/repo", "http", ""},
		{"github.com", "alice/../repo", "https", ""},
		{"github.com", "alice/%72epo", "https", ""},
		{"gitlab.com", "GROUP/project", "https", ""},
	} {
		r, err := b.answer(context.Background(), BrokerRequest{Service: "git", Host: tc.host, Path: tc.path, Protocol: tc.protocol})
		if err != nil || r.Token != tc.want {
			t.Fatalf("%+v: %+v %v", tc, r, err)
		}
	}
	b.Deadline = now
	if _, err := b.answer(context.Background(), BrokerRequest{Service: "github"}); err == nil {
		t.Fatal("issued token after expiry")
	}
}
func TestCredentialProtocol(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	query := func(r BrokerRequest) (BrokerResponse, error) {
		calls++
		if r.Host != "github.com" || r.Path != "alice/repo.git" {
			t.Error("bad request")
		}
		return BrokerResponse{Username: "x-access-token", Token: "test-token"}, nil
	}
	if err := credentialWith("get", strings.NewReader("protocol=https\nhost=github.com\npath=alice/repo.git\n\n"), &out, query); err != nil {
		t.Fatal(err)
	}
	if out.String() != "username=x-access-token\npassword=test-token\n\n" {
		t.Fatal("bad helper response")
	}
	if err := credentialWith("store", strings.NewReader("password=secret"), &out, query); err != nil || calls != 1 {
		t.Fatal("store operation accessed broker")
	}
}
func TestBrokerSkippedProviders(t *testing.T) {
	now := time.Now()
	b := &Broker{Config: testConfig(), Now: func() time.Time { return now }, Deadline: now.Add(time.Hour), GitHubSkipped: "no login", GitLabSkipped: "issuer failed"}
	for _, tc := range []struct {
		request BrokerRequest
		want    string
	}{
		{BrokerRequest{Service: "github"}, "GitHub is not available in this session: no login"},
		{BrokerRequest{Service: "gitlab"}, "GitLab is not available in this session: issuer failed"},
		{BrokerRequest{Service: "git", Protocol: "https", Host: "github.com", Path: "alice/repo.git"}, "GitHub is not available in this session: no login"},
		{BrokerRequest{Service: "git", Protocol: "https", Host: "gitlab.com", Path: "group/project.git"}, "GitLab is not available in this session: issuer failed"},
	} {
		r, err := b.answer(context.Background(), tc.request)
		if err == nil || err.Error() != tc.want || r.Token != "" || !errors.As(err, new(unavailableError)) {
			t.Errorf("%+v: %+v %v", tc.request, r, err)
		}
	}
	// Outside the allowlist the helper stays silent, as for any provider.
	if r, err := b.answer(context.Background(), BrokerRequest{Service: "git", Protocol: "https", Host: "github.com", Path: "alice/other"}); err != nil || r.Token != "" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestCredentialQuitsForSkippedProvider(t *testing.T) {
	var out bytes.Buffer
	query := func(BrokerRequest) (BrokerResponse, error) {
		return BrokerResponse{Quit: true}, errors.New("GitLab is not available in this session: issuer failed")
	}
	err := credentialWith("get", strings.NewReader("protocol=https\nhost=gitlab.com\npath=group/project.git\n\n"), &out, query)
	if err == nil || out.String() != "quit=1\n\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}
func TestSessionEnvironmentAndGitRewrites(t *testing.T) {
	dir := t.TempDir()
	binary := "/tmp/test 'auth'"
	base := append(os.Environ(), "GH_TOKEN=old-secret", "GITLAB_TOKEN=old-secret", "SSH_AUTH_SOCK=old-socket", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Alice")
	values, err := sessionEnvironment(testConfig(), dir, "/tmp/broker.sock", binary, base, func(cli string) (string, error) { return "/usr/bin/" + cli, nil })
	if err != nil {
		t.Fatal(err)
	}
	env := environmentMap(values)
	for _, key := range []string{"GH_TOKEN", "GITLAB_TOKEN", "SSH_AUTH_SOCK"} {
		if _, ok := env[key]; ok {
			t.Errorf("inherited %s", key)
		}
	}
	if env["GIT_CONFIG_KEY_0"] != "user.name" {
		t.Fatal("lost inherited config")
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
	cmd = exec.Command(git, "config", "--get-all", "credential.helper")
	cmd.Env = values
	output, err = cmd.Output()
	if err != nil || !strings.HasSuffix(string(output), "!"+shellQuote(binary)+" _credential\n") {
		t.Fatalf("helper: %s %v", output, err)
	}
}
func TestBrokerSocketRPC(t *testing.T) {
	now := time.Now()
	b := &Broker{Config: testConfig(), Now: time.Now, Deadline: now.Add(time.Hour), GitLabToken: "test-secret"}
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
	r, err := rpc(BrokerRequest{Service: "gitlab"})
	if err != nil || r.Token != "test-secret" {
		t.Fatalf("%+v %v", r, err)
	}
	connection, err := net.Dial("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.Write([]byte("invalid\n"))
	var malformed BrokerResponse
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
	if _, err = readIssuer(ctx, read, new(bytes.Buffer), true); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}
func TestBrokerGitLabNamespaces(t *testing.T) {
	now := time.Now()
	c := testConfig()
	c.GitLab = namespacedConfig()
	b := &Broker{Config: c, Now: func() time.Time { return now }, Deadline: now.Add(time.Hour), GitLabToken: "gl-test"}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"group/project.git", true},
		{"group/sub/deeper/project", true},
		{"other/sub/project", true},
		{"alice/dotfiles.git", true},
		{"elsewhere/project", true},
		{"group-foo/project", false},
		{"groupx/project", false},
		{"other/subway/project", false},
		{"other/project", false},
		{"elsewhere/project-2", false},
		{"elsewhere/project/sub", false},
		{"alice-bot/project", false},
		{"GROUP/project", false},
		{"group", false},
		{"group/../evil/project", false},
		{"group/%2e%2e/evil", false},
	} {
		r, err := b.answer(context.Background(), BrokerRequest{Service: "git", Host: "gitlab.com", Path: tc.path, Protocol: "https"})
		if err != nil || (r.Token != "") != tc.want {
			t.Errorf("%+v: %+v %v", tc, r, err)
		}
	}
	b.Config.GitLab.PersonalProjects = false
	if r, _ := b.answer(context.Background(), BrokerRequest{Service: "git", Host: "gitlab.com", Path: "alice/dotfiles", Protocol: "https"}); r.Token != "" {
		t.Fatal("personal project allowed without personal_projects")
	}
}

func TestBrokerGitHubOwners(t *testing.T) {
	now := time.Now()
	c := testConfig()
	c.GitHub.Owners = []string{"nolith", "Some-Org"}
	b := &Broker{Config: c, Now: func() time.Time { return now }, Deadline: now.Add(time.Hour), GitHubToken: func(context.Context) (string, error) { return "ghu_test", nil }}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"nolith/anything.git", true},
		{"NOLITH/Anything", true},
		{"some-org/repo", true},
		{"alice/repo", true},
		{"ALICE/REPO.git", true},
		{"nolith-foo/repo", false},
		{"nolithx/repo", false},
		{"no/lith/repo", false},
		{"nolith", false},
		{"alice/other", false},
		{"someone-else/x", false},
		{"nolith/../evil/x", false},
		{"nolith/%2e%2e/x", false},
	} {
		r, err := b.answer(context.Background(), BrokerRequest{Service: "git", Host: "github.com", Path: tc.path, Protocol: "https"})
		if err != nil || (r.Token != "") != tc.want {
			t.Errorf("%+v: %+v %v", tc, r, err)
		}
	}
	// Owners alone, without explicit repositories.
	b.Config.GitHub.Repositories = nil
	if r, _ := b.answer(context.Background(), BrokerRequest{Service: "git", Host: "github.com", Path: "alice/repo", Protocol: "https"}); r.Token != "" {
		t.Fatal("repository allowed after removing it")
	}
	if r, _ := b.answer(context.Background(), BrokerRequest{Service: "git", Host: "github.com", Path: "nolith/repo", Protocol: "https"}); r.Token == "" {
		t.Fatal("owner not allowed without repositories")
	}
	// GitLab stays case-sensitive whatever the GitHub owners.
	if gitlabAllowed(config.GitLabConfig{Groups: []string{"nolith"}}, "NOLITH/project") {
		t.Fatal("GitLab group matched in another case")
	}
}

// TestMain lets a test re-execute this binary as the launcher itself, with
// main's own signal handling, by passing its arguments as JSON. The launcher
// re-executes it too, as "_reaper" or "_proxy".
func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv("GEAS_TEST_ARGS"); ok {
		os.Unsetenv("GEAS_TEST_ARGS")
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(2)
		}
		os.Args = append([]string{"geas"}, args...)
		main()
	}
	// The launcher's own re-executions: the reaper, and gh/glab through _proxy.
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "_") {
		main()
	}
	os.Exit(m.Run())
}

type testSession struct {
	launcher *exec.Cmd
	done     chan error
	output   *os.File // read end of the launcher's stdout and stderr
	glab     string   // fake glab directory and call log
	state    string   // XDG_STATE_HOME: glab.lock and reaper.log
}

// startSession runs the launcher with a fake glab issuer around the harness
// script, which runs under /bin/sh, and returns once the harness has started.
func startSession(t *testing.T, script string) *testSession {
	t.Helper()
	s := &testSession{glab: fakeGlab(t), state: t.TempDir(), done: make(chan error, 1)}
	dir := t.TempDir()
	c := config.Config{SessionHours: 1, GitHub: config.GitHubConfig{Enabled: boolPointer(false)}, GitLab: glabConfig()}
	raw, _ := json.Marshal(c)
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "harness.pid")
	args, _ := json.Marshal([]string{"--config", configPath, "run", "--", "/bin/sh", "-c", "echo $$ >" + shellQuote(started) + "; " + script})
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s.output = read
	s.launcher = exec.Command(os.Args[0])
	s.launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+s.glab+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+s.state)
	s.launcher.Stdout, s.launcher.Stderr = write, write
	if err = s.launcher.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	exited := make(chan error, 1)
	go func() { exited <- s.launcher.Wait() }()
	// Wait for the reaper too, so that the temporary directories outlive it.
	t.Cleanup(func() { waitReaper(t, s) })
	deadline := time.After(20 * time.Second)
	for {
		if raw, err := os.ReadFile(started); err == nil && strings.HasSuffix(string(raw), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				// Only matters if nothing else stops it.
				t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			}
			go func() { s.done <- <-exited }()
			return s
		}
		select {
		case err := <-exited:
			// A harness that exits at once may finish before it is seen.
			if raw, _ := os.ReadFile(started); len(raw) > 0 {
				s.done <- err
				return s
			}
			output, _ := io.ReadAll(read)
			if strings.Contains(string(output), "UNIX socket") {
				t.Skip("runtime prohibits UNIX sockets")
			}
			t.Fatalf("launcher exited before the harness started: %v %s", err, output)
		case <-deadline:
			s.launcher.Process.Kill()
			t.Fatal("harness did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (s *testSession) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		s.launcher.Process.Kill()
		t.Fatal("launcher did not stop")
	}
}

// reaperPID finds this session's reaper by the PAT name on its command line.
func reaperPID(t *testing.T, s *testSession) int {
	t.Helper()
	name, err := os.ReadFile(filepath.Join(s.glab, "name"))
	if err != nil || len(name) == 0 {
		return 0
	}
	output, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "_reaper watch ") && strings.Contains(line, string(name)) {
			pid, _ := strconv.Atoi(strings.Fields(line)[0])
			return pid
		}
	}
	return 0
}
func waitReaper(t *testing.T, s *testSession) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); reaperPID(t, s) != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(reaperPID(t, s), syscall.SIGKILL)
			t.Fatal("reaper still running")
		}
	}
}
func deletes(t *testing.T, s *testSession) (count int, checked bool) {
	t.Helper()
	for _, call := range fakeGlabCalls(t, s.glab) {
		switch {
		case slices.Equal(call[3:], []string{"--method", "DELETE", "personal_access_tokens/42"}):
			count++
		case slices.Equal(call[3:], []string{"--method", "GET", "personal_access_tokens/42"}):
			checked = true
		}
	}
	return count, checked
}

// The timing log measures the steps before the harness, then goes quiet: a
// line once the harness owns the terminal would corrupt an agent's TUI.
func TestStepTimerSilencesAfterTheHarness(t *testing.T) {
	var output bytes.Buffer
	if newStepTimer("", &output) != nil || newStepTimer("http", &output) != nil {
		t.Fatal("timing enabled without GEAS_DEBUG=timing")
	}
	timer := newStepTimer("http,timing", &output)
	timer.step("config load")
	timer.stop()
	before := output.String()
	timer.step("mint")
	if output.String() != before {
		t.Fatalf("printed after the harness: %q", output.String())
	}
	for _, want := range []string{"config load", "harness start"} {
		if !strings.Contains(before, want) {
			t.Errorf("timing log lacks %q: %q", want, before)
		}
	}
}

// A closed terminal or pane takes the launcher's output with it, then sends
// SIGHUP: the harness must still be stopped and the session PAT revoked.
func TestHangupStopsHarnessAndRevokes(t *testing.T) {
	s := startSession(t, "exec sleep 30")
	s.output.Close()
	if err := s.launcher.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
	// 130 is a handled interruption; a launcher killed by SIGHUP or SIGPIPE
	// reports -1.
	if code := s.launcher.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit %d", code)
	}
	waitReaper(t, s)
	if count, checked := deletes(t, s); count != 1 || checked {
		t.Fatalf("%d DELETE calls, reaper checked: %v", count, checked)
	}
}

// The launcher revokes on a clean exit and tells the reaper, which does nothing.
func TestCleanExitRevokesOnce(t *testing.T) {
	s := startSession(t, "exit 0")
	io.Copy(io.Discard, s.output)
	s.wait(t)
	if code := s.launcher.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	waitReaper(t, s)
	if count, checked := deletes(t, s); count != 1 || checked {
		t.Fatalf("%d DELETE calls, reaper checked: %v", count, checked)
	}
	if _, err := os.Stat(filepath.Join(s.state, "geas", "reaper.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reaper logged on a clean exit: %v", err)
	}
}

// SIGKILL, as from a herdr pane that closes, leaves no chance to revoke: the
// reaper, outside the launcher's session and process tree, must do it.
func TestKilledLauncherIsReaped(t *testing.T) {
	s := startSession(t, "exec sleep 30")
	var reaper int
	for deadline := time.Now().Add(10 * time.Second); reaper == 0; time.Sleep(20 * time.Millisecond) {
		if reaper = reaperPID(t, s); reaper == 0 && time.Now().After(deadline) {
			t.Fatal("no reaper")
		}
	}
	launcher := s.launcher.Process.Pid
	reaperSession, _ := getsid(reaper)
	launcherSession, _ := getsid(launcher)
	reaperGroup, _ := syscall.Getpgid(reaper)
	parent, _ := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(reaper)).Output()
	if reaperSession == launcherSession || reaperGroup == syscall.Getpgrp() || strings.TrimSpace(string(parent)) == strconv.Itoa(launcher) {
		t.Fatalf("reaper not detached: session %d (launcher %d), group %d, parent %s", reaperSession, launcherSession, reaperGroup, parent)
	}
	if err := s.launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
	// The harness survives SIGKILL of the launcher; holding no end of the
	// reaper's pipe, it cannot delay the revocation either.
	waitReaper(t, s)
	if count, checked := deletes(t, s); count != 1 || !checked {
		t.Fatalf("%d DELETE calls, reaper checked: %v", count, checked)
	}
	log, err := os.ReadFile(filepath.Join(s.state, "geas", "reaper.log"))
	if err != nil || !strings.Contains(string(log), "launcher gone without revoking; revoked") {
		t.Fatalf("reaper log: %q %v", log, err)
	}
}

// shimSession runs `glab api --method GET user` through the session's glab
// wrapper, with lookup recording shim as the real glab, as mise's shim is when
// it is first on PATH. PATH is the wrappers, then the shim's directory, then a
// fake glab.
func shimSession(t *testing.T, shim string) (string, string, error) {
	t.Helper()
	real := fakeGlab(t)
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
	server, err := startBroker(context.Background(), address, &Broker{Config: testConfig(), Now: time.Now, Deadline: now.Add(time.Hour), GitLabToken: "session-secret"})
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
	values, err := sessionEnvironment(testConfig(), dir, address, binary, base, func(cli string) (string, error) { return filepath.Join(shims, cli), nil })
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
	calls := fakeGlabCalls(t, real)
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

// optionalSession runs the launcher to its end with a GitHub login never made
// and a glab issuer that fails, each optional as given, and returns its output,
// its exit code and the fake glab's directory. The gh beside that glab only
// records that it ran.
func optionalSession(t *testing.T, github, gitlab bool, script string) (string, int, string) {
	t.Helper()
	fakes := fakeGlab(t)
	gh := "#!/bin/sh\ntouch " + shellQuote(filepath.Join(fakes, "gh-called")) + "\n"
	if err := os.WriteFile(filepath.Join(fakes, "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := testConfig()
	c.SessionHours = 1
	c.GitHub.StateFile = filepath.Join(dir, "github.json")
	c.GitHub.Optional = github
	c.GitLab = glabConfig()
	c.GitLab.Optional = gitlab
	raw, _ := json.Marshal(c)
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal([]string{"--config", configPath, "run", "--", "/bin/sh", "-c", script})
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+t.TempDir(), "FAKE_GLAB_FAIL=1")
	output, err := launcher.CombinedOutput()
	if strings.Contains(string(output), "UNIX socket") {
		t.Skip("runtime prohibits UNIX sockets")
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(output), launcher.ProcessState.ExitCode(), fakes
}

// With every provider optional and failing, the harness still starts, and the
// wrappers and Git refuse instead of reaching the real CLIs or a prompt.
func TestOptionalProvidersAreSkipped(t *testing.T) {
	script := `gh api user; echo "gh exit $?"; glab api user; echo "glab exit $?"
if command -v git >/dev/null; then
	printf 'protocol=https\nhost=gitlab.com\npath=group/project.git\n\n' | git credential fill
fi
echo harness done`
	output, code, fakes := optionalSession(t, true, true, script)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	for _, want := range []string{
		"WARNING: GitHub is not available in this session: run github-login before starting a session. Fix: geas github-login\n",
		"WARNING: GitLab is not available in this session: glab issuer request failed. Fix: glab auth login\n",
		"Session ready without credentials",
		"geas: GitHub is not available in this session: run github-login before starting a session\ngh exit 1\n",
		"geas: GitLab is not available in this session: glab issuer request failed\nglab exit 1\n",
		"harness done\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if _, err := exec.LookPath("git"); err == nil && !strings.Contains(output, "told us to quit") {
		t.Errorf("Git did not stop at the helper:\n%s", output)
	}
	if strings.Contains(output, "glab-secret") {
		t.Errorf("issuer output leaked:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(fakes, "gh-called")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the gh wrapper reached the real gh")
	}
	// The issuer's lookup, and nothing after it: no PAT, so no reaper and no
	// revocation; the glab wrapper never reached the real glab.
	calls := fakeGlabCalls(t, fakes)
	if len(calls) != 1 || !slices.Equal(calls[0][3:], []string{"graphql", "-f", "query=" + lookupDocument(glabConfig())}) {
		t.Errorf("glab calls: %q", calls)
	}
}

// A required provider that fails stops the session before the harness.
func TestRequiredProviderFailureStopsSession(t *testing.T) {
	for _, tc := range []struct {
		github, gitlab bool
		want           string
		glabCalls      int
	}{
		{false, true, "geas: run github-login before starting a session\n", 0},
		{true, false, "geas: glab issuer request failed\n", 1},
	} {
		output, code, fakes := optionalSession(t, tc.github, tc.gitlab, "echo harness started")
		if code != 1 || !strings.HasSuffix(output, tc.want) || strings.Contains(output, "harness started") {
			t.Errorf("optional GitHub %v, GitLab %v: exit %d:\n%s", tc.github, tc.gitlab, code, output)
		}
		if calls := fakeGlabCalls(t, fakes); len(calls) != tc.glabCalls {
			t.Errorf("optional GitHub %v, GitLab %v: glab calls: %q", tc.github, tc.gitlab, calls)
		}
	}
}
