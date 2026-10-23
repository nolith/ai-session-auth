// Package credhelper is `geas credential`, the Git credential helper a
// session's gitconfig names for each host. It runs inside the session, finds
// it through GEAS_SESSION_DIR, and answers from the session directory alone:
// the hosts and allowlists the session recorded, the providers' markers and
// their secrets.
package credhelper

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/sessiondir"
)

// hostsFile lists the hosts the helper answers for, written once by the
// session before the harness starts.
const hostsFile = "git/hosts.json"

// Host is one provider.GitHost as the session records it, with the provider
// whose secret answers for it.
type Host struct {
	Provider     string   `json:"provider"`
	Host         string   `json:"host"`
	Username     string   `json:"username"`
	Namespaces   []string `json:"namespaces,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	FoldCase     bool     `json:"fold_case,omitempty"`
}

func (h Host) gitHost() provider.GitHost {
	return provider.GitHost{Host: h.Host, Username: h.Username, Namespaces: h.Namespaces, Repositories: h.Repositories, FoldCase: h.FoldCase}
}

// Allows reports whether the helper hands this host's credential out for a
// repository path, as Git sends it.
func (h Host) Allows(path string) bool { return h.gitHost().Allows(path) }

// Hosts collects every provider's Git hosts, and refuses a host two
// providers claim.
func Hosts(providers []provider.Provider) ([]Host, error) {
	var hosts []Host
	seen := map[string]string{}
	for _, p := range providers {
		for _, g := range p.Setup().Git {
			if other, ok := seen[g.Host]; ok {
				return nil, fmt.Errorf("providers %s and %s both serve Git on %s", other, p.ID(), g.Host)
			}
			seen[g.Host] = p.ID()
			hosts = append(hosts, Host{Provider: p.ID(), Host: g.Host, Username: g.Username, Namespaces: g.Namespaces, Repositories: g.Repositories, FoldCase: g.FoldCase})
		}
	}
	return hosts, nil
}

// WriteHosts records the hosts in the session directory for the helper.
func WriteHosts(dir *sessiondir.Dir, hosts []Host) error {
	raw, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	return dir.WriteFile(hostsFile, raw)
}

// Run serves `geas credential <operation>` for the session GEAS_SESSION_DIR
// names. Only get answers; Git's store and erase have nothing to do, since
// the session owns its credentials.
func Run(operation string, input io.Reader, output io.Writer) error {
	if operation != "get" {
		return nil
	}
	dir, err := sessiondir.FromEnv()
	if err != nil {
		return err
	}
	return Get(dir, input, output)
}

// Get answers one Git credential request from dir. Outside the allowlist,
// or for another protocol or host, it answers nothing, and Git, with no other
// helper and no prompt, fails. For a provider still pending it waits, up to
// markerWait; for one that failed, or is still not ready, it tells Git to
// quit, with the reason, so that nothing falls back to the user's own
// credentials.
func Get(dir *sessiondir.Dir, input io.Reader, output io.Writer) error {
	request, err := readRequest(input)
	if err != nil {
		return err
	}
	if request["protocol"] != "https" {
		return nil
	}
	raw, err := dir.ReadFile(hostsFile)
	if err != nil {
		return err
	}
	var hosts []Host
	if err = json.Unmarshal(raw, &hosts); err != nil {
		return errors.New("invalid session host list")
	}
	var host *Host
	for i := range hosts {
		if hosts[i].Host == request["host"] {
			host = &hosts[i]
		}
	}
	if host == nil || !host.Allows(request["path"]) {
		return nil
	}
	marker, err := waitMarker(dir, host.Provider)
	switch {
	case err != nil:
		return err
	case marker.State == sessiondir.Pending:
		return quit(output, fmt.Sprintf("%s credentials are still pending after %s; see geas status", host.Host, markerWait))
	case marker.State != sessiondir.Ready:
		return quit(output, host.Host+" is not available in this session: "+marker.Reason)
	}
	file, err := sessiondir.ProviderFile("secrets", host.Provider)
	if err != nil {
		return err
	}
	secret, err := dir.ReadFile(file)
	if err != nil {
		return err
	}
	if len(secret) == 0 || strings.ContainsAny(string(secret)+host.Username, "\r\n\x00") {
		return errors.New("invalid session credential")
	}
	_, err = fmt.Fprintf(output, "username=%s\npassword=%s\n\n", host.Username, secret)
	return err
}

// markerWait is how long the helper waits for a pending provider: a GitLab
// mint takes about 3 s, so 30 s covers a slow network ten times over and
// still fails well inside an agent's tool timeout. markerPoll is how often
// it looks. Variables for the tests only.
var markerWait, markerPoll = 30 * time.Second, 50 * time.Millisecond

// waitMarker returns provider's marker once it is no longer pending, or the
// pending one after markerWait: the session acquires credentials in the
// background, and Git, unlike glab or an agent, cannot retry on its own. A
// provider without a marker yet is pending too.
func waitMarker(dir *sessiondir.Dir, provider string) (sessiondir.Marker, error) {
	deadline := time.Now().Add(markerWait)
	for {
		marker, err := dir.Marker(provider)
		if errors.Is(err, os.ErrNotExist) {
			marker, err = sessiondir.Marker{State: sessiondir.Pending}, nil
		}
		if err != nil || marker.State != sessiondir.Pending || !time.Now().Before(deadline) {
			return marker, err
		}
		time.Sleep(markerPoll)
	}
}

// quit tells Git to stop at once, with no askpass or terminal prompt, and
// returns the reason as the error the helper prints.
func quit(output io.Writer, reason string) error {
	fmt.Fprint(output, "quit=1\n\n")
	return errors.New(reason)
}

// readRequest reads Git's key=value lines, up to the blank line or EOF.
func readRequest(input io.Reader) (map[string]string, error) {
	scanner := bufio.NewScanner(io.LimitReader(input, 8192))
	values := map[string]string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("invalid Git credential request")
	}
	return values, nil
}
