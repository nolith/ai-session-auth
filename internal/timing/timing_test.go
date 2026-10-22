package timing

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// The timing log measures the steps before the harness, then holds the
// background steps: a line once the harness owns the terminal would corrupt
// an agent's TUI. They print, in order, once the harness has exited.
func TestStepsAfterTheHarnessWaitForFlush(t *testing.T) {
	var output bytes.Buffer
	if New("", &output) != nil || New("http", &output) != nil {
		t.Fatal("timing enabled without GEAS_DEBUG=timing")
	}
	timer := New("http,timing", &output)
	gitlab := timer.Child("gitlab:gitlab.com")
	timer.Step("config load")
	timer.HarnessStart()
	before := output.String()
	var wg sync.WaitGroup
	for _, child := range []*Timer{gitlab, timer.Child("github:github.com")} {
		wg.Add(1)
		go func() { defer wg.Done(); child.Step("ready") }()
	}
	wg.Wait()
	gitlab.Step("mint")
	if output.String() != before {
		t.Fatalf("printed while the harness ran: %q", output.String())
	}
	for _, want := range []string{"config load", "harness start"} {
		if !strings.Contains(before, want) {
			t.Errorf("timing log lacks %q: %q", want, before)
		}
	}
	timer.Flush()
	after := strings.TrimPrefix(output.String(), before)
	for _, want := range []string{"geas timing: gitlab:gitlab.com ready", "geas timing: github:github.com ready", "s (+0."} {
		if !strings.Contains(after, want) {
			t.Errorf("flushed lines lack %q: %q", want, after)
		}
	}
	if !strings.HasSuffix(after, "\n") || !strings.Contains(after, "gitlab:gitlab.com mint") || strings.Index(after, "gitlab:gitlab.com mint") < strings.Index(after, "gitlab:gitlab.com ready") {
		t.Errorf("flushed lines out of order: %q", after)
	}
	// After the flush, steps print as they happen.
	timer.Step("teardown")
	if !strings.Contains(output.String(), "geas timing: teardown") {
		t.Errorf("teardown not logged: %q", output.String())
	}
}

// A nil timer, GEAS_DEBUG without timing, does nothing.
func TestNilTimer(t *testing.T) {
	var timer *Timer
	timer.Child("x").Step("y")
	timer.HarnessStart()
	timer.Flush()
}
