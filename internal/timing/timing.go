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
	"time"
)

// Timer measures the session start, for GEAS_DEBUG=timing. Each line is one
// step's duration since the previous step, so the lines add up to the time
// before the harness starts. The timer stops just before the harness does:
// after that, a line would scribble over the agent's terminal.
type Timer struct {
	enabled bool
	output  io.Writer
	last    time.Time
}

// New returns nil unless debug lists "timing". A nil timer's methods do
// nothing, so callers never check it.
func New(debug string, output io.Writer) *Timer {
	for _, value := range strings.Split(debug, ",") {
		if strings.TrimSpace(value) == "timing" {
			return &Timer{enabled: true, output: output, last: time.Now()}
		}
	}
	return nil
}

// Step prints the duration of the step that just finished, and starts the
// next one.
func (t *Timer) Step(name string) {
	if t == nil || !t.enabled {
		return
	}
	now := time.Now()
	fmt.Fprintf(t.output, "geas timing: %-20s %.3fs\n", name, now.Sub(t.last).Seconds())
	t.last = now
}

// Stop prints the last step and silences the timer for good.
func (t *Timer) Stop() {
	if t == nil {
		return
	}
	t.Step("harness start")
	t.enabled = false
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
