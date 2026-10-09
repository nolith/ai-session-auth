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
	t.Setenv("AI_AUTH_SOCKET", address)
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

// TestMain lets a test re-execute this binary as the launcher itself, with
// main's own signal handling, by passing its arguments as JSON. The launcher's
// reaper re-executes it too, as "_reaper".
func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv("AGENT_AUTH_TEST_ARGS"); ok {
		os.Unsetenv("AGENT_AUTH_TEST_ARGS")
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(2)
		}
		os.Args = append([]string{"agent-auth"}, args...)
		main()
	}
	if len(os.Args) > 1 && os.Args[1] == "_reaper" {
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
	c := Config{SessionHours: 1, GitHub: GitHubConfig{Enabled: boolPointer(false)}, GitLab: glabConfig()}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "harness.pid")
	args, _ := json.Marshal([]string{"--config", config, "run", "--", "/bin/sh", "-c", "echo $$ >" + shellQuote(started) + "; " + script})
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s.output = read
	s.launcher = exec.Command(os.Args[0])
	s.launcher.Env = append(os.Environ(), "AGENT_AUTH_TEST_ARGS="+string(args), "PATH="+s.glab+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+s.state)
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
	if _, err := os.Stat(filepath.Join(s.state, "ai-session-auth", "reaper.log")); !errors.Is(err, os.ErrNotExist) {
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
	reaperSession, _ := syscall.Getsid(reaper)
	launcherSession, _ := syscall.Getsid(launcher)
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
	log, err := os.ReadFile(filepath.Join(s.state, "ai-session-auth", "reaper.log"))
	if err != nil || !strings.Contains(string(log), "launcher gone without revoking; revoked") {
		t.Fatalf("reaper log: %q %v", log, err)
	}
}
