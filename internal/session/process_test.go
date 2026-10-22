package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	"github.com/nolith/geas/internal/sandbox"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

func TestSessionEnvironmentAndGitRewrites(t *testing.T) {
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
	values, err := sessionEnvironment(base, session, map[string]string{"GLAB_CONFIG_DIR": session.Path() + "/glab"})
	if err != nil {
		t.Fatal(err)
	}
	env := procenv.Map(values)
	if env["GEAS_SESSION_DIR"] != session.Path() || env["GLAB_CONFIG_DIR"] != session.Path()+"/glab" || env["GIT_TERMINAL_PROMPT"] != "0" || env["GEAS_SANDBOX_PROFILE"] != session.Path()+"/sandbox/nono.json" {
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
	// The tools on PATH are the real ones.
	if env["PATH"] != procenv.Map(base)["PATH"] {
		t.Fatalf("PATH changed: %s", env["PATH"])
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

// The policy denies what the providers protect and geas's state as a whole,
// and re-opens the session's directory; the nono fragment says the same.
func TestWriteSandbox(t *testing.T) {
	session := testutil.SessionDir(t)
	t.Setenv("GLAB_CONFIG_DIR", "/elsewhere/glab")
	t.Setenv("GH_CONFIG_DIR", "/elsewhere/gh")
	c := testutil.Config()
	c.GitHub.StateFile = "/state/github.json"
	c.GitLab.IssuerTokenFile = "/state/issuer.txt"
	if err := writeSandbox(session, []provider.Provider{&gitlab.Provider{Config: c.GitLab}, github.New(c.GitHub, nil)}); err != nil {
		t.Fatal(err)
	}
	state, _ := secretfile.StateDirectory()
	policy, err := sandbox.Load(session)
	want := []string{"/elsewhere/glab", "/state/issuer.txt", "/state/github.json", "/state/github.json.lock", "/elsewhere/gh", state}
	if err != nil || !slices.Equal(policy.Deny, want) || !slices.Equal(policy.Read, []string{session.Path()}) || !slices.Equal(policy.Reopen, []string{session.Path()}) {
		t.Fatalf("%+v %v", policy, err)
	}
	raw, err := session.ReadFile("sandbox/nono.json")
	if err != nil || !strings.Contains(string(raw), `"bypass_protection": [`) || strings.Contains(string(raw), "extends") {
		t.Fatalf("%s %v", raw, err)
	}
}

// refreshing is a GitHub provider whose every Refresh hands out a later
// token, as GitHub's would near expiry.
type refreshing struct {
	*github.Provider
	calls chan int
	n     int
}

func (r *refreshing) Refresh(_ context.Context, c provider.Credential) (provider.Credential, error) {
	r.n++
	r.calls <- r.n
	return provider.Credential{Secret: fmt.Sprintf("ghu_%d", r.n), User: c.User, Expires: time.Now().Add(time.Hour)}, nil
}

// keepFresh rewrites the token before it expires, and stops with its
// context.
func TestKeepFresh(t *testing.T) {
	margin, retry := refreshMargin, refreshRetry
	refreshMargin, refreshRetry = time.Hour-50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { refreshMargin, refreshRetry = margin, retry })
	session := testutil.SessionDir(t)
	store := newCredentials(session)
	p := &refreshing{Provider: github.New(testutil.Config().GitHub, nil), calls: make(chan int, 10)}
	first := provider.Credential{Secret: "ghu_0", User: "alice", Expires: time.Now().Add(time.Hour)}
	if err := store.put(p, first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { keepFresh(ctx, store, p, first, systemClock); close(done) }()
	for want := 1; want <= 2; want++ {
		select {
		case n := <-p.calls:
			if n != want {
				t.Fatalf("refresh %d, want %d", n, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no refresh")
		}
	}
	cancel()
	<-done
	raw, err := session.ReadFile("gh/hosts.yml")
	if err != nil || (!strings.Contains(string(raw), `"ghu_1"`) && !strings.Contains(string(raw), `"ghu_2"`)) {
		t.Fatalf("hosts.yml: %s %v", raw, err)
	}
	if secret, _ := session.ReadFile("secrets/github:github.com"); string(secret) == "ghu_0" {
		t.Fatal("secret not refreshed")
	}
}

// fakeClock moves only when keepFresh waits, by as long as it waits.
type fakeClock struct{ now time.Time }

func (f *fakeClock) clock() clock {
	return clock{now: func() time.Time { return f.now }, sleep: func(ctx context.Context, d time.Duration) error {
		f.now = f.now.Add(d)
		return ctx.Err()
	}}
}

// scripted is a GitHub provider whose Refresh answers from a script, one
// entry per call, and records the marker the helper would see at each call.
type scripted struct {
	*github.Provider
	dir     *sessiondir.Dir
	clock   *fakeClock
	answers []error // nil hands out a token valid for eight more hours
	seen    []string
	stop    context.CancelFunc
}

func (s *scripted) Refresh(_ context.Context, c provider.Credential) (provider.Credential, error) {
	m, _ := s.dir.Marker(s.ID())
	s.seen = append(s.seen, m.State+" "+m.Reason)
	if len(s.seen) > len(s.answers) {
		s.stop()
		return provider.Credential{}, errors.New("stopped")
	}
	if err := s.answers[len(s.seen)-1]; err != nil {
		return provider.Credential{}, err
	}
	return provider.Credential{Secret: fmt.Sprintf("ghu_%d", len(s.seen)), User: c.User, Expires: s.clock.now.Add(8 * time.Hour)}, nil
}

// With a fake clock: a refresh that fails while the token is valid leaves
// the marker ready and retries every minute; once the token has expired the
// marker says why; the next token makes it ready again.
func TestKeepFreshMarksAnExpiredCredential(t *testing.T) {
	session := testutil.SessionDir(t)
	store := newCredentials(session)
	start := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start}
	down := errors.New("GitHub refresh failed; reauthorize with github-login")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &scripted{Provider: github.New(testutil.Config().GitHub, nil), dir: session, clock: clk, stop: cancel,
		// At 7:56, 7:57, 7:58, 7:59 and 8:00, then 8:01.
		answers: []error{down, down, down, down, down, nil}}
	first := provider.Credential{Secret: "ghu_0", User: "alice", Expires: start.Add(8 * time.Hour)}
	if err := store.put(p, first); err != nil {
		t.Fatal(err)
	}
	if err := mark(session, p.ID(), sessiondir.Ready, ""); err != nil {
		t.Fatal(err)
	}
	keepFresh(ctx, store, p, first, clk.clock())
	expired := "failed credential expired, and refreshing it failed: " + down.Error()
	want := []string{"ready ", "ready ", "ready ", "ready ", "ready ", expired, "ready "}
	if !slices.Equal(p.seen, want) {
		t.Fatalf("markers at each refresh:\n%q\nwant\n%q", p.seen, want)
	}
	// The last wait is until four minutes before the new token expires.
	if want := start.Add(8*time.Hour + time.Minute + 8*time.Hour - refreshMargin); !clk.now.Equal(want) {
		t.Fatalf("clock at %s, want %s", clk.now, want)
	}
	raw, err := session.ReadFile("gh/hosts.yml")
	if err != nil || !strings.Contains(string(raw), `"ghu_6"`) {
		t.Fatalf("hosts.yml: %s %v", raw, err)
	}
	if secret, _ := session.ReadFile("secrets/github:github.com"); string(secret) != "ghu_6" {
		t.Fatalf("secret %q", secret)
	}
}
