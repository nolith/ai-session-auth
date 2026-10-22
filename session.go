package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/provider/gitlab"
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

func runSession(ctx context.Context, settings config.Config, args []string, api *api.API, output io.Writer, timer *timing.Timer) (int, error) {
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
	// A skipped optional provider keeps its wrapper, configuration directory
	// and credential helper, whose answer is then an error: never your own
	// credentials.
	var githubSkipped, gitlabSkipped string
	// Before the session's own wrappers exist: a glab issuer resolves the glab
	// on your PATH.
	var gl *gitlab.Provider
	if config.Enabled(settings.GitLab.Enabled) {
		gl, err = gitlab.New(settings.GitLab, api, timer)
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
			gl = nil
		}
	}
	// /tmp keeps UNIX socket names short even on macOS with a long TMPDIR.
	directory, err := os.MkdirTemp("/tmp", "geas-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(directory)
	address := filepath.Join(directory, "broker.sock")
	environment, err := sessionEnvironment(settings, directory, address, binary, os.Environ(), executableLookup)
	if err != nil {
		return 1, err
	}
	var gh *github.Provider
	if config.Enabled(settings.GitHub.Enabled) {
		gh = github.New(settings.GitHub, api)
		token, err := gh.Token(ctx)
		if err == nil {
			err = gh.Verify(ctx, token)
		}
		timer.Step("GitHub token")
		if err != nil {
			if githubSkipped = skippable(ctx, settings.GitHub.Optional, err); githubSkipped == "" {
				return 1, err
			}
			skip(output, "GitHub", githubSkipped, "geas github-login")
			gh = nil
		}
	}
	// The reaper's pipe: closed without "revoked", it revokes the PAT itself.
	var reaper *os.File
	if gl != nil {
		defer func() {
			if reaper != nil {
				defer reaper.Close()
			}
			if len(gl.Created) == 0 {
				return
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := gl.RevokeCreated(cleanup); err != nil {
				if reaper != nil {
					fmt.Fprintln(output, "WARNING: GitLab revocation failed; the reaper retries it, see "+reaperLogPath()+".")
				} else {
					fmt.Fprintln(output, "WARNING: GitLab revocation failed; the PAT will expire at its configured UTC date.")
				}
				return
			}
			if reaper != nil {
				reaper.WriteString(reaperDone)
			}
			fmt.Fprintln(output, "GitLab session PAT revoked.")
		}()
	}
	broker := &Broker{Config: settings, Deadline: time.Now().Add(settings.Duration()), Now: time.Now, GitHubSkipped: githubSkipped, GitLabSkipped: gitlabSkipped}
	if gh != nil {
		broker.GitHubToken = gh.Token
	}
	// Bind before creating a PAT: failure to create a socket must not mint a token.
	server, err := startBroker(ctx, address, broker)
	if err != nil {
		return 1, err
	}
	defer server.close()
	if gl != nil {
		token, createErr := gl.Create(ctx, settings.Duration())
		err = createErr
		// Skipped only before a PAT exists: one that was minted is revoked at
		// exit, which a skipped provider never needs.
		if err != nil && len(gl.Created) == 0 {
			gitlabSkipped = skippable(ctx, settings.GitLab.Optional, err)
		}
		broker.mu.Lock()
		broker.GitLabToken = token
		broker.GitLabSkipped = gitlabSkipped
		broker.mu.Unlock()
		if err != nil && gitlabSkipped == "" {
			return 1, err
		}
		if err != nil {
			skip(output, "GitLab", gitlabSkipped, gitlabFix)
		} else if reaper, err = startReaper(binary, settings.Source(), gl.Created[0]); err != nil {
			return 1, err
		}
	}
	if (githubSkipped != "" || !config.Enabled(settings.GitHub.Enabled)) && (gitlabSkipped != "" || !config.Enabled(settings.GitLab.Enabled)) {
		fmt.Fprintln(output, "Session ready without credentials: every enabled provider was skipped.")
	} else {
		fmt.Fprintln(output, "Session ready: user credentials available through gh/glab and Git HTTPS.")
	}
	timer.Stop()
	return runHarness(ctx, args, environment, settings.Duration())
}
