package main

import (
	"encoding/json"
	"errors"
	"io"
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
	// The launcher's own re-executions: the reaper, and Git's credential
	// helper.
	if len(os.Args) > 1 && (strings.HasPrefix(os.Args[1], "_") || os.Args[1] == "credential") {
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

// launch runs the launcher with a fake glab issuer around the harness script,
// which runs under /bin/sh, adding env to its environment.
func launch(t *testing.T, script string, env ...string) *testSession {
	t.Helper()
	s := &testSession{glab: testutil.FakeGlab(t), state: t.TempDir(), done: make(chan error, 1)}
	dir := t.TempDir()
	c := config.Config{SessionHours: 1, GitHub: config.GitHubConfig{Enabled: testutil.Bool(false)}, GitLab: testutil.GlabConfig()}
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
	s.launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+s.glab+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+s.state)
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
grep -c '"bypass_protection"' "$GEAS_SANDBOX_PROFILE"`)
	raw, _ := io.ReadAll(s.output)
	s.wait(t)
	output := string(raw)
	left := sessions(t, s)
	if len(left) != 0 {
		t.Fatalf("session directories left: %v", left)
	}
	dir := filepath.Join(s.state, "geas", "sessions")
	for _, want := range []string{"\ndir=" + dir + "/", "\nglab=" + dir + "/", "/glab\n", "\nsecret=session-secret\n", `marker={"state":"ready"`, "\n1\n", "drwx------\n-rw-------\n", "/sandbox/nono.json\n1\n"} {
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
// and a glab issuer that fails, each optional as given, and returns its output,
// its exit code and the fake glab's directory. The gh beside that glab only
// records that it ran.
func failingSession(t *testing.T, optional bool, script string) (string, int, string) {
	t.Helper()
	fakes := testutil.FakeGlab(t)
	gh := "#!/bin/sh\necho \"GH_CONFIG_DIR=$GH_CONFIG_DIR GH_TOKEN=${GH_TOKEN-unset}\" >" + shellQuote(filepath.Join(fakes, "gh-called")) + "\n"
	if err := os.WriteFile(filepath.Join(fakes, "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c := testutil.Config()
	c.SessionHours = 1
	c.GitHub.StateFile = filepath.Join(dir, "github.json")
	c.GitHub.Optional = optional
	c.GitLab = testutil.GlabConfig()
	c.GitLab.Optional = optional
	raw, _ := json.Marshal(c)
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal([]string{"--config", configPath, "run", "--", "/bin/sh", "-c", script})
	launcher := exec.Command(os.Args[0])
	launcher.Env = append(os.Environ(), "GEAS_TEST_ARGS="+string(args), "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"), "XDG_STATE_HOME="+t.TempDir(), "FAKE_GLAB_FAIL=1")
	output, err := launcher.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return string(output), launcher.ProcessState.ExitCode(), fakes
}

// A provider that fails never stops the start, optional or not: the harness
// runs, and the failure goes to the provider's marker, never to the
// terminal. gh and glab are the real ones, configured from the session
// directory, where no token arrived, and Git refuses instead of reaching a
// prompt.
func TestFailingProvidersNeverStopTheStart(t *testing.T) {
	script := `until grep -qs failed "$GEAS_SESSION_DIR/markers/gitlab:gitlab.com" && grep -qs failed "$GEAS_SESSION_DIR/markers/github:github.com"; do sleep 0.02; done
echo harness started
cat "$GEAS_SESSION_DIR/markers/github:github.com"; echo
gh api user; echo "gh exit $?"
case "$GLAB_CONFIG_DIR" in "$GEAS_SESSION_DIR"/glab) echo "glab configured in the session" ;; esac
[ -e "$GLAB_CONFIG_DIR/config.yml" ] || echo "no glab token"
if command -v git >/dev/null; then
	printf 'protocol=https\nhost=gitlab.com\npath=group/project.git\n\n' | git credential fill
fi
echo harness done`
	for _, optional := range []bool{false, true} {
		output, code, fakes := failingSession(t, optional, script)
		if code != 0 || !strings.HasPrefix(output, "harness started\n") {
			t.Fatalf("optional %v: exit %d: %s", optional, code, output)
		}
		for _, want := range []string{
			`{"state":"failed","reason":"run github-login before starting a session","time":`,
			"gh exit 0\n",
			"glab configured in the session\n",
			"no glab token\n",
			"harness done\n",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("optional %v: output lacks %q:\n%s", optional, want, output)
			}
		}
		if strings.Contains(output, "WARNING") || strings.Contains(output, "Session ready") {
			t.Errorf("optional %v: the launcher printed while the harness ran:\n%s", optional, output)
		}
		if _, err := exec.LookPath("git"); err == nil && (!strings.Contains(output, "told us to quit") || !strings.Contains(output, "gitlab.com is not available in this session: glab issuer request failed")) {
			t.Errorf("optional %v: Git did not stop at the helper:\n%s", optional, output)
		}
		if strings.Contains(output, "glab-secret") {
			t.Errorf("optional %v: issuer output leaked:\n%s", optional, output)
		}
		// The real gh ran with the session's configuration directory and no
		// token of yours.
		raw, err := os.ReadFile(filepath.Join(fakes, "gh-called"))
		if err != nil || !strings.Contains(string(raw), "/geas/sessions/") || !strings.HasSuffix(string(raw), "/gh GH_TOKEN=unset\n") {
			t.Errorf("optional %v: gh: %q %v", optional, raw, err)
		}
		// The issuer's lookup, and nothing after it: no PAT, so nothing to
		// revoke. The lookup document itself is pinned in internal/provider/gitlab.
		calls := testutil.Calls(t, fakes)
		if len(calls) != 1 || len(calls[0]) != 6 || calls[0][3] != "graphql" || !strings.HasPrefix(calls[0][5], "query=query SessionLookup") {
			t.Errorf("optional %v: glab calls: %q", optional, calls)
		}
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
	if code := launcher.ProcessState.ExitCode(); code != 1 || !strings.Contains(string(output), "unset GEAS_SESSION_DIR to start a new session") {
		t.Fatalf("stale session: exit %d: %s", code, output)
	}
}
