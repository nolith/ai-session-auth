package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The reaper revokes the session PAT when the launcher no longer can. A closing
// herdr pane follows SIGHUP and SIGTERM with SIGKILL about half a second later,
// often before the launcher's own revocation finishes, and kills the whole pane
// process tree. The reaper is outside that tree and session, so it survives. It
// gets only the configuration path and the PAT's name and ID, and waits on a
// pipe whose write end only the launcher holds: EOF without reaperDone means
// the launcher died, however it died, or its revocation failed.

const reaperDone = "revoked\n"

func reaperLogPath() string {
	state, err := stateDirectory()
	if err != nil {
		return "reaper.log"
	}
	return filepath.Join(state, "reaper.log")
}

// startReaper starts `agent-auth _reaper detach`, the leader of a new session,
// which starts the reaper itself and exits: the reaper is then neither a
// descendant of the launcher nor in its session. It returns the write end of
// the reaper's pipe.
func startReaper(binary, configPath string, pat sessionPAT) (*os.File, error) {
	if configPath == "" || pat.ID <= 0 {
		return nil, errors.New("cannot start the revocation reaper")
	}
	// Close-on-exec: the harness, gh and glab never inherit either end.
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	command := exec.Command(binary, "_reaper", "detach", configPath, pat.Name, strconv.FormatInt(pat.ID, 10))
	command.Dir = "/"
	environment := environmentMap(os.Environ())
	for _, key := range inheritedCredentials {
		delete(environment, key)
	}
	command.Env = environmentList(environment)
	command.ExtraFiles = []*os.File{read}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Returns once the detaching leader has started the reaper and exited.
	if err = command.Run(); err != nil {
		write.Close()
		return nil, errors.New("cannot start the revocation reaper")
	}
	return write, nil
}

// runReaper serves `_reaper detach` and `_reaper watch`. SIGHUP, SIGINT and
// SIGTERM never stop a revocation; ctx only cuts the pauses between attempts.
func runReaper(ctx context.Context, args []string) error {
	if len(args) != 4 {
		return errors.New("invalid reaper invocation")
	}
	id, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil || id <= 0 {
		return errors.New("invalid reaper invocation")
	}
	launcher := os.NewFile(3, "launcher")
	switch args[0] {
	case "detach":
		binary, err := executablePath()
		if err != nil {
			return err
		}
		command := exec.Command(binary, append([]string{"_reaper", "watch"}, args[1:]...)...)
		command.Dir = "/"
		command.ExtraFiles = []*os.File{launcher}
		return command.Start()
	case "watch":
		return watch(ctx, launcher, args[1], sessionPAT{Name: args[2], ID: id})
	}
	return errors.New("invalid reaper invocation")
}

// watch logs to reaperLogPath(), never to a terminal, and only when it acts.
func watch(ctx context.Context, launcher io.Reader, configPath string, pat sessionPAT) error {
	logf := func(format string, args ...any) {
		if os.MkdirAll(filepath.Dir(reaperLogPath()), 0700) != nil {
			return
		}
		file, err := os.OpenFile(reaperLogPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		fmt.Fprintf(file, "%s %s (ID %d): %s\n", time.Now().UTC().Format(time.RFC3339), pat.Name, pat.ID, fmt.Sprintf(format, args...))
	}
	// Prepared before waiting, so that later changes to the configuration or
	// PATH cannot prevent the revocation.
	config, err := loadConfig(configPath)
	if err != nil {
		logf("cannot load the configuration: %v", err)
		return err
	}
	gitlab, err := newGitLab(config.GitLab, newAPI())
	if err != nil {
		logf("cannot prepare the issuer: %v", err)
		return err
	}
	message, _ := io.ReadAll(io.LimitReader(launcher, int64(len(reaperDone))+1))
	if string(message) == reaperDone {
		return nil
	}
	for _, pause := range []time.Duration{0, 10 * time.Second, time.Minute} {
		if err = sleepContext(ctx, pause); err != nil {
			break
		}
		attempt, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		revoked, revokeErr := gitlab.revokeChecked(attempt, pat)
		cancel()
		switch {
		case revokeErr == nil && revoked:
			logf("launcher gone without revoking; revoked")
			return nil
		case revokeErr == nil:
			logf("launcher gone; already revoked or expired")
			return nil
		case errors.Is(revokeErr, errOtherPAT):
			logf("%v", revokeErr)
			return revokeErr
		}
		err = revokeErr
		logf("revocation failed: %v", revokeErr)
	}
	logf("giving up; the PAT expires at its configured UTC date")
	return err
}
