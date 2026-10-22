// Package timing holds the parts of geas that wait: the session's timing log,
// enabled with GEAS_DEBUG=timing, and the sleep that stops early when a
// context ends. The CLI, the session and the providers all time their own
// steps with it, and a provider cannot import the session.
package timing

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Timer measures the session, for GEAS_DEBUG=timing. Each line is one step's
// duration since the timer's previous step. Until the harness starts, the
// lines add up to the time before it does. After that, a line would scribble
// over the agent's terminal, so the steps of the background acquisition are
// held, with the time since the harness started, until Flush prints them
// once the harness has exited.
type Timer struct {
	log    *log
	prefix string
	last   time.Time
}

// log is what a timer shares with its children: one output, in order.
type log struct {
	mu      sync.Mutex
	output  io.Writer
	started time.Time // when the harness started; zero before
	held    []string
	flushed bool
}

// New returns nil unless debug lists "timing". A nil timer's methods do
// nothing, so callers never check it.
func New(debug string, output io.Writer) *Timer {
	for _, value := range strings.Split(debug, ",") {
		if strings.TrimSpace(value) == "timing" {
			return &Timer{log: &log{output: output}, last: time.Now()}
		}
	}
	return nil
}

// Child returns a timer for one provider, whose steps run in parallel with
// the others': its lines start with prefix, and its first step after the
// harness starts is measured from that start.
func (t *Timer) Child(prefix string) *Timer {
	if t == nil {
		return nil
	}
	return &Timer{log: t.log, prefix: prefix + " ", last: time.Now()}
}

// Step logs the duration of the step that just finished, and starts the
// next one.
func (t *Timer) Step(name string) {
	if t == nil {
		return
	}
	l := t.log
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	since := t.last
	if since.Before(l.started) {
		since = l.started
	}
	t.last = now
	line := fmt.Sprintf("geas timing: %-20s %.3fs", t.prefix+name, now.Sub(since).Seconds())
	if l.started.IsZero() || l.flushed {
		fmt.Fprintln(l.output, line)
		return
	}
	l.held = append(l.held, fmt.Sprintf("%s (+%.3fs)", line, now.Sub(l.started).Seconds()))
}

// HarnessStart logs the last step before the harness starts; every later
// step is held until Flush.
func (t *Timer) HarnessStart() {
	if t == nil {
		return
	}
	t.Step("harness start")
	t.log.mu.Lock()
	t.log.started = time.Now()
	t.log.mu.Unlock()
}

// Flush prints the held steps, once the harness has given the terminal
// back, and any later step as it happens.
func (t *Timer) Flush() {
	if t == nil {
		return
	}
	l := t.log
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.held {
		fmt.Fprintln(l.output, line)
	}
	l.held, l.flushed = nil, true
}

// SleepContext waits for duration, unless ctx ends first: a lock or a retry
// never outlives the session.
func SleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
