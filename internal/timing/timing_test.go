package timing

import (
	"bytes"
	"strings"
	"testing"
)

// The timing log measures the steps before the harness, then goes quiet: a
// line once the harness owns the terminal would corrupt an agent's TUI.
func TestStepTimerSilencesAfterTheHarness(t *testing.T) {
	var output bytes.Buffer
	if New("", &output) != nil || New("http", &output) != nil {
		t.Fatal("timing enabled without GEAS_DEBUG=timing")
	}
	timer := New("http,timing", &output)
	timer.Step("config load")
	timer.Stop()
	before := output.String()
	timer.Step("mint")
	if output.String() != before {
		t.Fatalf("printed after the harness: %q", output.String())
	}
	for _, want := range []string{"config load", "harness start"} {
		if !strings.Contains(before, want) {
			t.Errorf("timing log lacks %q: %q", want, before)
		}
	}
}
