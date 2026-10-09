package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockAPI(t *testing.T, handler func(*http.Request) (int, any)) *API {
	t.Helper()
	a := newAPI()
	a.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, value := handler(r)
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	})
	return a
}
func boolPointer(v bool) *bool { return &v }

// namespacedConfig allows two groups (one a subgroup), alice's personal
// projects and one project elsewhere.
func namespacedConfig() GitLabConfig {
	return GitLabConfig{ExpectedUser: "alice", IssuerTokenFile: "issuer.txt", Groups: []string{"group", "other/sub"}, PersonalProjects: true, Projects: []string{"elsewhere/project"}, Permissions: []string{"read_project", "push_code"}}
}
func testConfig() Config {
	return Config{SessionHours: 24, GitHub: GitHubConfig{ClientID: "app", ExpectedUser: "alice", StateFile: "state.json", Repositories: []string{"alice/repo"}}, GitLab: GitLabConfig{ExpectedUser: "alice", IssuerTokenFile: "issuer.txt", Repositories: []string{"group/project"}, ProjectIDs: []int64{42}, Scopes: []Scope{{Access: "USER", Permissions: []string{"read_user"}}, {Access: "SELECTED_MEMBERSHIPS", ResourceIDs: []string{"gid://gitlab/Project/42"}, Permissions: []string{"read_project", "push_code"}}}}}
}

func TestConfigRejectsBroaderScopeAndMissingPaths(t *testing.T) {
	c := testConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.GitLab.Scopes[1].ResourceIDs[0] = "gid://gitlab/Project/99"
	if c.validate() == nil {
		t.Fatal("accepted another project")
	}
	c = testConfig()
	c.GitLab.Scopes[1].Permissions = []string{"create_personal_access_token"}
	if c.validate() == nil {
		t.Fatal("accepted issuer permissions for agent")
	}
	c = testConfig()
	c.GitHub.StateFile = ""
	raw, _ := json.Marshal(c)
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, raw, 0600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("missing path expanded into directory")
	}
	c = testConfig()
	raw, _ = json.Marshal(c)
	os.WriteFile(path, append(raw, []byte(" {}")...), 0600)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}
func TestExpiryCoversSessionAtUTCMidnight(t *testing.T) {
	for _, tc := range []struct {
		now   string
		hours time.Duration
		want  string
	}{
		{"2026-10-08T10:00:00Z", 24, "2026-10-10"},
		{"2026-10-08T00:00:00Z", 24, "2026-10-09"},
		{"2026-10-08T23:30:00-02:00", 48, "2026-10-12"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		if got := expiryForDuration(now, tc.hours*time.Hour); got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.now, got, tc.want)
		}
	}
}
func TestPrivateReadAndAtomicRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := atomicWrite(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	raw, err := privateRead(path)
	if err != nil || string(raw) != "second" {
		t.Fatalf("%s %v", raw, err)
	}
	link := path + "link"
	os.Symlink(path, link)
	if _, err = privateRead(link); err == nil {
		t.Fatal("followed symlink")
	}
	os.Chmod(path, 0644)
	if _, err = privateRead(path); err == nil {
		t.Fatal("read world-readable secret")
	}
}
func TestFileLockCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withFileLock(context.Background(), path, func() error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := withFileLock(ctx, path, func() error { t.Error("entered held lock"); return nil })
	close(release)
	if other := <-done; other != nil {
		t.Fatal(other)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}
func TestGitHubConcurrentRotation(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	c := testConfig().GitHub
	c.StateFile = filepath.Join(t.TempDir(), "github.json")
	var calls atomic.Int32
	api := mockAPI(t, func(r *http.Request) (int, any) {
		calls.Add(1)
		if r.URL.Path != "/login/oauth/access_token" {
			t.Errorf("unexpected %s", r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_secret") != "" {
			t.Error("incorrect refresh request")
		}
		return 200, OAuthResponse{AccessToken: "ghu_new", RefreshToken: "rotated-refresh", ExpiresIn: 28800, RefreshExpiresIn: 15552000}
	})
	g := &GitHub{Config: c, API: api, Now: func() time.Time { return now }}
	writeJSON(c.StateFile, GitHubState{ClientID: c.ClientID, AccessToken: "ghu_old", RefreshToken: "old-refresh", ExpiresAt: float64(now.Unix()), RefreshExpiresAt: float64(now.Add(time.Hour).Unix())})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := g.token(context.Background())
			if err != nil || token != "ghu_new" {
				t.Errorf("token=%s err=%v", token, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}
	raw, _ := privateRead(c.StateFile)
	var state GitHubState
	json.Unmarshal(raw, &state)
	if state.RefreshToken != "rotated-refresh" {
		t.Fatal("rotation not persisted")
	}
}
func TestGitHubIdentityMismatch(t *testing.T) {
	api := mockAPI(t, func(r *http.Request) (int, any) { return 200, map[string]string{"login": "someone-else"} })
	g := &GitHub{Config: testConfig().GitHub, API: api, Now: time.Now}
	if g.verify(context.Background(), "ghu_test") == nil {
		t.Fatal("accepted wrong user")
	}
}
func TestGitLabCreateAndRevokeExactPAT(t *testing.T) {
	c := testConfig().GitLab
	name := ""
	deleted := false
	api := mockAPI(t, func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "Bearer issuer" {
			t.Error("incorrect credential")
		}
		switch {
		case r.URL.Path == "/api/v4/user":
			return 200, map[string]string{"username": "alice"}
		case r.URL.Path == "/api/v4/projects/42":
			return 200, map[string]string{"path_with_namespace": "group/project"}
		case r.URL.Path == "/api/graphql":
			var payload struct {
				Variables struct {
					Input struct {
						Name      string  `json:"name"`
						ExpiresAt string  `json:"expiresAt"`
						Scopes    []Scope `json:"granularScopes"`
					} `json:"input"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			name = payload.Variables.Input.Name
			if !strings.HasPrefix(name, "ai-session-") || payload.Variables.Input.ExpiresAt != "2026-10-10" || len(payload.Variables.Input.Scopes) != 2 {
				t.Error("bad PAT input")
			}
			return 200, map[string]any{"data": map[string]any{"personalAccessTokenCreate": map[string]any{"token": "session-secret", "errors": []string{}}}}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/personal_access_tokens":
			if r.URL.Query().Get("search") != name {
				t.Error("bad search")
			}
			return 200, []map[string]any{{"id": 99, "name": name + "-other"}, {"id": 42, "name": name}}
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v4/personal_access_tokens/42":
			deleted = true
			return 204, nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			return 500, nil
		}
	})
	g := &GitLab{Config: c, Issuer: bearerIssuer{api: api, token: "issuer"}, Now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }}
	token, err := g.create(context.Background(), 24*time.Hour)
	if err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	if err = g.revokeCreated(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !deleted || len(g.Created) != 0 {
		t.Fatal("PAT not revoked")
	}
}
func TestGitLabProjectMismatchStopsBeforeMint(t *testing.T) {
	minted := false
	api := mockAPI(t, func(r *http.Request) (int, any) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			return 200, map[string]string{"username": "alice"}
		}
		if r.URL.Path == "/api/graphql" {
			minted = true
		}
		return 200, map[string]string{"path_with_namespace": "wrong/project"}
	})
	g := &GitLab{Config: testConfig().GitLab, Issuer: bearerIssuer{api: api, token: "issuer"}, Now: time.Now}
	if _, err := g.create(context.Background(), time.Hour); err == nil || minted {
		t.Fatal("minted for wrong project")
	}
}
func TestAPIErrorsDoNotExposeSecrets(t *testing.T) {
	api := mockAPI(t, func(*http.Request) (int, any) { return 401, map[string]string{"error": "secret-from-server"} })
	err := api.request(context.Background(), "GET", "https://gitlab.com/api/v4/user", "issuer-secret", nil, false, new(any))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error %v", err)
	}
	if api.request(context.Background(), "GET", "http://gitlab.com", "token", nil, false, nil) == nil {
		t.Fatal("accepted HTTP")
	}
}

// fakeGlab writes a glab into a new directory and returns it. Each call
// records its arguments (NUL-separated), environment and directory there, and
// is answered as gitlab.com would for alice and group/project (ID 42).
func fakeGlab(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
log=` + shellQuote(dir) + `
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
for last; do :; done
case $last in
user) echo '{"username":"alice"}' ;;
projects/42) echo '{"path_with_namespace":"group/project"}' ;;
input=*) echo '{"data":{"personalAccessTokenCreate":{"token":"session-secret","errors":[]}}}' ;;
'personal_access_tokens?'*)
	name=${last##*search=}
	name=${name%%&*}
	printf '[{"id":99,"name":"%s-other"},{"id":42,"name":"%s"}]\n' "$name" "$name"
	;;
personal_access_tokens/42) ;;
*)
	echo "unexpected $last" >&2
	exit 1
	;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "glab"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func fakeGlabCalls(t *testing.T, dir string) [][]string {
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
func glabConfig() GitLabConfig {
	c := testConfig().GitLab
	c.Issuer, c.IssuerTokenFile = "glab", ""
	return c
}

func TestGlabIssuerCreateAndRevoke(t *testing.T) {
	dir := fakeGlab(t)
	path := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+path)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	for _, key := range glabUnset {
		t.Setenv(key, "outer-secret")
	}
	g, err := newGitLab(glabConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A glab found on PATH later, such as a session wrapper, must not be used.
	decoy := t.TempDir()
	os.WriteFile(filepath.Join(decoy, "glab"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	t.Setenv("PATH", decoy+string(os.PathListSeparator)+path)
	g.Now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	token, err := g.create(context.Background(), 24*time.Hour)
	if err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	if err = g.revokeCreated(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := fakeGlabCalls(t, dir)
	if len(calls) != 5 {
		t.Fatalf("got %d glab calls: %q", len(calls), calls)
	}
	host := []string{"api", "--hostname", "gitlab.com"}
	for i, call := range calls {
		if !slices.Equal(call[:3], host) {
			t.Errorf("call %d not pinned to gitlab.com: %q", i+1, call)
		}
	}
	for i, want := range [][]string{{"--method", "GET", "user"}, {"--method", "GET", "projects/42"}} {
		if !slices.Equal(calls[i][3:], want) {
			t.Errorf("call %d: %q", i+1, calls[i])
		}
	}
	mint := calls[2][3:]
	if len(mint) != 5 || mint[0] != "graphql" || mint[1] != "-f" || mint[2] != "query="+createPAT || mint[3] != "-F" || !strings.HasPrefix(mint[4], "input=") {
		t.Fatalf("mint: %q", mint)
	}
	var input struct {
		Name      string  `json:"name"`
		ExpiresAt string  `json:"expiresAt"`
		Scopes    []Scope `json:"granularScopes"`
	}
	if err = json.Unmarshal([]byte(strings.TrimPrefix(mint[4], "input=")), &input); err != nil {
		t.Fatal(err)
	}
	name := input.Name
	if !strings.HasPrefix(name, "ai-session-") || input.ExpiresAt != "2026-10-10" || len(input.Scopes) != 2 || input.Scopes[1].ResourceIDs[0] != "gid://gitlab/Project/42" {
		t.Errorf("bad PAT input %+v", input)
	}
	if !slices.Equal(calls[3][3:], []string{"--method", "GET", "personal_access_tokens?per_page=100&search=" + name}) {
		t.Errorf("search: %q", calls[3])
	}
	if !slices.Equal(calls[4][3:], []string{"--method", "DELETE", "personal_access_tokens/42"}) {
		t.Errorf("revocation: %q", calls[4])
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
	info, err := os.Stat(filepath.Join(state, "ai-session-auth", "glab.lock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("lock: %v %v", info, err)
	}
}
func TestGlabIssuerFailureIsSilent(t *testing.T) {
	dir := fakeGlab(t)
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
	if len(fakeGlabCalls(t, dir)) != 2 {
		t.Fatal("glab not called")
	}
}
func TestGlabIssuerRejectsScalarVariables(t *testing.T) {
	dir := fakeGlab(t)
	issuer := glabIssuer{executable: filepath.Join(dir, "glab"), lockPath: filepath.Join(t.TempDir(), "glab")}
	if issuer.graphql(context.Background(), createPAT, map[string]any{"name": "x"}, new(any)) == nil {
		t.Fatal("sent a JSON string through -F")
	}
	if len(fakeGlabCalls(t, dir)) != 0 {
		t.Fatal("called glab")
	}
}
func TestGlabIssuerConfig(t *testing.T) {
	c := testConfig()
	c.GitLab = glabConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(c)
	os.WriteFile(path, raw, 0600)
	loaded, err := loadConfig(path)
	if err != nil || loaded.GitLab.IssuerTokenFile != "" {
		t.Fatalf("%q %v", loaded.GitLab.IssuerTokenFile, err)
	}
	// Refused before reading stdin or touching any file.
	code, err := execute(context.Background(), []string{"--config", path, "save-gitlab-issuer", "--stdin"}, nil)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "glab") {
		t.Fatalf("save-gitlab-issuer: %d %v", code, err)
	}
	for _, mutate := range []func(*GitLabConfig){
		func(g *GitLabConfig) { g.IssuerTokenFile = "issuer.txt" },
		func(g *GitLabConfig) { g.Issuer = "file" },
		func(g *GitLabConfig) { g.Issuer = "" },
	} {
		c = testConfig()
		c.GitLab = glabConfig()
		mutate(&c.GitLab)
		if c.validate() == nil {
			t.Fatalf("accepted issuer %q with issuer_token_file %q", c.GitLab.Issuer, c.GitLab.IssuerTokenFile)
		}
	}
}

func TestGitLabNamespacedConfig(t *testing.T) {
	c := testConfig()
	c.GitLab = namespacedConfig()
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*GitLabConfig){
		"mixed with repositories":     func(g *GitLabConfig) { g.Repositories = []string{"group/project"} },
		"mixed with project_ids":      func(g *GitLabConfig) { g.ProjectIDs = []int64{42} },
		"mixed with granular_scopes":  func(g *GitLabConfig) { g.Scopes = testConfig().GitLab.Scopes },
		"no namespace":                func(g *GitLabConfig) { g.Groups, g.PersonalProjects, g.Projects = nil, false, nil },
		"no permissions":              func(g *GitLabConfig) { g.Permissions = nil },
		"PAT management":              func(g *GitLabConfig) { g.Permissions = append(g.Permissions, "revoke_personal_access_token") },
		"own namespace as a group":    func(g *GitLabConfig) { g.Groups = append(g.Groups, "alice") },
		"group with a trailing slash": func(g *GitLabConfig) { g.Groups = []string{"group/"} },
		"group with dot segments":     func(g *GitLabConfig) { g.Groups = []string{"group/../other"} },
		"duplicate group":             func(g *GitLabConfig) { g.Groups = []string{"group", "group"} },
		"project without namespace":   func(g *GitLabConfig) { g.Projects = []string{"project"} },
	} {
		c := testConfig()
		c.GitLab = namespacedConfig()
		mutate(&c.GitLab)
		if c.validate() == nil {
			t.Errorf("accepted %s", name)
		}
	}
	// The earlier format can never ask for every membership or the instance.
	for _, access := range []string{"ALL_MEMBERSHIPS", "INSTANCE", "PERSONAL_PROJECTS"} {
		c := testConfig()
		c.GitLab.Scopes = append(c.GitLab.Scopes, Scope{Access: access, Permissions: []string{"read_project"}})
		if c.validate() == nil {
			t.Errorf("accepted %s", access)
		}
	}
	// The shipped example must load as it is.
	if _, err := loadConfig("config.example.json"); err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
}
func TestGitLabNamespacedScopes(t *testing.T) {
	var scopes []Scope
	var lookups []string
	api := mockAPI(t, func(r *http.Request) (int, any) {
		switch {
		case r.URL.Path == "/api/v4/user":
			return 200, map[string]string{"username": "alice"}
		case r.URL.Path == "/api/graphql":
			var payload struct {
				Variables struct {
					Input struct {
						Scopes []Scope `json:"granularScopes"`
					} `json:"input"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			scopes = payload.Variables.Input.Scopes
			return 200, map[string]any{"data": map[string]any{"personalAccessTokenCreate": map[string]any{"token": "session-secret", "errors": []string{}}}}
		}
		// Subgroup and project paths must travel as one escaped segment.
		lookups = append(lookups, r.URL.EscapedPath())
		switch r.URL.EscapedPath() {
		case "/api/v4/groups/group":
			return 200, map[string]any{"id": 7, "full_path": "group"}
		case "/api/v4/groups/other%2Fsub":
			return 200, map[string]any{"id": 8, "full_path": "other/sub"}
		case "/api/v4/projects/elsewhere%2Fproject":
			return 200, map[string]any{"id": 42, "path_with_namespace": "elsewhere/project"}
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.EscapedPath())
		return 404, nil
	})
	g := &GitLab{Config: namespacedConfig(), Issuer: bearerIssuer{api: api, token: "issuer"}, Now: time.Now}
	if token, err := g.create(context.Background(), time.Hour); err != nil || token != "session-secret" {
		t.Fatalf("%s %v", token, err)
	}
	if len(lookups) != 3 {
		t.Fatalf("lookups %q", lookups)
	}
	permissions := []string{"read_project", "push_code"}
	want := []Scope{
		{Access: "USER", Permissions: []string{"read_user"}},
		{Access: "SELECTED_MEMBERSHIPS", ResourceIDs: []string{"gid://gitlab/Group/7", "gid://gitlab/Group/8", "gid://gitlab/Project/42"}, Permissions: permissions},
		{Access: "PERSONAL_PROJECTS", Permissions: permissions},
	}
	if got, _ := json.Marshal(scopes); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("scopes %s", got)
	}
	// Without personal projects or explicit lists, only what is configured.
	c := namespacedConfig()
	c.Groups, c.Projects = nil, nil
	g = &GitLab{Config: c, Issuer: bearerIssuer{api: api, token: "issuer"}, Now: time.Now}
	if _, err := g.create(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(scopes); string(got) != string(must(json.Marshal([]Scope{want[0], want[2]}))) {
		t.Fatalf("personal-only scopes %s", got)
	}
}
func TestGitLabNamespaceMismatchStopsBeforeMint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer map[string]any
	}{
		{"renamed group", map[string]any{"id": 7, "full_path": "renamed"}},
		{"case differs", map[string]any{"id": 7, "full_path": "Group"}},
		{"no id", map[string]any{"full_path": "group"}},
		{"project fields for a group", map[string]any{"id": 7, "path_with_namespace": "group"}},
	} {
		minted := false
		api := mockAPI(t, func(r *http.Request) (int, any) {
			switch r.URL.Path {
			case "/api/v4/user":
				return 200, map[string]string{"username": "alice"}
			case "/api/graphql":
				minted = true
			}
			return 200, tc.answer
		})
		c := namespacedConfig()
		c.Groups, c.Projects = []string{"group"}, nil
		g := &GitLab{Config: c, Issuer: bearerIssuer{api: api, token: "issuer"}, Now: time.Now}
		if _, err := g.create(context.Background(), time.Hour); err == nil || minted {
			t.Errorf("%s: minted (err %v)", tc.name, err)
		}
	}
}
func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
