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

func runSession(ctx context.Context, config Config, args []string, api *API, output io.Writer) (int, error) {
	if len(args) == 0 {
		return 1, errors.New("specify a harness command after run --")
	}
	binary, err := executablePath()
	if err != nil {
		return 1, err
	}
	// Before the session's own wrappers exist: a glab issuer resolves the glab
	// on your PATH.
	var gitlab *GitLab
	if enabled(config.GitLab.Enabled) {
		gitlab, err = newGitLab(config.GitLab, api)
		if err != nil {
			return 1, err
		}
	}
	// /tmp keeps UNIX socket names short even on macOS with a long TMPDIR.
	directory, err := os.MkdirTemp("/tmp", "ai-auth-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(directory)
	address := filepath.Join(directory, "broker.sock")
	environment, err := sessionEnvironment(config, directory, address, binary, os.Environ(), executableLookup)
	if err != nil {
		return 1, err
	}
	var github *GitHub
	if enabled(config.GitHub.Enabled) {
		github = &GitHub{Config: config.GitHub, API: api, Now: time.Now}
		token, err := github.token(ctx)
		if err != nil {
			return 1, err
		}
		if err = github.verify(ctx, token); err != nil {
			return 1, err
		}
	}
	// The reaper's pipe: closed without "revoked", it revokes the PAT itself.
	var reaper *os.File
	if gitlab != nil {
		defer func() {
			if reaper != nil {
				defer reaper.Close()
			}
			if len(gitlab.Created) == 0 {
				return
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := gitlab.revokeCreated(cleanup); err != nil {
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
	broker := &Broker{Config: config, Deadline: time.Now().Add(config.duration()), Now: time.Now}
	if github != nil {
		broker.GitHubToken = github.token
	}
	// Bind before creating a PAT: failure to create a socket must not mint a token.
	server, err := startBroker(ctx, address, broker)
	if err != nil {
		return 1, err
	}
	defer server.close()
	if gitlab != nil {
		token, createErr := gitlab.create(ctx, config.duration())
		err = createErr
		broker.mu.Lock()
		broker.GitLabToken = token
		broker.mu.Unlock()
		if err != nil {
			return 1, err
		}
		if reaper, err = startReaper(binary, config.source, gitlab.Created[0]); err != nil {
			return 1, err
		}
	}
	fmt.Fprintln(output, "Session ready: user credentials available through gh/glab and Git HTTPS.")
	return runHarness(ctx, args, environment, config.duration())
}
