package github

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/testutil"
)

// mockAPI is the shared transport fake, wired to this package's API.
func mockAPI(t *testing.T, handler func(*http.Request) (int, any)) *api.API {
	t.Helper()
	a := api.New()
	a.Client.Transport = testutil.Transport(handler)
	return a
}

func TestGitHubConcurrentRotation(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	c := testutil.GitHub()
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
	g := &Provider{Config: c, API: api, Now: func() time.Time { return now }}
	secretfile.WriteJSON(c.StateFile, GitHubState{ClientID: c.ClientID, AccessToken: "ghu_old", RefreshToken: "old-refresh", ExpiresAt: float64(now.Unix()), RefreshExpiresAt: float64(now.Add(time.Hour).Unix())})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := g.Refresh(context.Background(), provider.Credential{})
			if err != nil || c.Secret != "ghu_new" || !c.Expires.Equal(now.Add(8*time.Hour)) {
				t.Errorf("token=%s expires=%v err=%v", c.Secret, c.Expires, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times", calls.Load())
	}
	raw, _ := secretfile.Read(c.StateFile)
	var state GitHubState
	json.Unmarshal(raw, &state)
	if state.RefreshToken != "rotated-refresh" {
		t.Fatal("rotation not persisted")
	}
}

func TestGitHubIdentityMismatch(t *testing.T) {
	api := mockAPI(t, func(r *http.Request) (int, any) { return 200, map[string]string{"login": "someone-else"} })
	g := &Provider{Config: testutil.GitHub(), API: api, Now: time.Now}
	if g.Verify(context.Background(), "ghu_test") == nil {
		t.Fatal("accepted wrong user")
	}
}
