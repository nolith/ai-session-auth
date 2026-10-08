package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
