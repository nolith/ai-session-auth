//go:build linux

package session

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// setNoNewPrivs, in a process of its own since the flag cannot be cleared:
// every thread has it afterwards, or, in a binary built with cgo, as the
// race detector's test binaries are, it refuses and says why.
func TestSetNoNewPrivs(t *testing.T) {
	if os.Getenv("GEAS_TEST_NO_NEW_PRIVS") == "1" {
		if err := setNoNewPrivs(); err != nil {
			os.Stdout.WriteString("error: " + err.Error() + "\n")
			os.Exit(0)
		}
		// Every thread, not only the one that made the call.
		threads, _ := os.ReadDir("/proc/self/task")
		for _, thread := range threads {
			status, _ := os.ReadFile("/proc/self/task/" + thread.Name() + "/status")
			for _, line := range strings.Split(string(status), "\n") {
				if strings.HasPrefix(line, "NoNewPrivs:") {
					os.Stdout.WriteString(line + "\n")
				}
			}
		}
		os.Exit(0)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSetNoNewPrivs$")
	command.Env = append(os.Environ(), "GEAS_TEST_NO_NEW_PRIVS=1")
	raw, err := command.Output()
	output := string(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if cgoEnabled {
		if output != "error: cannot set no_new_privs: this geas was built with cgo; build it with CGO_ENABLED=0\n" {
			t.Fatalf("with cgo: %q", output)
		}
		return
	}
	if lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n"); len(lines) < 2 || strings.Count(output, "NoNewPrivs:\t1\n") != len(lines) {
		t.Fatalf("threads: %q", output)
	}
}
