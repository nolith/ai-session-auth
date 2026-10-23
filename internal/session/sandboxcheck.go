package session

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/sessiondir"
)

// SandboxMarker is the marker the background sandbox check records its
// result under, next to the providers', for geas status.
const SandboxMarker = "sandbox"

// sandboxCheckTimeout bounds the background check: a wrapper that hangs
// fails it instead of running for the whole session.
var sandboxCheckTimeout = time.Minute

// checkSandbox runs `geas sandbox check` through the wrapper, as the harness
// itself runs, and records the verdict in SandboxMarker: ready, or failed
// with the paths the sandbox let it read. It runs in a session of its own,
// with no terminal, its output kept: nothing may reach the harness's
// terminal. When ctx ends first, it records nothing: the session is over.
func checkSandbox(ctx context.Context, dir *sessiondir.Dir, wrapper []string, binary string, environment []string) {
	ctx, cancel := context.WithTimeout(ctx, sandboxCheckTimeout)
	defer cancel()
	args := append(slices.Clone(wrapper), binary, "sandbox", "check")
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Env = environment
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	// No controlling terminal: a wrapper that takes the terminal's
	// foreground, as the harness's may, cannot take it from the harness.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	err := command.Run()
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	state, why := sandboxVerdict(err, output.Bytes(), ctx.Err() != nil)
	mark(dir, SandboxMarker, state, why)
}

// sandboxVerdict reads the check's exit and output: ready when it passed;
// otherwise the paths it could read, or its own summary, or the last thing
// the wrapper said, or how it ended.
func sandboxVerdict(err error, output []byte, timedOut bool) (state, reason string) {
	if err == nil {
		return sessiondir.Ready, ""
	}
	if timedOut {
		return sessiondir.Failed, "the sandbox check did not finish within " + sandboxCheckTimeout.String()
	}
	var readable []string
	summary, last := "", ""
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "readable: "):
			readable = append(readable, strings.TrimPrefix(line, "readable: "))
		case strings.HasPrefix(line, "sandbox check: "):
			summary = strings.TrimPrefix(line, "sandbox check: ")
		}
		last = line
	}
	switch {
	case len(readable) > 0:
		reason = "readable: " + strings.Join(readable, ", ")
	case summary != "":
		reason = summary
	case last != "":
		reason = "the wrapper or the check failed: " + last
	default:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			reason = "the sandbox check failed: " + exit.String()
		} else {
			reason = "cannot run the sandbox check through the wrapper"
		}
	}
	return sessiondir.Failed, checkReason(reason)
}

// checkReason makes a reason one line, without control characters, and
// shorter than a terminal's scrollback: it names paths, which the
// providers' reasons never do, so it may be longer than theirs.
func checkReason(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, text)
	if len(text) > 1000 {
		text = text[:1000] + "..."
	}
	return text
}

// limitedBuffer keeps the first 64 KiB written to it, and drops the rest.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 64<<10 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
