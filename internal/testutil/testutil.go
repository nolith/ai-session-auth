// Package testutil holds the fixtures and fakes several test packages share:
// configuration values, an HTTP transport answering as a provider would, the
// fake glab script the provider and session tests run, session directories
// and golden files.
package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/sessiondir"
)

// Bool returns a pointer to v, for the *bool enabled fields of a provider.
func Bool(v bool) *bool { return &v }

// Config is a valid configuration with a GitLab and a GitHub provider, GitLab()
// and GitHub().
func Config() config.Config {
	return config.Config{SessionHours: 24, GitLab: []config.GitLabConfig{GitLab()}, GitHub: []config.GitHubConfig{GitHub()}}
}

// GitLab is gitlab.com with a stored issuer PAT, allowing the one project the
// fake glab answers for, group/project (ID 42).
func GitLab() config.GitLabConfig {
	return config.GitLabConfig{Host: "gitlab.com", Issuer: config.Issuer{Type: "bearer", TokenFile: "issuer.txt"}, ExpectedUser: "alice", Namespaces: []string{"group/project"}, Permissions: []string{"read_project", "push_code"}}
}

// GitHub is github.com for alice, allowing alice/repo.
func GitHub() config.GitHubConfig {
	return config.GitHubConfig{Host: "github.com", ClientID: "app", ExpectedUser: "alice", StateFile: "state.json", Repositories: []string{"alice/repo"}}
}

// NamespacedConfig allows two groups (one a subgroup), alice's own namespace
// and one project elsewhere.
func NamespacedConfig() config.GitLabConfig {
	c := GitLab()
	c.Namespaces = []string{"group", "other/sub", "alice", "elsewhere/project"}
	return c
}

// GlabConfig is GitLab() with the glab issuer, which needs no token file at
// all.
func GlabConfig() config.GitLabConfig {
	c := GitLab()
	c.Issuer = config.Issuer{Type: "glab"}
	return c
}

// Transport answers every request with handler's status code and its JSON
// body, so a provider's HTTP calls need no network.
func Transport(handler func(*http.Request) (int, any)) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, value := handler(r)
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// FakeGlab writes a glab into a new directory and returns it. Each call
// records its arguments (NUL-separated), environment and directory there, and
// is answered as gitlab.com would for alice and group/project (ID 42). The id
// lookup finds only the names the mutation minted. With FAKE_GLAB_BLOCK set,
// the mutation mints, creates "mutation" and waits for a "release" file
// before it answers, as a slow mint would; FAKE_GLAB_DELAY makes it answer
// that many seconds later.
func FakeGlab(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
log=` + quote(dir) + `
n=$(($(cat "$log/count" 2>/dev/null || echo 0) + 1))
echo "$n" >"$log/count"
printf '%s\0' "$@" >"$log/args.$n"
env >"$log/env.$n"
pwd >"$log/cwd.$n"
ps -o pgid= -p $$ | tr -d ' ' >"$log/pgid.$n"
if [ -n "${FAKE_GLAB_FAIL-}" ]; then
	echo 'glab-secret on stdout'
	echo 'glab-secret on stderr' >&2
	exit 1
fi
method=
for last; do
	[ "${previous-}" = --method ] && method=$last
	previous=$last
done
case "$method $last" in
"GET user") echo '{"username":"alice"}' ;;
"GET projects/42") echo '{"path_with_namespace":"group/project"}' ;;
" input="*)
	name=$(printf '%s' "$last" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')
	printf '%s\n' "$name" >>"$log/minted"
	printf '%s' "$name" >"$log/name"
	[ -n "${FAKE_GLAB_DELAY-}" ] && sleep "$FAKE_GLAB_DELAY"
	if [ -n "${FAKE_GLAB_BLOCK-}" ]; then
		touch "$log/mutation"
		while [ ! -e "$log/release" ]; do sleep 0.05; done
	fi
	echo '{"data":{"personalAccessTokenCreate":{"token":"session-secret","errors":[]}}}'
	;;
" query="*)
	case "$last" in
	*personalAccessTokens*)
		name=$(printf '%s' "$last" | sed -n 's/.*search: "\([^"]*\)".*/\1/p')
		if ! grep -qxF "$name" "$log/minted" 2>/dev/null; then
			echo '{"data":{"currentUser":{"personalAccessTokens":{"nodes":[]}}}}'
			exit 0
		fi
		printf '{"data":{"currentUser":{"personalAccessTokens":{"nodes":[{"id":"gid://gitlab/PersonalAccessToken/99","name":"%s-other"},{"id":"gid://gitlab/PersonalAccessToken/42","name":"%s"}]}}}}\n' "$name" "$name"
		;;
	*'n0: namespace(fullPath: "group/project")'*)
		echo '{"data":{"currentUser":{"username":"alice"},"n0":{"id":"gid://gitlab/Namespaces::ProjectNamespace/1042","fullPath":"group/project"},"p0":{"id":"gid://gitlab/Project/42","fullPath":"group/project"}}}'
		;;


	*)
		echo "unexpected query" >&2
		exit 1
		;;
	esac
	;;
"GET personal_access_tokens/42")
	active=true
	[ -e "$log/revoked" ] && active=false
	printf '{"id":42,"name":"%s","active":%s}\n' "$(cat "$log/name")" "$active"
	;;
"DELETE personal_access_tokens/42") touch "$log/revoked" ;;
*)
	echo "unexpected $method $last" >&2
	exit 1
	;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "glab"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Calls returns the calls a fake glab recorded, in order.
func Calls(t *testing.T, dir string) [][]string {
	t.Helper()
	var calls [][]string
	for n := 1; ; n++ {
		raw, err := os.ReadFile(filepath.Join(dir, "args."+strconv.Itoa(n)))
		if errors.Is(err, os.ErrNotExist) {
			return calls
		}
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"))
	}
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// SessionDir creates a session directory under a new XDG_STATE_HOME.
func SessionDir(t *testing.T) *sessiondir.Dir {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id, err := sessiondir.NewID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := sessiondir.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Golden compares got with the golden file at path, below the test's
// package. GEAS_UPDATE_GOLDEN=1 rewrites the file instead.
func Golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if os.Getenv("GEAS_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; got:\n%s", path, got)
	}
}
