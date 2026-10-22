package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/procenv"
	"github.com/nolith/geas/internal/provider/github"
	"github.com/nolith/geas/internal/provider/gitlab"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/timing"
)

// The reaper revokes what the session's ledger lists when the launcher no
// longer can. A closing herdr pane follows SIGHUP and SIGTERM with SIGKILL
// about half a second later, often before the launcher's own revocation
// finishes, and kills the whole pane process tree. The reaper is outside that
// tree and session, so it survives. It gets only the configuration path and
// the session ID, starts before anything revocable exists, and waits on a pipe
// whose write end only the launcher holds: EOF without reaperDone means the
// launcher died, however it died, or its revocation failed. Only then does it
// read the ledger.

const reaperDone = "revoked\n"

// reaperPauses come before each revocation attempt: a create the launcher sent
// as it died gets a minute to land before its name counts as never used.
var reaperPauses = []time.Duration{0, 10 * time.Second, time.Minute}

func reaperLogPath() string {
	state, err := secretfile.StateDirectory()
	if err != nil {
		return "reaper.log"
	}
	return filepath.Join(state, "reaper.log")
}

// startReaper starts `geas _reaper detach`, the leader of a new session,
// which starts the reaper itself and exits: the reaper is then neither a
// descendant of the launcher nor in its session. It returns the write end of
// the reaper's pipe.
func startReaper(binary, configPath, sessionID string) (*os.File, error) {
	if configPath == "" || sessionID == "" {
		return nil, errors.New("cannot start the revocation reaper")
	}
	// Close-on-exec: the harness, gh and glab never inherit either end.
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	command := exec.Command(binary, "_reaper", "detach", configPath, sessionID)
	command.Dir = "/"
	environment := procenv.Map(os.Environ())
	for _, key := range inheritedCredentials {
		delete(environment, key)
	}
	command.Env = procenv.List(environment)
	command.ExtraFiles = []*os.File{read}
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Returns once the detaching leader has started the reaper and exited.
	if err = command.Run(); err != nil {
		write.Close()
		return nil, errors.New("cannot start the revocation reaper")
	}
	return write, nil
}

// RunReaper serves `_reaper detach` and `_reaper watch`. SIGHUP, SIGINT and
// SIGTERM never stop a revocation; ctx only cuts the pauses between attempts.
func RunReaper(ctx context.Context, args []string) error {
	if len(args) != 3 {
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
		return watch(ctx, launcher, args[1], args[2])
	}
	return errors.New("invalid reaper invocation")
}

// revokers builds, from the configuration alone, every provider an entry may
// name, by ID.
func revokers(settings config.Config) (map[string]ledger.Revoker, error) {
	providers := map[string]ledger.Revoker{}
	if config.Enabled(settings.GitLab.Enabled) {
		gl, err := gitlab.New(settings.GitLab, settings.Duration(), api.New(), nil)
		if err != nil {
			return nil, err
		}
		providers[gl.ID()] = gl
	}
	if config.Enabled(settings.GitHub.Enabled) {
		gh := github.New(settings.GitHub, api.New())
		providers[gh.ID()] = gh
	}
	return providers, nil
}

// watch logs to reaperLogPath(), never to a terminal, and only when it acts.
func watch(ctx context.Context, launcher io.Reader, configPath, sessionID string) error {
	logf := func(subject, format string, args ...any) {
		if os.MkdirAll(filepath.Dir(reaperLogPath()), 0700) != nil {
			return
		}
		file, err := os.OpenFile(reaperLogPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		fmt.Fprintf(file, "%s %s: %s\n", time.Now().UTC().Format(time.RFC3339), subject, fmt.Sprintf(format, args...))
	}
	entryf := func(e ledger.Entry, format string, args ...any) {
		subject := e.Name
		if e.ID > 0 {
			subject = fmt.Sprintf("%s (ID %d)", e.Name, e.ID)
		}
		logf(subject, format, args...)
	}
	session := "session " + sessionID
	// Prepared before waiting, so that later changes to the configuration or
	// PATH cannot prevent the revocation.
	settings, err := config.Load(configPath)
	if err != nil {
		logf(session, "cannot load the configuration: %v", err)
		return err
	}
	// An issuer that cannot be prepared matters only if the ledger lists
	// something for it to revoke; the directory goes either way.
	providers, prepareErr := revokers(settings)
	message, _ := io.ReadAll(io.LimitReader(launcher, int64(len(reaperDone))+1))
	if string(message) == reaperDone {
		return nil
	}
	dir, err := sessiondir.Open(sessionID)
	if err != nil {
		logf(session, "cannot open the session directory: %v", err)
		return err
	}
	book, err := ledger.Open(dir)
	if err != nil {
		logf(session, "cannot read the ledger: %v", err)
		return err
	}
	pending, err := book.Unrevoked()
	if err != nil {
		book.Close()
		logf(session, "cannot read the ledger: %v", err)
		return err
	}
	if len(pending) > 0 && prepareErr != nil {
		book.Close()
		logf(session, "cannot prepare the issuer: %v", prepareErr)
		return prepareErr
	}
	// Retrying cannot revoke an entry whose provider is gone or whose ID
	// names another credential; it stays in the ledger.
	var refused bool
	for attempt, pause := range reaperPauses {
		if len(pending) == 0 {
			break
		}
		if err = timing.SleepContext(ctx, pause); err != nil {
			break
		}
		last := attempt == len(reaperPauses)-1
		revoking, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		results := book.Revoke(revoking, pending, providers)
		cancel()
		pending = pending[:0]
		for _, r := range results {
			switch {
			case r.Err == nil:
				entryf(r.Entry, "launcher gone without revoking; revoked")
			case errors.Is(r.Err, ledger.ErrGone):
				entryf(r.Entry, "launcher gone; already revoked or expired")
			case errors.Is(r.Err, ledger.ErrNotFound) && last:
				// A create the launcher sent as it died had its chance to land.
				r.Entry.Revoked = true
				if err = book.Record(r.Entry); err != nil {
					entryf(r.Entry, "cannot close the entry: %v", err)
					refused = true
					continue
				}
				entryf(r.Entry, "launcher gone; no PAT has this name, so none was created")
			case errors.Is(r.Err, gitlab.ErrOtherPAT), errors.Is(r.Err, ledger.ErrNoProvider):
				err = r.Err
				refused = true
				entryf(r.Entry, "%v", r.Err)
			default:
				// ErrNotFound before the last attempt too: the create may still
				// be on its way.
				err = r.Err
				entryf(r.Entry, "revocation failed: %v", r.Err)
				pending = append(pending, r.Entry)
			}
		}
	}
	for _, e := range pending {
		entryf(e, "giving up; the PAT expires at its configured UTC date")
	}
	book.Close()
	if len(pending) > 0 || refused {
		return err
	}
	return dir.Remove()
}
