package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/testutil"
)

// shellQuote quotes a word for /bin/sh, for the harness scripts these tests
// write into a session.
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// TestMain lets a test re-execute this binary as the launcher itself, with
// main's own signal handling, by passing its arguments as JSON. The launcher
// re-executes it too, as "_reaper", and Git as "credential".
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
	// The launcher's own re-executions: the reaper, Git's credential helper
	// and the background sandbox check; and geas status, which a harness
	// runs as $GEAS_TEST_BINARY.
	if len(os.Args) > 1 && (strings.HasPrefix(os.Args[1], "_") || os.Args[1] == "credential" || os.Args[1] == "status" || os.Args[1] == "sandbox") {
		main()
	}
	os.Exit(m.Run())
}

type testSession struct {
	launcher *exec.Cmd
	done     chan error
	output   *os.File // read end of the launcher's stdout and stderr
	glab     string   // fake glab directory and call log
	state    string   // XDG_STATE_HOME: glab.lock, reaper.log and the ledger
	config   string   // the configuration path, on the reaper's command line
	started  string   // the harness writes its PID here once it runs
}

// launch runs the launcher with a fake glab issuer for gitlab.com around the
// harness script, which runs under /bin/sh, adding env to its environment.
func launch(t *testing.T, script string, env ...string) *testSession {
	t.Helper()
	return launchConfig(t, config.Config{SessionHours: 1, GitLab: []config.GitLabConfig{testutil.GlabConfig()}}, script, env...)
}

// launchConfig is launch with the configuration c.
func launchConfig(t *testing.T, c config.Config, script string, env ...string) *testSession {
	t.Helper()
	s := &testSession{glab: testutil.FakeGlab(t), state: t.TempDir(), done: make(chan error, 1)}
	dir := t.TempDir()
	raw, _ := json.Marshal(c)

	s.config = filepath.Join(dir, "config.json")
	if err := os.WriteFile(s.config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s.started = filepath.Join(dir, "harness.pid")
	args, _ := json.Marshal([]string{"--config", s.config, "run", "--", "/bin/sh", "-c", "echo $$ >" + shellQuote(s.started) + "; " + script})
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	s.output = read
	s.launcher = exec.Command(os.Args[0])
	s.launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+s.glab+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+s.state, "GEAS_TEST_BINARY="+os.Args[0])
	s.launcher.Env = append(s.launcher.Env, env...)
	s.launcher.Stdout, s.launcher.Stderr = write, write
	if err = s.launcher.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	go func() { s.done <- s.launcher.Wait() }()
	// Wait for the reaper too, so that the temporary directories outlive it.
	t.Cleanup(func() { waitReaper(t, s) })
	return s
}

// startSession launches a session and returns once the harness has started.
func startSession(t *testing.T, script string) *testSession {
	t.Helper()
	s := launch(t, script)
	deadline := time.After(20 * time.Second)
	for {
		if raw, err := os.ReadFile(s.started); err == nil && strings.HasSuffix(string(raw), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				// Only matters if nothing else stops it.
				t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			}
			return s
		}
		select {
		case err := <-s.done:
			// A harness that exits at once may finish before it is seen.
			if raw, _ := os.ReadFile(s.started); len(raw) > 0 {
				s.done <- err
				return s
			}
			output, _ := io.ReadAll(s.output)
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

// untilReady and untilSettled are harness script lines that wait for the
// background acquisition: GitLab's marker ready, then its PAT's ID in the
// ledger, after the id lookup.
const (
	untilReady   = `until grep -qs '"state":"ready"' "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com"; do sleep 0.02; done; `
	untilSettled = `until grep -qs '"id":42' "$GEAS_SESSION_DIR/ledger"; do sleep 0.02; done; `
)

// waitSettled waits until the session's ledger has the PAT's ID: the
// background acquisition has minted it and resolved its ID.
func waitSettled(t *testing.T, s *testSession) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if left := sessions(t, s); len(left) == 1 {
			raw, _ := os.ReadFile(filepath.Join(s.state, "geas", "sessions", left[0], "ledger"))
			if strings.Contains(string(raw), `"id":42`) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the PAT was never minted and resolved")
		}
	}
}

// reaperPID finds this session's reaper by the configuration path on its
// command line, which no other test shares.
func reaperPID(t *testing.T, s *testSession) int {
	t.Helper()
	output, err := exec.Command("ps", "-A", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "_reaper watch ") && strings.Contains(line, s.config+" ") {
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
	for _, call := range testutil.Calls(t, s.glab) {
		switch {
		case slices.Equal(call[3:], []string{"--method", "DELETE", "personal_access_tokens/42"}):
			count++
		case slices.Equal(call[3:], []string{"--method", "GET", "personal_access_tokens/42"}):
			checked = true
		}
	}
	return count, checked
}

// sessions lists the session directories left in the state directory.
func sessions(t *testing.T, s *testSession) []string {
	t.Helper()
	dirs, err := os.ReadDir(filepath.Join(s.state, "geas", "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, dir := range dirs {
		names = append(names, dir.Name())
	}
	return names
}

// A closed terminal or pane takes the launcher's output with it, then sends
// SIGHUP: the harness must still be stopped and the session PAT revoked.
func TestHangupStopsHarnessAndRevokes(t *testing.T) {
	s := startSession(t, "exec sleep 30")
	waitSettled(t, s)
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
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// The launcher revokes on a clean exit and tells the reaper, which does nothing.
func TestCleanExitRevokesOnce(t *testing.T) {
	s := startSession(t, untilSettled+"exit 0")
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
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// The harness finds its credentials in its session directory, in the files
// glab and the credential helper read, once GitLab's marker is ready, and
// the directory is gone after the session.
func TestHarnessSeesTheSessionDirectory(t *testing.T) {
	s := startSession(t, untilReady+`printf 'dir=%s\n' "$GEAS_SESSION_DIR"
printf 'glab=%s\n' "$GLAB_CONFIG_DIR"
printf 'secret=%s\n' "$(cat "$GEAS_SESSION_DIR/secrets/gitlab:gitlab.com")"
printf 'marker=%s\n' "$(cat "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com")"
grep -c 'token: "session-secret"' "$GLAB_CONFIG_DIR/config.yml"
ls -ld "$GEAS_SESSION_DIR" "$GLAB_CONFIG_DIR/config.yml" | cut -c1-10
printf 'profile=%s\n' "$GEAS_SANDBOX_PROFILE"
grep -c '"bypass_protection"' "$GEAS_SANDBOX_PROFILE"
"$GEAS_TEST_BINARY" status; echo "status exit $?"`)
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	left := sessions(t, s)
	if len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
	dir := filepath.Join(s.state, "geas", "sessions")
	for _, want := range []string{"\ndir=" + dir + "/", "\nglab=" + dir + "/", "/glab\n", "\nsecret=session-secret\n", `marker={"state":"ready"`, "\n1\n", "drwx------\n-rw-------\n", "/sandbox/nono.json\n1\n", "\ngitlab:gitlab.com  ready\nstatus exit 0\n"} {
		if !strings.Contains("\n"+output, want) {
			t.Errorf("harness output lacks %q:\n%s", want, output)
		}
	}
}

// SIGKILL, as from a herdr pane that closes, leaves no chance to revoke: the
// reaper, outside the launcher's session and process tree, must do it.
func TestKilledLauncherIsReaped(t *testing.T) {
	s := startSession(t, "exec sleep 30")
	waitSettled(t, s)
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
	if err != nil || !strings.Contains(string(log), "(ID 42): launcher gone without revoking; revoked") {
		t.Fatalf("reaper log: %q %v", log, err)
	}
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// SIGKILL between "name written" and "mint returned": the ledger holds the
// PAT's name and no ID yet, and the reaper finds the PAT by that name.
func TestLauncherKilledDuringMintIsReaped(t *testing.T) {
	s := launch(t, "exec sleep 30", "FAKE_GLAB_BLOCK=1")
	release := filepath.Join(s.glab, "release")
	// Before waitReaper, which needs the mint out of the way.
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.glab, "mutation")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mutation never reached the issuer")
		}
	}
	name, err := os.ReadFile(filepath.Join(s.glab, "name"))
	if err != nil || len(name) == 0 {
		t.Fatalf("minted name: %q %v", name, err)
	}
	left := sessions(t, s)
	if len(left) != 1 {
		t.Fatalf("session directories: %v", left)
	}
	raw, err := os.ReadFile(filepath.Join(s.state, "geas", "sessions", left[0], "ledger"))
	var entry ledger.Entry
	if err != nil || json.Unmarshal(raw, &entry) != nil || strings.Count(string(raw), "\n") != 1 || entry.Name != string(name) || entry.ID != 0 || entry.Revoked {
		t.Fatalf("ledger during the mint: %q %v", raw, err)
	}
	if reaperPID(t, s) == 0 {
		t.Fatal("no reaper before the mint")
	}
	if err = s.launcher.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	s.wait(t)
	// The mint lands after the launcher is gone, as GitLab would complete it.
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitReaper(t, s)
	calls := testutil.Calls(t, s.glab)
	var after [][]string
	for i, call := range calls {
		if len(call) > 4 && strings.HasPrefix(call[len(call)-1], "input=") {
			after = calls[i+1:]
		}
	}
	if len(after) != 3 || after[0][len(after[0])-1] != "query=query SessionPAT {\n  currentUser { personalAccessTokens(search: \""+string(name)+"\") { nodes { id name } } }\n}\n" {
		t.Fatalf("calls after the mint: %q", after)
	}
	if count, checked := deletes(t, s); count != 1 || !checked {
		t.Fatalf("%d DELETE calls, reaper checked: %v", count, checked)
	}
	log, err := os.ReadFile(filepath.Join(s.state, "geas", "reaper.log"))
	if err != nil || !strings.Contains(string(log), " "+string(name)+": launcher gone without revoking; revoked") {
		t.Fatalf("reaper log: %q %v", log, err)
	}
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// failingSession runs the launcher to its end with a GitHub login never made
// and a glab issuer that fails, and returns its output, its exit code and the
// fake glab's directory. The gh beside that glab only records that it ran.
func failingSession(t *testing.T, script string) (string, int, string) {
	t.Helper()
	fakes := testutil.FakeGlab(t)
	gh := "#!/bin/sh\necho \"GH_CONFIG_DIR=$GH_CONFIG_DIR GH_TOKEN=${GH_TOKEN-unset}\" >" + shellQuote(filepath.Join(fakes, "gh-called")) + "\n"
	if err := os.WriteFile(filepath.Join(fakes, "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := testutil.Config()
	c.SessionHours = 1
	c.GitHub[0].StateFile = filepath.Join(dir, "github.json")
	c.GitLab = []config.GitLabConfig{testutil.GlabConfig()}
	raw, _ := json.Marshal(c)
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal([]string{"--config", configPath, "run", "--", "/bin/sh", "-c", script})
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+t.TempDir(), "FAKE_GLAB_FAIL=1", "GEAS_TEST_BINARY="+os.Args[0])
	output, err := launcher.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(output), launcher.ProcessState.ExitCode(), fakes
}

// A provider that fails never stops the start: the harness runs, and the
// failure goes to the provider's marker, never to the terminal. gh and glab
// are the real ones, configured from the session directory, where no token
// arrived, and Git refuses instead of reaching a prompt.
func TestFailingProvidersNeverStopTheStart(t *testing.T) {
	script := `until grep -qs failed "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com" && grep -qs failed "$GEAS_SESSION_DIR/markers/github:github.com"; do sleep 0.02; done
echo harness started
cat "$GEAS_SESSION_DIR/markers/github:github.com"; echo
"$GEAS_TEST_BINARY" status; echo "status exit $?"
gh api user; echo "gh exit $?"
case "$GLAB_CONFIG_DIR" in "$GEAS_SESSION_DIR"/glab) echo "glab configured in the session" ;; esac
[ -e "$GLAB_CONFIG_DIR/config.yml" ] || echo "no glab token"
if command -v git >/dev/null; then
	printf 'protocol=https\nhost=gitlab.com\npath=group/project.git\n\n' | git credential fill
fi
echo harness done`
	output, code, fakes := failingSession(t, script)
	if code != 0 || !strings.HasPrefix(output, "harness started\n") {
		t.Fatalf("exit %d: %s", code, output)
	}
	for _, want := range []string{
		`{"state":"failed","reason":"run github-login before starting a session","time":`,
		"github:github.com  failed: run github-login before starting a session\ngitlab:gitlab.com  failed: glab issuer request failed\nstatus exit 1\n",
		"gh exit 0\n",
		"glab configured in the session\n",
		"no glab token\n",
		"harness done\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output lacks %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "WARNING") || strings.Contains(output, "Session ready") {
		t.Errorf("the launcher printed while the harness ran:\n%s", output)
	}
	if _, err := exec.LookPath("git"); err == nil && (!strings.Contains(output, "told us to quit") || !strings.Contains(output, "gitlab.com is not available in this session: glab issuer request failed")) {
		t.Errorf("Git did not stop at the helper:\n%s", output)
	}
	if strings.Contains(output, "glab-secret") {
		t.Errorf("issuer output leaked:\n%s", output)
	}
	// The real gh ran with the session's configuration directory and no
	// token of yours.
	raw, err := os.ReadFile(filepath.Join(fakes, "gh-called"))
	if err != nil || !strings.Contains(string(raw), "/geas/sessions/") || !strings.HasSuffix(string(raw), "/gh GH_TOKEN=unset\n") {
		t.Errorf("gh: %q %v", raw, err)
	}
	// The issuer's lookup, and nothing after it: no PAT, so nothing to
	// revoke. The lookup document itself is pinned in internal/provider/gitlab.
	calls := testutil.Calls(t, fakes)
	if len(calls) != 1 || len(calls[0]) != 6 || calls[0][3] != "graphql" || !strings.HasPrefix(calls[0][5], "query=query SessionLookup") {
		t.Errorf("glab calls: %q", calls)
	}
}

// The harness starts before any network call, and the timing log says so:
// "harness start" comes first, the providers' steps are held while the
// harness runs, and print once it has exited.
func TestTimingLogStartsTheHarnessFirst(t *testing.T) {
	s := launch(t, untilSettled+"echo harness ran", "GEAS_DEBUG=timing")
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	order := []string{"geas timing: config load", "geas timing: plan", "geas timing: reaper", "geas timing: harness start", "harness ran\n", "geas timing: gitlab:gitlab.com lookup", "geas timing: gitlab:gitlab.com mint", "geas timing: gitlab:gitlab.com ready", "geas timing: gitlab:gitlab.com id lookup"}
	last := -1
	for _, want := range order {
		at := strings.Index(output, want)
		if at <= last {
			t.Fatalf("%q missing or out of order:\n%s", want, output)
		}
		last = at
	}
}

// A harness that exits while the mint is in flight: the mint runs to its
// answer, and the launcher revokes the PAT before it exits, by the name in
// the ledger.
func TestHarnessExitDuringMintRevokes(t *testing.T) {
	// The fake glab is first on the harness's PATH, and its directory holds
	// the "mutation" file once the mint has reached it.
	s := launch(t, `until [ -e "$(dirname "$(command -v glab)")/mutation" ]; do sleep 0.02; done; exit 3`, "FAKE_GLAB_BLOCK=1")
	release := filepath.Join(s.glab, "release")
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.glab, "mutation")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mutation never reached the issuer")
		}
	}
	select {
	case err := <-s.done:
		t.Fatalf("the launcher exited during the mint: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, s.output)
	s.wait(t)
	if code := s.launcher.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("exit %d", code)
	}
	waitReaper(t, s)
	if count, checked := deletes(t, s); count != 1 || checked {
		t.Fatalf("%d DELETE calls, reaper checked: %v", count, checked)
	}
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// fill asks Git for gitlab.com's credential, as a push would, the moment the
// harness starts.
const fill = `printf 'protocol=https\nhost=gitlab.com\npath=group/project.git\n\n' | git credential fill; echo "fill exit $?"`

// Git asking right after the start, while the mint is still on its way,
// waits for it in the helper and gets the session PAT.
func TestGitWaitsForASlowIssuer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	s := launch(t, `cat "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com"; echo; `+fill, "FAKE_GLAB_DELAY=1")
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	if !strings.Contains(output, `{"state":"pending"`) || !strings.Contains(output, "username=oauth2\npassword=session-secret\n") || !strings.Contains(output, "fill exit 0\n") {
		t.Fatalf("harness output:\n%s", output)
	}
}

// With an issuer that fails, the same request fails as soon as the marker
// says so, with the reason, long before the helper's wait is over.
func TestGitFailsFastForAFailingIssuer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	started := time.Now()
	s := launch(t, fill, "FAKE_GLAB_FAIL=1")
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	if !strings.Contains(output, "gitlab.com is not available in this session: glab issuer request failed") || !strings.Contains(output, "fill exit 128\n") || strings.Contains(output, "password=") {
		t.Fatalf("harness output:\n%s", output)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("the helper waited %s", took)
	}
}

// Inside a session, `geas run` runs the command in place, sharing the
// session: no configuration is read, nothing is minted.
func TestNestedRunSharesTheSession(t *testing.T) {
	dir := testutil.SessionDir(t)
	out := filepath.Join(t.TempDir(), "out")
	args, _ := json.Marshal([]string{"--config", "/nonexistent/config.json", "run", "--", "/bin/sh", "-c", "echo \"$GEAS_SESSION_DIR\" >" + shellQuote(out) + "; exit 7"})
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "GEAS_SESSION_DIR="+dir.Path())
	output, _ := launcher.CombinedOutput()
	if code := launcher.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("exit %d: %s", code, output)
	}
	if raw, err := os.ReadFile(out); err != nil || string(raw) != dir.Path()+"\n" {
		t.Fatalf("harness saw %q %v", raw, err)
	}
	// A GEAS_SESSION_DIR naming no session is refused, not trusted.
	launcher = exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "GEAS_SESSION_DIR="+filepath.Join(t.TempDir(), "20261022T213405Z-0011223344556677"))
	output, _ = launcher.CombinedOutput()
	if code := launcher.ProcessState.ExitCode(); code != 125 || !strings.Contains(string(output), "unset GEAS_SESSION_DIR to start a new session") {
		t.Fatalf("stale session: exit %d: %s", code, output)
	}
}

// runGeas runs the launcher to its end with args, in a state directory of
// its own unless env names one, and returns its output and exit code.
func runGeas(t *testing.T, args []string, env ...string) (string, int) {
	t.Helper()
	raw, _ := json.Marshal(args)
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(raw), "XDG_STATE_HOME="+t.TempDir())
	launcher.Env = append(launcher.Env, env...)
	output, err := launcher.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(output), launcher.ProcessState.ExitCode()
}

// geas run exits 125, with the error and nothing else, when it fails before
// the harness starts; once the harness has started, its exit code is the
// harness's, 125 included, and geas prints nothing of its own.
func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	// GitHub alone, with no login: it fails in the background, offline.
	c := testutil.Config()
	c.SessionHours = 1
	c.GitLab = nil
	c.GitHub[0].StateFile = filepath.Join(dir, "github.json")

	raw, _ := json.Marshal(c)
	good := filepath.Join(dir, "config.json")
	invalid := filepath.Join(dir, "invalid.json")
	if os.WriteFile(good, raw, 0600) != nil || os.WriteFile(invalid, []byte(`{"version": 2, "session_hours": 1, "unknown": true}`), 0600) != nil {
		t.Fatal("cannot write the configurations")
	}
	notADirectory := filepath.Join(dir, "state-file")
	os.WriteFile(notADirectory, nil, 0600)
	for _, tc := range []struct {
		name   string
		args   []string
		env    []string
		code   int
		output string
	}{
		{"no configuration", []string{"--config", filepath.Join(dir, "missing.json"), "run", "--", "true"}, nil, 125, "geas: cannot read configuration\n"},
		{"invalid configuration", []string{"--config", invalid, "run", "--", "true"}, nil, 125, "geas: unknown field \"unknown\"\n"},
		{"no harness", []string{"--config", good, "run", "--"}, nil, 125, "geas: specify a harness command after run --\n"},
		{"no session directory", []string{"--config", good, "run", "--", "true"}, []string{"XDG_STATE_HOME=" + notADirectory}, 125, "geas: local file or permission error\n"},
		{"harness not found", []string{"--config", good, "run", "--", filepath.Join(dir, "no-such-harness")}, nil, 125, "geas: cannot start harness; check executable and arguments\n"},
		{"harness exit code", []string{"--config", good, "run", "--", "/bin/sh", "-c", "echo harness; exit 42"}, nil, 42, "harness\n"},
		{"harness exits 125", []string{"--config", good, "run", "--", "/bin/sh", "-c", "exit 125"}, nil, 125, ""},
		// Outside run, a configuration error is still 1.
		{"github-login", []string{"--config", invalid, "github-login"}, nil, 1, "geas: unknown field \"unknown\"\n"},
	} {
		output, code := runGeas(t, tc.args, tc.env...)
		if code != tc.code || output != tc.output {
			t.Errorf("%s: exit %d, output %q; want %d, %q", tc.name, code, output, tc.code, tc.output)
		}
	}
}

// Two GitLab instances, each through the user's own glab login for its
// host: both mint in the background, glab's config.yml has an entry for
// each, Git gets each host's PAT from the helper, and both are revoked.
func TestTwoGitLabInstances(t *testing.T) {
	second := testutil.GlabConfig()
	second.Host = "gitlab.example.com"
	c := config.Config{SessionHours: 1, GitLab: []config.GitLabConfig{testutil.GlabConfig(), second}}
	script := `until grep -qs '"state":"ready"' "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com" && grep -qs '"state":"ready"' "$GEAS_SESSION_DIR/markers/gitlab:gitlab.example.com"; do sleep 0.02; done
until [ "$(grep -c '"id":42' "$GEAS_SESSION_DIR/ledger")" = 2 ]; do sleep 0.02; done
printf 'glab=%s\n' "$GLAB_CONFIG_DIR"
grep -E '^    [a-z.]+:$|^host:' "$GLAB_CONFIG_DIR/config.yml"
"$GEAS_TEST_BINARY" status; echo "status exit $?"
if command -v git >/dev/null; then
	printf 'protocol=https\nhost=gitlab.example.com\npath=group/project.git\n\n' | git credential fill
fi`
	s := launchConfig(t, c, script)
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	for _, want := range []string{
		"/glab\nhost: gitlab.com\n    gitlab.com:\n    gitlab.example.com:\n",
		"gitlab:gitlab.com          ready\ngitlab:gitlab.example.com  ready\nstatus exit 0\n",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("harness output lacks %q:\n%s", want, output)
		}
	}
	if _, err := exec.LookPath("git"); err == nil && !strings.Contains(output, "username=oauth2\npassword=session-secret\n") {
		t.Errorf("Git got no PAT for the second instance:\n%s", output)
	}
	waitReaper(t, s)
	// Every call is pinned to one of the two hosts, and each host minted,
	// resolved and revoked its own PAT.
	hosts := map[string][]string{}
	for _, call := range testutil.Calls(t, s.glab) {
		if len(call) < 4 || call[0] != "api" || call[1] != "--hostname" {
			t.Fatalf("call not pinned to a host: %q", call)
		}
		last := call[len(call)-1]
		step := last
		switch {
		case strings.HasPrefix(last, "query=query SessionLookup"):
			step = "lookup"
		case strings.HasPrefix(last, "input="):
			step = "mint"
		case strings.HasPrefix(last, "query=query SessionPAT"):
			step = "id lookup"
		}
		hosts[call[2]] = append(hosts[call[2]], step)
	}
	want := []string{"lookup", "mint", "id lookup", "personal_access_tokens/42"}
	if len(hosts) != 2 || !slices.Equal(hosts["gitlab.com"], want) || !slices.Equal(hosts["gitlab.example.com"], want) {
		t.Fatalf("calls by host: %q", hosts)
	}
	if count, _ := deletes(t, s); count != 2 {
		t.Fatalf("%d DELETE calls", count)
	}
	if left := sessions(t, s); len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
}

// fakeWrapper writes a wrapper that records its arguments and the session's
// variables in log, one line each, then runs its arguments after its own
// options, such as "--profile agent".
func fakeWrapper(t *testing.T, log string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wrapper")
	script := "#!/bin/sh\nprintf 'wrapper %s|%s|%s\\n' \"$*\" \"$GEAS_SESSION_DIR\" \"$GEAS_SANDBOX_PROFILE\" >>" + shellQuote(log) + "\nwhile [ \"${1#--}\" != \"$1\" ]; do shift 2; done\nexec \"$@\"\n"

	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The harness starts through the wrapper, with the session's variables
// already set. A harness that exits non-zero at once gets the wrapper's
// hint, after everything else geas prints; a clean exit gets none.
func TestWrapperAndEarlyExitHint(t *testing.T) {
	log := filepath.Join(t.TempDir(), "wrapper.log")
	c := config.Config{SessionHours: 1, GitLab: []config.GitLabConfig{testutil.GlabConfig()}, Wrapper: &config.Wrapper{
		Command:       []string{fakeWrapper(t, log), "--profile", "agent"},
		EarlyExitHint: "skip the sandbox with SANDBOX=off",
	}}
	s := launchConfig(t, c, untilSettled+"echo harness ran; exit 3")
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	if code := s.launcher.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("exit %d", code)
	}
	if output := string(raw); !strings.HasPrefix(output, "harness ran\n") || !strings.HasSuffix(output, "GitLab session PAT revoked.\nskip the sandbox with SANDBOX=off\n") {
		t.Fatalf("output:\n%s", output)
	}
	// The harness's line; the background sandbox check runs through the
	// wrapper too.
	logged, err := os.ReadFile(log)
	var harness []string
	for _, line := range strings.Split(strings.TrimSuffix(string(logged), "\n"), "\n") {
		if !strings.HasSuffix(strings.Split(line, "|")[0], " sandbox check") {
			harness = append(harness, line)
		}
	}
	if err != nil || len(harness) != 1 {
		t.Fatalf("wrapper log: %q %v", logged, err)
	}
	fields := strings.Split(harness[0], "|")
	if len(fields) != 3 || !strings.HasPrefix(fields[0], "wrapper --profile agent /bin/sh -c ") || !strings.Contains(fields[1], "/geas/sessions/") || fields[2] != fields[1]+"/sandbox/nono.json" {
		t.Fatalf("the wrapper saw %q", harness[0])
	}

	// A clean exit: no hint.
	s = launchConfig(t, c, untilSettled+"exit 0")
	raw, _ = io.ReadAll(s.output)
	s.wait(t)
	if output := string(raw); s.launcher.ProcessState.ExitCode() != 0 || strings.Contains(output, "SANDBOX=off") {
		t.Fatalf("clean exit: %d:\n%s", s.launcher.ProcessState.ExitCode(), output)
	}
	// A wrapper that cannot start is geas's failure, before the harness.
	c.Wrapper.Command = []string{filepath.Join(t.TempDir(), "no-such-wrapper")}
	raw, _ = json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, raw, 0600)
	output, code := runGeas(t, []string{"--config", path, "run", "--", "true"}, "PATH="+testutil.FakeGlab(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code != 125 || output != "geas: cannot start the wrapper; check wrapper.command\n" {
		t.Fatalf("no wrapper: exit %d: %q", code, output)
	}
}

// untilSandboxChecked is a harness script line that waits for the
// background sandbox check's verdict, and for GitLab's marker.
const untilSandboxChecked = `until grep -qs '"state":"ready"' "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com" && ! grep -qs pending "$GEAS_SESSION_DIR/markers/sandbox"; do sleep 0.02; done; `

// With a wrapper, geas runs geas sandbox check through it in the
// background, and geas status shows the verdict: here no sandbox at all,
// so geas's state is readable and the check fails, naming it. Nothing the
// check or the wrapper prints reaches the terminal.
func TestBackgroundSandboxCheck(t *testing.T) {
	log := filepath.Join(t.TempDir(), "wrapper.log")
	wrapper := fakeWrapper(t, log)
	noisy := filepath.Join(t.TempDir(), "noisy-wrapper")
	script := "#!/bin/sh\necho wrapper noise; echo wrapper noise >&2\nexec " + shellQuote(wrapper) + " \"$@\"\n"
	if err := os.WriteFile(noisy, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := config.Config{SessionHours: 1, GitLab: []config.GitLabConfig{testutil.GlabConfig()}, Wrapper: &config.Wrapper{Command: []string{noisy}}}
	// No glab login to find: geas's own state is the one readable path.
	s := launchConfig(t, c, untilSandboxChecked+`"$GEAS_TEST_BINARY" status; echo "status exit $?"`, "GLAB_CONFIG_DIR=/nonexistent/glab-cli")
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	want := "wrapper noise\nwrapper noise\ngitlab:gitlab.com  ready\nsandbox            failed: readable: " + filepath.Join(s.state, "geas") + "\nstatus exit 1\n"
	if !strings.HasPrefix(output, want) || strings.Count(output, "wrapper noise") != 2 {
		t.Fatalf("output:\n%s\nwant it to start with:\n%s", output, want)
	}
	// The harness and the check, both through the wrapper, in the session.
	logged, err := os.ReadFile(log)
	if err != nil || strings.Count(string(logged), "\n") != 2 || !strings.Contains(string(logged), "/"+filepath.Base(os.Args[0])+" sandbox check|") {
		t.Fatalf("wrapper log: %q %v", logged, err)
	}
	// A wrapper whose sandbox passes the check: ready.
	passing := filepath.Join(t.TempDir(), "passing-wrapper")
	script = "#!/bin/sh\ncase \"$*\" in *' sandbox check') echo 'sandbox check: 0 of 2 protected paths readable (1 absent); the session directory is readable'; exit 0 ;; esac\nexec \"$@\"\n"
	if err := os.WriteFile(passing, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Wrapper.Command = []string{passing}
	s = launchConfig(t, c, untilSandboxChecked+`"$GEAS_TEST_BINARY" status; echo "status exit $?"`)
	raw, _ = io.ReadAll(s.output)
	s.wait(t)
	if output := string(raw); !strings.HasPrefix(output, "gitlab:gitlab.com  ready\nsandbox            ready\nstatus exit 0\n") {
		t.Fatalf("passing sandbox:\n%s", output)
	}
}

// no_new_privs: a configuration error outside Linux, exit code 125; on
// Linux, geas built without cgo, as the flake builds it, runs the harness
// with the flag set, as /proc says.
func TestNoNewPrivs(t *testing.T) {
	dir := t.TempDir()
	c := testutil.Config()
	c.SessionHours, c.NoNewPrivs, c.GitLab = 1, true, nil
	c.GitHub[0].StateFile = filepath.Join(dir, "github.json")
	raw, _ := json.Marshal(c)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		output, code := runGeas(t, []string{"--config", path, "run", "--", "true"})
		if code != 125 || output != "geas: no_new_privs is supported on Linux only, not on "+runtime.GOOS+": leave it out\n" {
			t.Fatalf("exit %d: %q", code, output)
		}
		return
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go to build geas without cgo")
	}
	binary := filepath.Join(dir, "geas")
	build := exec.Command(goTool, "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	launcher := exec.Command(binary, "--config", path, "run", "--", "/bin/sh", "-c", "grep NoNewPrivs /proc/self/status")
	launcher.Env = append(os.Environ(), "XDG_STATE_HOME="+t.TempDir())
	output, err := launcher.CombinedOutput()
	if err != nil || string(output) != "NoNewPrivs:\t1\n" {
		t.Fatalf("%v: %q", err, output)
	}
}
