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
	"strings"
	"sync"
	"sync/atomic"
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
