// Package session runs one session: it creates the session directory and
// starts the revocation reaper, starts the harness with an environment that
// reads that directory, acquires the credentials into it in the background,
// and revokes them when the harness exits.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/credhelper"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/provider/gitlab"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/timing"
)

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// harness is the started harness, the leader of its own process group, in
// the terminal's foreground when there is a terminal.
type harness struct {
	command  *exec.Cmd
	terminal bool
	previous int // the terminal's foreground group before the harness
	wait     chan error
}

func startHarness(args, environment []string) (*harness, error) {
	command := exec.Command(args[0], args[1:]...)
	command.Env = environment
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	// Keep the controlling terminal; syscall handles the foreground group before exec.
	previous, terminalErr := terminalProcessGroup(int(os.Stdin.Fd()))
	terminal := terminalErr == nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: terminal, Ctty: int(os.Stdin.Fd())}
	if err := command.Start(); err != nil {
		return nil, errors.New("cannot start harness; check executable and arguments")
	}
	h := &harness{command: command, terminal: terminal, previous: previous, wait: make(chan error, 1)}
	go func() { h.wait <- command.Wait() }()
	return h, nil
}

// await returns the harness's exit code once it exits, or stops its process
// group when ctx ends or the session's time is up.
func (h *harness) await(ctx context.Context, duration time.Duration) (int, error) {
	if h.terminal {
		defer func() {
			signal.Ignore(syscall.SIGTTOU)
			setTerminalProcessGroup(int(os.Stdin.Fd()), h.previous)
			signal.Reset(syscall.SIGTTOU)
		}()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	var reason error
	code := 1
	select {
	case err := <-h.wait:
		return exitCode(err), nil
	case <-ctx.Done():
		reason = errors.New("session interrupted")
		code = 130
	case <-timer.C:
		reason = errors.New("session time limit reached; stopping harness")
		code = 124
	}
	syscall.Kill(-h.command.Process.Pid, syscall.SIGTERM)
	stopProcessGroup(h.command.Process.Pid, h.wait, 5*time.Second)
	return code, reason
}

func runHarness(ctx context.Context, args, environment []string, duration time.Duration) (int, error) {
	h, err := startHarness(args, environment)
	if err != nil {
		return ExitNotStarted, err
	}
	return h.await(ctx, duration)
}

func stopProcessGroup(processGroup int, wait <-chan error, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	leaderExited := false
	for {
		if !leaderExited {
			select {
			case <-wait:
				leaderExited = true
			default:
			}
		}
		groupErr := syscall.Kill(-processGroup, 0)
		groupExists := groupErr == nil || errors.Is(groupErr, syscall.EPERM)
		if leaderExited && !groupExists {
			return
		}
		select {
		case <-wait:
			leaderExited = true
		case <-ticker.C:
		case <-timer.C:
			syscall.Kill(-processGroup, syscall.SIGKILL)
			if !leaderExited {
				<-wait
			}
			return
		}
	}
}

// reason is why a provider failed, as its marker records it for the harness:
// one line, which, like main's errors, names no local path.
func reason(err error) string {
	if errors.As(err, new(*os.PathError)) {
		return "local file or permission error"
	}
	text := strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, err.Error())
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// displayName is how the output names a provider kind.
var displayName = map[string]string{"gitlab": "GitLab", "github": "GitHub"}

// teardown revokes every unrevoked ledger entry through the provider that
// created it, in parallel across providers. Once nothing is left it tells the
// reaper and removes the session's directory; otherwise the directory stays,
// as evidence and for the reaper, which reads the ledger and retries what
// failed.
func teardown(dir *sessiondir.Dir, book *ledger.Ledger, providers []provider.Provider, reaper *os.File, output io.Writer) {
	if reaper != nil {
		defer reaper.Close()
	}
	revokers := map[string]ledger.Revoker{}
	kinds := map[string]string{}
	for _, p := range providers {
		revokers[p.ID()] = p
		kinds[p.ID()] = p.Kind()
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, err := book.Unrevoked()
	var results []ledger.Result
	if err == nil {
		results = book.Revoke(cleanup, entries, revokers)
	}
	revoked := map[string]bool{}
	failed := map[string]bool{}
	for _, r := range results {
		kind := kinds[r.Entry.Provider]
		switch {
		case r.Err == nil:
			revoked[kind] = true
		case errors.Is(r.Err, ledger.ErrGone):
		case errors.Is(r.Err, ledger.ErrNotFound):
			// Every request this process sent has returned: a name the
			// provider does not know was never created.
			r.Entry.Revoked = true
			if book.Record(r.Entry) != nil {
				failed[kind] = true
			}
		default:
			failed[kind] = true
		}
	}
	book.Close()
	if err != nil || len(failed) > 0 {
		// The directory stays: its ledger names what is left to revoke.
		if err != nil {
			for _, p := range providers {
				failed[p.Kind()] = true
			}
		}
		for _, kind := range slices.Sorted(maps.Keys(failed)) {
			fmt.Fprintln(output, "WARNING: "+displayName[kind]+" revocation failed; the reaper retries it, see "+reaperLogPath()+".")
		}
		return
	}
	if reaper != nil {
		reaper.WriteString(reaperDone)
	}
	dir.Remove()
	for _, kind := range slices.Sorted(maps.Keys(revoked)) {
		fmt.Fprintln(output, displayName[kind]+" session PAT revoked.")
	}
}

// mark records how far a provider got, for the credential helper and geas
// status. A marker that cannot be written leaves the previous state, which
// the helper never takes for ready.
func mark(dir *sessiondir.Dir, id, state, reason string) error {
	return dir.SetMarker(id, sessiondir.Marker{State: state, Reason: reason, Time: time.Now().UTC()})
}

// plan builds the enabled providers from the configuration, without a
// network call. A provider that cannot even be prepared, such as GitLab
// without glab or with an unreadable issuer file, is kept for its setup and
// configuration, and failed lists why: it never acquires anything, and the
// session starts all the same.
func plan(settings config.Config, api *api.API, timer *timing.Timer) (providers []provider.Provider, failed map[string]string) {
	failed = map[string]string{}
	if config.Enabled(settings.GitLab.Enabled) {
		gl, err := gitlab.New(settings.GitLab, settings.Duration(), api, nil)
		if err != nil {
			gl = &gitlab.Provider{Config: settings.GitLab}
			failed[gl.ID()] = reason(err)
		}
		gl.Steps = timer.Child(gl.ID())
		providers = append(providers, gl)
	}
	if config.Enabled(settings.GitHub.Enabled) {
		providers = append(providers, github.New(settings.GitHub, api))
	}
	return providers, failed
}

// ExitNotStarted is geas run's exit code when it fails before the harness
// starts: a configuration error, a session directory or a reaper it cannot
// create, a harness it cannot start. The error is printed; a launcher
// around geas can tell this from a harness that started and failed.
const ExitNotStarted = 125

// launchReaper starts the reaper; the tests replace it with one that fails.
var launchReaper = startReaper

// Run runs one session around args. It plans the session without a network
// call, starts the reaper, then the harness, and only then acquires the
// providers' credentials, in parallel, in the background: the harness never
// waits for the network. A provider that fails is marked failed and never
// stops the session; nothing prints while the harness runs. Before the
// harness starts, a failure returns ExitNotStarted; after, the exit code is
// the harness's, unchanged, or 130 and 124 when the session interrupted it
// or ran out of time. What the session minted is revoked on the way out.
func Run(ctx context.Context, settings config.Config, args []string, api *api.API, output io.Writer, timer *timing.Timer) (int, error) {
	if len(args) == 0 {
		return ExitNotStarted, errors.New("specify a harness command after run --")
	}
	binary, err := executablePath()
	if err != nil {
		return ExitNotStarted, err
	}
	providers, failed := plan(settings, api, timer)
	id, err := sessiondir.NewID(time.Now())
	if err != nil {
		return ExitNotStarted, err
	}
	// The configuration's errors first: nothing exists yet to clean up.
	path, err := sessiondir.Path(id)
	if err != nil {
		return ExitNotStarted, err
	}
	exported, err := providerEnv(providers, path)
	if err != nil {
		return ExitNotStarted, err
	}
	hosts, err := credhelper.Hosts(providers)
	if err != nil {
		return ExitNotStarted, err
	}
	dir, err := sessiondir.Create(id)
	if err != nil {
		return ExitNotStarted, err
	}
	book, err := ledger.Create(dir)
	if err != nil {
		dir.Remove()
		return ExitNotStarted, err
	}
	if err = writeGitConfig(dir, binary, hosts); err != nil {
		dir.Remove()
		return ExitNotStarted, err
	}
	if err = writeSandbox(dir, providers); err != nil {
		dir.Remove()
		return ExitNotStarted, err
	}
	environment, err := sessionEnvironment(os.Environ(), dir, exported)
	if err != nil {
		dir.Remove()
		return ExitNotStarted, err
	}
	// Every provider has a marker before the harness can look for one.
	for _, p := range providers {
		state, why := sessiondir.Pending, ""
		if why = failed[p.ID()]; why != "" {
			state = sessiondir.Failed
		}
		if err = mark(dir, p.ID(), state, why); err != nil {
			dir.Remove()
			return ExitNotStarted, err
		}
	}
	timer.Step("plan")
	// The reaper's pipe: closed without "revoked", it revokes the ledger
	// itself and removes the session's directory.
	var reaper *os.File
	defer func() { teardown(dir, book, providers, reaper, output) }()
	// Before any credential exists: whatever the ledger will list, and the
	// directory the credentials land in, the reaper cleans up if the launcher
	// cannot.
	if reaper, err = launchReaper(binary, settings.Source(), id); err != nil {
		return ExitNotStarted, err
	}
	timer.Step("reaper")
	timer.HarnessStart()
	h, err := startHarness(args, environment)
	if err != nil {
		return ExitNotStarted, err
	}
	// Stopped, and waited for, before teardown revokes what they minted and
	// removes the directory they write into.
	acquiring, stop := context.WithCancel(ctx)
	var acquirers sync.WaitGroup
	defer func() { stop(); acquirers.Wait(); timer.Flush() }()
	store := newCredentials(dir)
	for _, p := range providers {
		if failed[p.ID()] != "" {
			continue
		}
		acquirers.Add(1)
		go func() {
			defer acquirers.Done()
			acquire(acquiring, store, book, p, timer.Child(p.ID()))
		}()
	}
	return h.await(ctx, settings.Duration())
}

// acquire gets p's credential while the harness runs. It writes the
// credential's files and its kind's tool configuration, then p's ready
// marker, or a failed one with the reason; then it settles what is left,
// such as GitLab's id lookup, and keeps the credential fresh for as long as
// ctx lasts. Nothing prints: the harness owns the terminal.
func acquire(ctx context.Context, store *credentials, book *ledger.Ledger, p provider.Provider, steps *timing.Timer) {
	c, err := p.Acquire(ctx, book)
	if err == nil {
		err = store.put(p, c)
	}
	if err != nil {
		mark(store.dir, p.ID(), sessiondir.Failed, reason(err))
		steps.Step("failed")
		return
	}
	if mark(store.dir, p.ID(), sessiondir.Ready, "") != nil {
		return
	}
	steps.Step("ready")
	if settler, ok := p.(provider.Settler); ok {
		// Left undone, the exit and the reaper find the credential by the
		// name the ledger holds.
		settler.Settle(ctx, book)
	}
	keepFresh(ctx, store, p, c, systemClock)
}

// refreshMargin is how long before a credential expires keepFresh replaces
// it: inside the five minutes in which GitHub's provider rotates its token.
// refreshRetry is the pause after a refresh that failed or changed nothing.
var refreshMargin, refreshRetry = 4 * time.Minute, time.Minute

// clock is what keepFresh reads the time and waits with; the tests' clock
// moves only when it is told to wait.
type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

var systemClock = clock{now: time.Now, sleep: timing.SleepContext}

// keepFresh replaces c in the session directory before it expires, for as
// long as ctx lasts: a GitHub App token lasts eight hours, a session up to
// 48. Another session may have rotated the token already; Refresh then
// returns that one. A failure keeps the current credential and retries
// while it is still valid; once it has expired with no replacement, p's
// marker turns failed, with the reason, and the next replacement makes it
// ready again. Nothing prints, as the harness owns the terminal. A
// credential that never expires within the session, such as a GitLab
// session PAT, only waits.
func keepFresh(ctx context.Context, store *credentials, p provider.Provider, c provider.Credential, clk clock) {
	if c.Expires.IsZero() {
		return
	}
	expired := false
	for {
		wait := c.Expires.Add(-refreshMargin).Sub(clk.now())
		if wait < refreshRetry {
			wait = refreshRetry
		}
		if clk.sleep(ctx, wait) != nil {
			return
		}
		next, err := p.Refresh(ctx, c)
		if ctx.Err() != nil {
			return
		}
		if err == nil && next.Expires.After(c.Expires) {
			if err = store.put(p, next); err == nil {
				c = next
				if expired && mark(store.dir, p.ID(), sessiondir.Ready, "") == nil {
					expired = false
				}
				continue
			}
		}
		if expired || clk.now().Before(c.Expires) {
			continue
		}
		why := "credential expired, and no newer one is available yet"
		if err != nil {
			why = "credential expired, and refreshing it failed: " + reason(err)
		}
		expired = mark(store.dir, p.ID(), sessiondir.Failed, why) == nil
	}
}
