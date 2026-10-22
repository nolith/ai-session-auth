// Package session runs one session: it creates the session directory and
// starts the revocation reaper, resolves the credentials the harness needs
// into that directory, hands the harness an environment that reads them, and
// revokes them when it exits.
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
func runHarness(ctx context.Context, args, environment []string, duration time.Duration) (int, error) {
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
		return 1, errors.New("cannot start harness; check executable and arguments")
	}
	if terminal {
		defer func() {
			signal.Ignore(syscall.SIGTTOU)
			setTerminalProcessGroup(int(os.Stdin.Fd()), previous)
			signal.Reset(syscall.SIGTTOU)
		}()
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	var reason error
	code := 1
	select {
	case err := <-wait:
		return exitCode(err), nil
	case <-ctx.Done():
		reason = errors.New("session interrupted")
		code = 130
	case <-timer.C:
		reason = errors.New("session time limit reached; stopping harness")
		code = 124
	}
	syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	stopProcessGroup(command.Process.Pid, wait, 5*time.Second)
	return code, reason
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

// skippable reports why an optional provider is skipped, or "" when err must
// stop the session: the provider is required, or the session was interrupted.
// The reason, like main's, names no local path.
func skippable(ctx context.Context, optional bool, err error) string {
	if !optional || ctx.Err() != nil {
		return ""
	}
	if errors.As(err, new(*os.PathError)) {
		return "local file or permission error"
	}
	return err.Error()
}

// skip warns that an optional provider is unavailable, and how to fix it.
func skip(output io.Writer, provider, reason, fix string) {
	fmt.Fprintf(output, "WARNING: %s is not available in this session: %s. Fix: %s\n", provider, reason, fix)
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

// mark records how far a provider got, for the credential helper and, later,
// geas status. A marker that cannot be written leaves the provider pending,
// which the helper reports as unavailable too.
func mark(dir *sessiondir.Dir, id, skipped string) {
	m := sessiondir.Marker{State: sessiondir.Ready, Time: time.Now().UTC()}
	if skipped != "" {
		m = sessiondir.Marker{State: sessiondir.Failed, Reason: skipped, Time: m.Time}
	}
	dir.SetMarker(id, m)
}

// Run runs one session around args: it resolves the providers' credentials,
// starts the harness, and revokes what it minted on the way out. The exit code
// it returns is the harness's.
func Run(ctx context.Context, settings config.Config, args []string, api *api.API, output io.Writer, timer *timing.Timer) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("specify a harness command after run --")
	}
	binary, err := executablePath()
	if err != nil {
		return 1, err
	}
	gitlabFix := "geas save-gitlab-issuer"
	if settings.GitLab.Issuer == "glab" {
		gitlabFix = "glab auth login"
	}
	// A skipped optional provider keeps its configuration directory and
	// credential helper, whose answer is then an error, from its marker:
	// never your own credentials.
	var githubSkipped, gitlabSkipped string
	var gl *gitlab.Provider
	if config.Enabled(settings.GitLab.Enabled) {
		gl, err = gitlab.New(settings.GitLab, settings.Duration(), api, timer)
		if err != nil {
			// A missing glab is setup, like a missing gh; an unreadable issuer
			// file is a credential, like GitHub's state file.
			if settings.GitLab.Issuer == "glab" {
				return 1, err
			}
			if gitlabSkipped = skippable(ctx, settings.GitLab.Optional, err); gitlabSkipped == "" {
				return 1, err
			}
			skip(output, "GitLab", gitlabSkipped, gitlabFix)
			// Kept for its ID and configuration, never asked for a credential.
			gl = &gitlab.Provider{Config: settings.GitLab}
		}
	}
	var gh *github.Provider
	if config.Enabled(settings.GitHub.Enabled) {
		gh = github.New(settings.GitHub, api)
	}
	var providers []provider.Provider
	if gl != nil {
		providers = append(providers, gl)
	}
	if gh != nil {
		providers = append(providers, gh)
	}
	id, err := sessiondir.NewID(time.Now())
	if err != nil {
		return 1, err
	}
	// The configuration's errors first: nothing exists yet to clean up.
	path, err := sessiondir.Path(id)
	if err != nil {
		return 1, err
	}
	exported, err := providerEnv(providers, path)
	if err != nil {
		return 1, err
	}
	hosts, err := credhelper.Hosts(providers)
	if err != nil {
		return 1, err
	}
	dir, err := sessiondir.Create(id)
	if err != nil {
		return 1, err
	}
	book, err := ledger.Create(dir)
	if err != nil {
		dir.Remove()
		return 1, err
	}
	if err = writeGitConfig(dir, binary, hosts); err != nil {
		dir.Remove()
		return 1, err
	}
	if err = writeSandbox(dir, providers); err != nil {
		dir.Remove()
		return 1, err
	}
	environment, err := sessionEnvironment(os.Environ(), dir, exported)
	if err != nil {
		dir.Remove()
		return 1, err
	}
	store := newCredentials(dir)
	// The reaper's pipe: closed without "revoked", it revokes the ledger
	// itself and removes the session's directory.
	var reaper *os.File
	defer func() { teardown(dir, book, providers, reaper, output) }()
	// Before any credential exists: whatever the ledger will list, and the
	// directory the credentials land in, the reaper cleans up if the launcher
	// cannot.
	if reaper, err = startReaper(binary, settings.Source(), id); err != nil {
		return 1, err
	}
	var githubCredential provider.Credential
	if gh != nil {
		githubCredential, err = gh.Acquire(ctx, book)
		timer.Step("GitHub token")
		if err != nil {
			if githubSkipped = skippable(ctx, settings.GitHub.Optional, err); githubSkipped == "" {
				return 1, err
			}
			skip(output, "GitHub", githubSkipped, "geas github-login")
		} else if err = store.put(gh, githubCredential); err != nil {
			return 1, err
		}
		mark(dir, gh.ID(), githubSkipped)
	}
	if gl != nil && gitlabSkipped == "" {
		credential, err := gl.Acquire(ctx, book)
		// Skipped only before a PAT exists: a failure after the mint stops the
		// session, as it always has. The ledger revokes the PAT either way.
		if err != nil && !errors.As(err, new(provider.CreatedError)) {
			gitlabSkipped = skippable(ctx, settings.GitLab.Optional, err)
		}
		if err != nil && gitlabSkipped == "" {
			return 1, err
		}
		if err != nil {
			skip(output, "GitLab", gitlabSkipped, gitlabFix)
		} else if err = store.put(gl, credential); err != nil {
			return 1, err
		}
	}
	if gl != nil {
		mark(dir, gl.ID(), gitlabSkipped)
	}
	if (githubSkipped != "" || !config.Enabled(settings.GitHub.Enabled)) && (gitlabSkipped != "" || !config.Enabled(settings.GitLab.Enabled)) {
		fmt.Fprintln(output, "Session ready without credentials: every enabled provider was skipped.")
	} else {
		fmt.Fprintln(output, "Session ready: user credentials available through gh/glab and Git HTTPS.")
	}
	timer.Stop()
	// Stopped, and waited for, before teardown removes the directory it
	// writes into.
	refreshing, stop := context.WithCancel(ctx)
	var refreshers sync.WaitGroup
	defer func() { stop(); refreshers.Wait() }()
	if gh != nil && githubSkipped == "" {
		refreshers.Add(1)
		go func() {
			defer refreshers.Done()
			keepFresh(refreshing, store, gh, githubCredential)
		}()
	}
	return runHarness(ctx, args, environment, settings.Duration())
}

// refreshMargin is how long before a credential expires keepFresh replaces
// it: inside the five minutes in which GitHub's provider rotates its token.
// refreshRetry is the pause after a refresh that failed or changed nothing.
var refreshMargin, refreshRetry = 4 * time.Minute, time.Minute

// keepFresh replaces c in the session directory before it expires, for as
// long as ctx lasts: a GitHub App token lasts eight hours, a session up to
// 48. Another session may have rotated the token already; Refresh then
// returns that one. A failure keeps the current credential and retries;
// nothing prints, as the harness owns the terminal.
func keepFresh(ctx context.Context, store *credentials, p provider.Provider, c provider.Credential) {
	for {
		wait := time.Until(c.Expires.Add(-refreshMargin))
		if wait < refreshRetry {
			wait = refreshRetry
		}
		if timing.SleepContext(ctx, wait) != nil {
			return
		}
		next, err := p.Refresh(ctx, c)
		if err != nil || !next.Expires.After(c.Expires) || ctx.Err() != nil {
			continue
		}
		if store.put(p, next) == nil {
			c = next
		}
	}
}
