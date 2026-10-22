package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/timing"
)

type GitHubState struct {
	ClientID         string  `json:"client_id"`
	AccessToken      string  `json:"access_token"`
	RefreshToken     string  `json:"refresh_token"`
	ExpiresAt        float64 `json:"expires_at"`
	RefreshExpiresAt float64 `json:"refresh_expires_at"`
}
type OAuthResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	Error            string `json:"error"`
	Interval         int64  `json:"interval"`
}
type Provider struct {
	Config config.GitHubConfig
	API    *api.API
	Now    func() time.Time
}

// New returns the GitHub provider for config: it reads the state file a
// github-login wrote, and refreshes the App token when it is about to expire.
func New(config config.GitHubConfig, a *api.API) *Provider {
	return &Provider{Config: config, API: a, Now: time.Now}
}

func (g *Provider) save(response OAuthResponse) (GitHubState, error) {
	var state GitHubState
	if response.Error != "" || response.RefreshToken == "" || response.ExpiresIn <= 0 || response.RefreshExpiresIn <= 0 || !strings.HasPrefix(response.AccessToken, "ghu_") {
		return state, errors.New("GitHub did not return expiring App user tokens; check App settings")
	}
	now := float64(g.Now().Unix())
	state = GitHubState{g.Config.ClientID, response.AccessToken, response.RefreshToken, now + float64(response.ExpiresIn), now + float64(response.RefreshExpiresIn)}
	if err := secretfile.WriteJSON(g.Config.StateFile, state); err != nil {
		return state, errors.New("cannot persist rotated GitHub credentials; reauthorization may be necessary")
	}
	return state, nil
}
func (g *Provider) Verify(ctx context.Context, token string) error {
	var user struct {
		Login string `json:"login"`
	}
	if err := g.API.Request(ctx, http.MethodGet, g.API.GitHubAPI+"/user", token, nil, false, &user); err != nil {
		return err
	}
	if !strings.EqualFold(user.Login, g.Config.ExpectedUser) || user.Login == "" {
		return errors.New("GitHub authenticated user does not match expected_user")
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)

// ID names the host; only github.com is configurable.
func (g *Provider) ID() string   { return "github:github.com" }
func (g *Provider) Kind() string { return "github" }

// Acquire returns the App user token once GitHub confirms it is
// expected_user's.
// Nothing is revocable, so nothing goes in the ledger.
func (g *Provider) Acquire(ctx context.Context, _ *ledger.Ledger) (provider.Credential, error) {
	state, err := g.current(ctx)
	if err != nil {
		return provider.Credential{}, err
	}
	if err = g.Verify(ctx, state.AccessToken); err != nil {
		return provider.Credential{}, err
	}
	return credential(state), nil
}

// Refresh reads the state file again, and rotates the token there once it is
// five minutes from expiring: another session may have rotated it already.
func (g *Provider) Refresh(ctx context.Context, _ provider.Credential) (provider.Credential, error) {
	state, err := g.current(ctx)
	if err != nil {
		return provider.Credential{}, err
	}
	return credential(state), nil
}

// Revoke does nothing: an App user token expires on its own, and revoking it
// would sign out every other session sharing the login.
func (g *Provider) Revoke(ctx context.Context, _ ledger.Entry) error { return nil }

func credential(state GitHubState) provider.Credential {
	return provider.Credential{Secret: state.AccessToken, Expires: time.Unix(int64(state.ExpiresAt), 0)}
}

// current reads the state file under its lock, and rotates the token there
// when it is about to expire.
func (g *Provider) current(ctx context.Context) (GitHubState, error) {
	var current GitHubState
	err := secretfile.WithFileLock(ctx, g.Config.StateFile, func() error {
		raw, err := secretfile.Read(g.Config.StateFile)
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("run github-login before starting a session")
		}
		if err != nil {
			return err
		}
		var state GitHubState
		if err = json.Unmarshal(raw, &state); err != nil {
			return errors.New("invalid GitHub credential state")
		}
		if state.ClientID != g.Config.ClientID {
			return errors.New("GitHub state belongs to a different App")
		}
		if state.AccessToken == "" || state.RefreshToken == "" {
			return errors.New("incomplete GitHub credential state")
		}
		now := float64(g.Now().Unix())
		if state.ExpiresAt <= now+300 {
			if state.RefreshExpiresAt <= now {
				return errors.New("GitHub refresh token expired; run github-login again")
			}
			var response OAuthResponse
			payload := url.Values{"client_id": {g.Config.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {state.RefreshToken}}
			if err = g.API.Request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/oauth/access_token", "", payload, true, &response); err != nil {
				return err
			}
			if response.Error != "" {
				return errors.New("GitHub refresh failed; reauthorize with github-login")
			}
			state, err = g.save(response)
			if err != nil {
				return err
			}
		}
		current = state
		return nil
	})
	return current, err
}
func (g *Provider) Login(ctx context.Context, output io.Writer) error {
	return secretfile.WithFileLock(ctx, g.Config.StateFile, func() error {
		var device struct {
			DeviceCode      string `json:"device_code"`
			UserCode        string `json:"user_code"`
			VerificationURI string `json:"verification_uri"`
			ExpiresIn       int64  `json:"expires_in"`
			Interval        int64  `json:"interval"`
		}
		if err := g.API.Request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/device/code", "", url.Values{"client_id": {g.Config.ClientID}}, true, &device); err != nil {
			return err
		}
		if device.DeviceCode == "" || device.UserCode == "" || device.Interval <= 0 || device.ExpiresIn <= 0 {
			return errors.New("invalid GitHub device response; enable device flow")
		}
		fmt.Fprintf(output, "Open %s and enter %s\n", device.VerificationURI, device.UserCode)
		expires := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
		interval := time.Duration(device.Interval) * time.Second
		for time.Now().Before(expires) {
			if err := timing.SleepContext(ctx, interval); err != nil {
				return err
			}
			var response OAuthResponse
			values := url.Values{"client_id": {g.Config.ClientID}, "device_code": {device.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
			if err := g.API.Request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/oauth/access_token", "", values, true, &response); err != nil {
				return err
			}
			switch response.Error {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				if response.Interval > 0 {
					interval = time.Duration(response.Interval) * time.Second
				}
				continue
			case "":
				// Verify the user before replacing a previously authorized account.
				if err := g.Verify(ctx, response.AccessToken); err != nil {
					return err
				}
				if _, err := g.save(response); err != nil {
					return err
				}
				fmt.Fprintln(output, "GitHub user authorization saved; no token printed.")
				return nil
			default:
				return errors.New("GitHub device authorization denied or expired")
			}
		}
		return errors.New("GitHub device authorization expired")
	})
}
