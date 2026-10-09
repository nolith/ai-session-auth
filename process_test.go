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
// main's own signal handling, by passing its arguments as JSON.
func TestMain(m *testing.M) {
	if raw, ok := os.LookupEnv("AGENT_AUTH_TEST_ARGS"); ok {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(2)
		}
		os.Args = append([]string{"agent-auth"}, args...)
		main()
	}
	os.Exit(m.Run())
}

// A closed terminal or pane takes the launcher's output with it, then sends
// SIGHUP: the harness must still be stopped and the session PAT revoked.
func TestHangupStopsHarnessAndRevokes(t *testing.T) {
	glab := fakeGlab(t)
	dir := t.TempDir()
	c := Config{SessionHours: 1, GitHub: GitHubConfig{Enabled: boolPointer(false)}, GitLab: glabConfig()}
	raw, _ := json.Marshal(c)
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "harness.pid")
	args, _ := json.Marshal([]string{"--config", config, "run", "--", "/bin/sh", "-c", "echo $$ >" + shellQuote(started) + "; exec sleep 30"})
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "AGENT_AUTH_TEST_ARGS="+string(args), "PATH="+glab+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+t.TempDir())
	launcher.Stdout, launcher.Stderr = write, write
	if err = launcher.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	done := make(chan error, 1)
	go func() { done <- launcher.Wait() }()
	deadline := time.After(20 * time.Second)
	for {
		if raw, err := os.ReadFile(started); err == nil && strings.HasSuffix(string(raw), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				// Only matters if the launcher failed to stop it.
				t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			}
			break
		}
		select {
		case err := <-done:
			output, _ := io.ReadAll(read)
			if strings.Contains(string(output), "UNIX socket") {
				t.Skip("runtime prohibits UNIX sockets")
			}
			t.Fatalf("launcher exited before the harness started: %v %s", err, output)
		case <-deadline:
			launcher.Process.Kill()
			t.Fatal("harness did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	read.Close()
	if err = launcher.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		launcher.Process.Kill()
		t.Fatal("launcher did not stop after SIGHUP")
	}
	// 130 is a handled interruption; a launcher killed by SIGHUP or SIGPIPE
	// reports -1.
	if code := launcher.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit %d (%v)", code, err)
	}
	calls := fakeGlabCalls(t, glab)
	if len(calls) == 0 || !slices.Equal(calls[len(calls)-1][3:], []string{"--method", "DELETE", "personal_access_tokens/42"}) {
		t.Fatalf("session PAT not revoked: %q", calls)
	}
}
