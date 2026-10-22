package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/sessiondir"
	"github.com/nolith/geas/internal/testutil"
)

// mockAPI is the shared transport fake, wired to this package's API.
func mockAPI(t *testing.T, handler func(*http.Request) (int, any)) *api.API {
	t.Helper()
	a := api.New()
	a.Client.Transport = testutil.Transport(handler)
	return a
}

// lookupAlias matches one aliased group or project of the lookup document.
var lookupAlias = regexp.MustCompile(`(\w+): (group|project)\(fullPath: ("(?:[^"\\]|\\.)*")\)`)

// graphqlPayload decodes a GraphQL request's document and variables.
func graphqlPayload(t *testing.T, r *http.Request) (string, map[string]json.RawMessage) {
	t.Helper()
	var payload struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Query, payload.Variables
}

// lookupAnswers answers the lookup document as GitLab would: every group or
// project alias resolves to the node answers holds for its path, and a path
// with no node resolves to null. The earlier format's projects(ids:) alias
// resolves to ids instead. The test fails on an alias it did not expect.
func lookupAnswers(t *testing.T, query, user string, answers map[string]any, ids []any) map[string]any {
	t.Helper()
	data := map[string]any{"currentUser": map[string]any{"username": user}}
	for _, match := range lookupAlias.FindAllStringSubmatch(query, -1) {
		alias, kind, quoted := match[1], match[2], match[3]
		var path string
		if err := json.Unmarshal([]byte(quoted), &path); err != nil {
			t.Fatalf("alias %s: %v", alias, err)
		}
		node, ok := answers[path]
		if !ok {
			t.Errorf("the document asks for %s %s, which the test does not answer", kind, path)
		}
		data[alias] = node
	}
	if strings.Contains(query, "projects(ids:") {
		data["l0"] = map[string]any{"nodes": ids}
	}
	return data
}

// testLedger is a new session's ledger in a temporary state directory.
func testLedger(t *testing.T) *ledger.Ledger {
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
	l, err := ledger.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// mintAnswer is what GitLab answers to personalAccessTokenCreate.
func mintAnswer() map[string]any {
	return map[string]any{"data": map[string]any{"personalAccessTokenCreate": map[string]any{"token": "session-secret", "errors": []string{}}}}
}

// patSearch matches the inlined name of the id lookup document.
var patSearch = regexp.MustCompile(`personalAccessTokens\(search: ("(?:[^"\\]|\\.)*")\)`)

// patName reads the name the id lookup asks for.
func patName(t *testing.T, query string) string {
	t.Helper()
	match := patSearch.FindStringSubmatch(query)
	if match == nil {
		t.Fatalf("the document searches for a PAT in %s", query)
	}
	var name string
	if err := json.Unmarshal([]byte(match[1]), &name); err != nil {
		t.Fatal(err)
	}
	return name
}

// patAnswer answers the id lookup as a search does: the token asked for, among
// another whose name merely starts the same way.
func patAnswer(t *testing.T, query string) map[string]any {
	t.Helper()
	name := patName(t, query)
	return map[string]any{"data": map[string]any{"currentUser": map[string]any{"personalAccessTokens": map[string]any{"nodes": []any{
		map[string]any{"id": "gid://gitlab/PersonalAccessToken/99", "name": name + "-other"},
		map[string]any{"id": "gid://gitlab/PersonalAccessToken/42", "name": name},
	}}}}}
}

// issuerAnswer answers every document the issuer sends as GitLab would: the
// lookup, the id lookup, and the mint. It reports the mint, so the caller can
// read the input whose name and scopes it wants to check.
func issuerAnswer(t *testing.T, query, user string, answers map[string]any, ids []any) (minted bool, answer any) {
	t.Helper()
	switch {
	case strings.Contains(query, "personalAccessTokenCreate"):
		return true, mintAnswer()
	case strings.Contains(query, "personalAccessTokens(search:"):
		return false, patAnswer(t, query)
	}
	return false, map[string]any{"data": lookupAnswers(t, query, user, answers, ids)}
}

func TestGitLabCreateAndRevokeExactPAT(t *testing.T) {
	c := testutil.Config().GitLab
	name := ""
	deleted := false
	book := testLedger(t)
	// The earlier format resolves its project IDs, then checks the paths.
	ids := []any{map[string]any{"id": "gid://gitlab/Project/42", "fullPath": "group/project"}}
	api := mockAPI(t, func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "Bearer issuer" {
			t.Error("incorrect credential")
		}
		switch {
		case r.URL.Path == "/api/graphql":
			query, variables := graphqlPayload(t, r)
			// The id lookup must ask for the PAT the mint created.
			if strings.Contains(query, "personalAccessTokens(search:") && patName(t, query) != name {
				t.Errorf("the id lookup asks for %q, the session PAT is %q", patName(t, query), name)
			}
			minted, answer := issuerAnswer(t, query, "alice", nil, ids)
			if !minted {
				return 200, answer
			}
			var input struct {
				Name      string         `json:"name"`
				ExpiresAt string         `json:"expiresAt"`
				Scopes    []config.Scope `json:"granularScopes"`
			}
			if err := json.Unmarshal(variables["input"], &input); err != nil {
				t.Error(err)
			}
			name = input.Name
			if !strings.HasPrefix(name, "geas-") || input.ExpiresAt != "2026-10-10" || len(input.Scopes) != 2 {
				t.Error("bad PAT input")
			}
			// The name is on disk before the mint, without an ID yet.
			if entries, err := book.Entries(); err != nil || len(entries) != 1 || entries[0] != (ledger.Entry{Provider: "gitlab:gitlab.com", Name: name, Created: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}) {
				t.Errorf("ledger at the mint: %+v %v", entries, err)
			}
			return 200, answer
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v4/personal_access_tokens/42":
			deleted = true
			return 204, nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			return 500, nil
		}
	})
	g := &Provider{Config: c, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	token, err := g.create(context.Background(), book, 24*time.Hour)
	if err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	entries, err := book.Entries()
	if err != nil || len(entries) != 1 || entries[0].Name != name || entries[0].ID != 42 || entries[0].Revoked {
		t.Fatalf("ledger after the id lookup: %+v %v", entries, err)
	}
	// Minted here: a single DELETE, no check first.
	if err = g.Revoke(context.Background(), entries[0]); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("PAT not revoked")
	}
}

func TestGitLabProjectMismatchStopsBeforeMint(t *testing.T) {
	minted := false
	book := testLedger(t)
	api := mockAPI(t, func(r *http.Request) (int, any) {
		query, _ := graphqlPayload(t, r)
		var answer any
		minted, answer = issuerAnswer(t, query, "alice", nil, []any{map[string]any{"id": "gid://gitlab/Project/42", "fullPath": "wrong/project"}})
		return 200, answer
	})
	g := &Provider{Config: testutil.Config().GitLab, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
	if _, err := g.create(context.Background(), book, time.Hour); err == nil || minted {
		t.Fatal("minted for wrong project")
	}
}

func TestGitLabUserMismatchStopsBeforeMint(t *testing.T) {
	minted := false
	book := testLedger(t)
	api := mockAPI(t, func(r *http.Request) (int, any) {
		query, _ := graphqlPayload(t, r)
		var answer any
		minted, answer = issuerAnswer(t, query, "someone-else", nil, []any{map[string]any{"id": "gid://gitlab/Project/42", "fullPath": "group/project"}})
		return 200, answer
	})
	g := &Provider{Config: testutil.Config().GitLab, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
	if _, err := g.create(context.Background(), book, time.Hour); err == nil || minted {
		t.Fatal("minted for another user")
	}
}

func TestGlabIssuerCreateAndRevoke(t *testing.T) {
	dir := testutil.FakeGlab(t)
	path := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+path)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	for _, key := range glabUnset {
		t.Setenv(key, "outer-secret")
	}
	g, err := New(testutil.GlabConfig(), 24*time.Hour, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// After New, which has the lock's path already.
	book := testLedger(t)
	// A glab found on PATH later must not be used: the issuer resolved its own.
	decoy := t.TempDir()
	os.WriteFile(filepath.Join(decoy, "glab"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	t.Setenv("PATH", decoy+string(os.PathListSeparator)+path)
	g.Now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	token, err := g.create(context.Background(), book, 24*time.Hour)
	if err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	entries, err := book.Entries()
	if err != nil || len(entries) != 1 {
		t.Fatalf("ledger: %+v %v", entries, err)
	}
	if err = g.Revoke(context.Background(), entries[0]); err != nil {
		t.Fatal(err)
	}
	calls := testutil.Calls(t, dir)
	if len(calls) != 4 {
		t.Fatalf("got %d glab calls: %q", len(calls), calls)
	}
	host := []string{"api", "--hostname", "gitlab.com"}
	for i, call := range calls {
		if !slices.Equal(call[:3], host) {
			t.Errorf("call %d not pinned to gitlab.com: %q", i+1, call)
		}
	}
	if !slices.Equal(calls[0][3:], []string{"graphql", "-f", "query=" + lookupDocument(testutil.GlabConfig())}) {
		t.Errorf("lookup: %q", calls[0])
	}
	mint := calls[1][3:]
	if len(mint) != 5 || mint[0] != "graphql" || mint[1] != "-f" || mint[2] != "query="+createPAT || mint[3] != "-F" || !strings.HasPrefix(mint[4], "input=") {
		t.Fatalf("mint: %q", mint)
	}
	var input struct {
		Name      string         `json:"name"`
		ExpiresAt string         `json:"expiresAt"`
		Scopes    []config.Scope `json:"granularScopes"`
	}
	if err = json.Unmarshal([]byte(strings.TrimPrefix(mint[4], "input=")), &input); err != nil {
		t.Fatal(err)
	}
	name := input.Name
	if !strings.HasPrefix(name, "geas-") || input.ExpiresAt != "2026-10-10" || len(input.Scopes) != 2 || input.Scopes[1].ResourceIDs[0] != "gid://gitlab/Project/42" {
		t.Errorf("bad PAT input %+v", input)
	}
	if !slices.Equal(calls[2][3:], []string{"graphql", "-f", "query=" + findDocument(name)}) {
		t.Errorf("id lookup: %q", calls[2])
	}
	if !slices.Equal(calls[3][3:], []string{"--method", "DELETE", "personal_access_tokens/42"}) {
		t.Errorf("revocation: %q", calls[3])
	}
	for n := 1; n <= len(calls); n++ {
		raw, _ := os.ReadFile(filepath.Join(dir, "env."+strconv.Itoa(n)))
		env := "\n" + string(raw)
		for _, key := range glabUnset {
			if strings.Contains(env, "\n"+key+"=") {
				t.Errorf("call %d inherited %s", n, key)
			}
		}
		if !strings.Contains(env, "\nHOME=") {
			t.Errorf("call %d lost HOME, which glab needs for its login", n)
		}
		cwd, _ := os.ReadFile(filepath.Join(dir, "cwd."+strconv.Itoa(n)))
		if strings.TrimSpace(string(cwd)) != "/" {
			t.Errorf("call %d ran in %q", n, cwd)
		}
		pgid, _ := os.ReadFile(filepath.Join(dir, "pgid."+strconv.Itoa(n)))
		if group, err := strconv.Atoi(strings.TrimSpace(string(pgid))); err != nil || group == syscall.Getpgrp() {
			t.Errorf("call %d shares the launcher's process group: %q %v", n, pgid, err)
		}
	}
	info, err := os.Stat(filepath.Join(state, "geas", "glab.lock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lock: %v %v", info, err)
	}
}

func TestGlabIssuerFailureIsSilent(t *testing.T) {
	dir := testutil.FakeGlab(t)
	t.Setenv("FAKE_GLAB_FAIL", "1")
	issuer := glabIssuer{executable: filepath.Join(dir, "glab"), lockPath: filepath.Join(t.TempDir(), "glab")}
	captured, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = captured, captured
	restErr := issuer.rest(context.Background(), http.MethodGet, "user", new(any))
	graphqlErr := issuer.graphql(context.Background(), createPAT, map[string]any{"input": map[string]any{}}, new(any))
	os.Stdout, os.Stderr = stdout, stderr
	for _, err := range []error{restErr, graphqlErr} {
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe error %v", err)
		}
	}
	if raw, _ := os.ReadFile(captured.Name()); len(raw) != 0 {
		t.Fatalf("glab output reached the launcher's: %q", raw)
	}
	if len(testutil.Calls(t, dir)) != 2 {
		t.Fatal("glab not called")
	}
}

func TestGlabIssuerRejectsScalarVariables(t *testing.T) {
	dir := testutil.FakeGlab(t)
	issuer := glabIssuer{executable: filepath.Join(dir, "glab"), lockPath: filepath.Join(t.TempDir(), "glab")}
	if issuer.graphql(context.Background(), createPAT, map[string]any{"name": "x"}, new(any)) == nil {
		t.Fatal("sent a JSON string through -F")
	}
	if len(testutil.Calls(t, dir)) != 0 {
		t.Fatal("called glab")
	}
}

// TestLookupDocument pins the one aliased query that replaces the REST user
// call and the per-namespace lookups, and the escaping of a path in it.
func TestLookupDocument(t *testing.T) {
	c := testutil.NamespacedConfig()
	c.Groups, c.Projects, c.PersonalProjects = []string{"group", "other/sub"}, []string{"elsewhere/project"}, false
	want := `query SessionLookup {
  currentUser { username }
  g0: group(fullPath: "group") { id fullPath }
  g1: group(fullPath: "other/sub") { id fullPath }
  p0: project(fullPath: "elsewhere/project") { id fullPath }
}
`
	if got := lookupDocument(c); got != want {
		t.Fatalf("document:\n%s", got)
	}
	// The earlier format asks for its project IDs, one node per ID.
	c = testutil.Config().GitLab
	want = `query SessionLookup {
  currentUser { username }
  l0: projects(ids: ["gid://gitlab/Project/42"]) { nodes { id fullPath } }
}
`
	if got := lookupDocument(c); got != want {
		t.Fatalf("earlier format:\n%s", got)
	}
	// A quote in a path would end the string literal, so it is escaped.
	c = config.GitLabConfig{Groups: []string{`group"x`}}
	if got, want := lookupDocument(c), `g0: group(fullPath: "group\"x")`; !strings.Contains(got, want) {
		t.Fatalf("unescaped path: %s", got)
	}
}

func TestGitLabNamespacedScopes(t *testing.T) {
	var scopes []config.Scope
	book := testLedger(t)
	lookups := 0
	answers := map[string]any{
		"group":             map[string]any{"id": "gid://gitlab/Group/7", "fullPath": "group"},
		"other/sub":         map[string]any{"id": "gid://gitlab/Group/8", "fullPath": "other/sub"},
		"elsewhere/project": map[string]any{"id": "gid://gitlab/Project/42", "fullPath": "elsewhere/project"},
	}
	api := mockAPI(t, func(r *http.Request) (int, any) {
		switch r.URL.Path {
		case "/api/graphql":
			query, variables := graphqlPayload(t, r)
			if strings.Contains(query, "personalAccessTokens(search:") {
				return 200, patAnswer(t, query)
			}
			minted, answer := issuerAnswer(t, query, "alice", answers, nil)
			if !minted {
				lookups++
				return 200, answer
			}
			var input struct {
				Scopes []config.Scope `json:"granularScopes"`
			}
			if err := json.Unmarshal(variables["input"], &input); err != nil {
				t.Error(err)
			}
			scopes = input.Scopes
			return 200, answer
		case "/api/v4/personal_access_tokens":
			return 200, []map[string]any{{"id": 1, "name": r.URL.Query().Get("search")}}
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		return 404, nil
	})
	g := &Provider{Config: testutil.NamespacedConfig(), Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
	if token, err := g.create(context.Background(), book, time.Hour); err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	// The user and every namespace come from one document.
	if lookups != 1 {
		t.Fatalf("%d lookup queries", lookups)
	}
	permissions := []string{"read_project", "push_code"}
	want := []config.Scope{
		{Access: "USER", Permissions: []string{"read_user"}},
		{Access: "SELECTED_MEMBERSHIPS", ResourceIDs: []string{"gid://gitlab/Group/7", "gid://gitlab/Group/8", "gid://gitlab/Project/42"}, Permissions: permissions},
		{Access: "PERSONAL_PROJECTS", Permissions: permissions},
	}
	if got, _ := json.Marshal(scopes); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("scopes %s", got)
	}
	// Without personal projects or explicit lists, only what is configured.
	c := testutil.NamespacedConfig()
	c.Groups, c.Projects = nil, nil
	g = &Provider{Config: c, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
	if _, err := g.create(context.Background(), book, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(scopes); string(got) != string(must(json.Marshal([]config.Scope{want[0], want[2]}))) {
		t.Fatalf("personal-only scopes %s", got)
	}
}

func TestGitLabNamespaceMismatchStopsBeforeMint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer any
	}{
		{"renamed group", map[string]any{"id": "gid://gitlab/Group/7", "fullPath": "renamed"}},
		{"case differs", map[string]any{"id": "gid://gitlab/Group/7", "fullPath": "Group"}},
		{"no id", map[string]any{"fullPath": "group"}},
		{"another model's id", map[string]any{"id": "gid://gitlab/Project/7", "fullPath": "group"}},
		{"project fields for a group", map[string]any{"id": "gid://gitlab/Group/7", "pathWithNamespace": "group"}},
		{"missing path", nil},
	} {
		minted := false
		book := testLedger(t)
		api := mockAPI(t, func(r *http.Request) (int, any) {
			query, _ := graphqlPayload(t, r)
			var answer any
			minted, answer = issuerAnswer(t, query, "alice", map[string]any{"group": tc.answer}, nil)
			return 200, answer
		})
		c := testutil.NamespacedConfig()
		c.Groups, c.Projects = []string{"group"}, nil
		g := &Provider{Config: c, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
		if _, err := g.create(context.Background(), book, time.Hour); err == nil || minted {
			t.Errorf("%s: minted (err %v)", tc.name, err)
		}
		if entries, err := book.Entries(); err != nil || len(entries) != 0 {
			t.Errorf("%s: a refused lookup reached the ledger: %+v %v", tc.name, entries, err)
		}
	}
}

// A mint GitLab answers with errors creates nothing, so its entry is settled
// at once; a mint whose answer never arrives stays open for the reaper.
func TestRefusedMintSettlesItsEntry(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func() (int, any)
		open bool
	}{
		{"refused", func() (int, any) {
			return 200, map[string]any{"data": map[string]any{"personalAccessTokenCreate": map[string]any{"token": nil, "errors": []string{"granular scopes are invalid"}}}}
		}, false},
		{"no answer", func() (int, any) { return 502, map[string]any{"message": "bad gateway"} }, true},
	} {
		book := testLedger(t)
		api := mockAPI(t, func(r *http.Request) (int, any) {
			query, _ := graphqlPayload(t, r)
			if strings.Contains(query, "personalAccessTokenCreate") {
				return tc.mint()
			}
			_, answer := issuerAnswer(t, query, "alice", map[string]any{"group": map[string]any{"id": "gid://gitlab/Group/7", "fullPath": "group"}}, nil)
			return 200, answer
		})
		c := testutil.NamespacedConfig()
		c.Groups, c.Projects = []string{"group"}, nil
		g := &Provider{Config: c, Issuer: bearerIssuer{client: api, token: "issuer"}, Now: time.Now}
		if _, err := g.create(context.Background(), book, time.Hour); err == nil {
			t.Fatalf("%s: the mint succeeded", tc.name)
		}
		entries, err := book.Entries()
		if err != nil || len(entries) != 1 {
			t.Fatalf("%s: entries %+v %v", tc.name, entries, err)
		}
		if open, _ := book.Unrevoked(); (len(open) == 1) != tc.open {
			t.Errorf("%s: open entries %+v, want open=%v", tc.name, open, tc.open)
		}
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// The reaper revokes from a fresh process: it finds a PAT by name when the
// ledger has no ID, and checks that the ID still names an active session PAT
// before the DELETE.
func TestRevokeFromLedger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      int64
		found   bool
		current map[string]any
		deleted bool
		want    error
	}{
		{"active", 42, true, map[string]any{"name": "geas-x", "active": true}, true, nil},
		{"already revoked", 42, true, map[string]any{"name": "geas-x", "active": false}, false, ledger.ErrGone},
		{"another token", 42, true, map[string]any{"name": "someone-else", "active": true}, false, ErrOtherPAT},
		{"found by name", 0, true, map[string]any{"name": "geas-x", "active": true}, true, nil},
		{"never minted", 0, false, nil, false, ledger.ErrNotFound},
	} {
		deleted, searched := false, false
		api := mockAPI(t, func(r *http.Request) (int, any) {
			switch {
			case r.URL.Path == "/api/graphql":
				searched = true
				query, _ := graphqlPayload(t, r)
				if !tc.found {
					return 200, map[string]any{"data": map[string]any{"currentUser": map[string]any{"personalAccessTokens": map[string]any{"nodes": []any{}}}}}
				}
				return 200, patAnswer(t, query)
			case r.URL.Path != "/api/v4/personal_access_tokens/42":
				t.Errorf("%s: unexpected %s %s", tc.name, r.Method, r.URL)
				return 404, nil
			case r.Method == http.MethodDelete:
				deleted = true
				return 204, nil
			}
			return 200, tc.current
		})
		g := &Provider{Issuer: bearerIssuer{client: api, token: "issuer"}}
		err := g.Revoke(context.Background(), ledger.Entry{Provider: "gitlab:gitlab.com", Name: "geas-x", ID: tc.id})
		if deleted != tc.deleted || searched != (tc.id == 0) || !errors.Is(err, tc.want) || (err == nil) != (tc.want == nil) {
			t.Errorf("%s: deleted %v, searched %v, err %v", tc.name, deleted, searched, err)
		}
	}
}
