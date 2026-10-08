package main

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
type GitHub struct {
	Config GitHubConfig
	API    *API
	Now    func() time.Time
}

func (g *GitHub) save(response OAuthResponse) (GitHubState, error) {
	var state GitHubState
	if response.Error != "" || response.RefreshToken == "" || response.ExpiresIn <= 0 || response.RefreshExpiresIn <= 0 || !strings.HasPrefix(response.AccessToken, "ghu_") {
		return state, errors.New("GitHub did not return expiring App user tokens; check App settings")
	}
	now := float64(g.Now().Unix())
	state = GitHubState{g.Config.ClientID, response.AccessToken, response.RefreshToken, now + float64(response.ExpiresIn), now + float64(response.RefreshExpiresIn)}
	if err := writeJSON(g.Config.StateFile, state); err != nil {
		return state, errors.New("cannot persist rotated GitHub credentials; reauthorization may be necessary")
	}
	return state, nil
}
func (g *GitHub) verify(ctx context.Context, token string) error {
	var user struct {
		Login string `json:"login"`
	}
	if err := g.API.request(ctx, http.MethodGet, g.API.GitHubAPI+"/user", token, nil, false, &user); err != nil {
		return err
	}
	if !strings.EqualFold(user.Login, g.Config.ExpectedUser) || user.Login == "" {
		return errors.New("GitHub authenticated user does not match expected_user")
	}
	return nil
}
func (g *GitHub) token(ctx context.Context) (string, error) {
	var token string
	err := withFileLock(ctx, g.Config.StateFile, func() error {
		raw, err := privateRead(g.Config.StateFile)
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
			if err = g.API.request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/oauth/access_token", "", payload, true, &response); err != nil {
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
		token = state.AccessToken
		return nil
	})
	return token, err
}
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (g *GitHub) login(ctx context.Context, output io.Writer) error {
	return withFileLock(ctx, g.Config.StateFile, func() error {
		var device struct {
			DeviceCode      string `json:"device_code"`
			UserCode        string `json:"user_code"`
			VerificationURI string `json:"verification_uri"`
			ExpiresIn       int64  `json:"expires_in"`
			Interval        int64  `json:"interval"`
		}
		if err := g.API.request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/device/code", "", url.Values{"client_id": {g.Config.ClientID}}, true, &device); err != nil {
			return err
		}
		if device.DeviceCode == "" || device.UserCode == "" || device.Interval <= 0 || device.ExpiresIn <= 0 {
			return errors.New("invalid GitHub device response; enable device flow")
		}
		fmt.Fprintf(output, "Open %s and enter %s\n", device.VerificationURI, device.UserCode)
		expires := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
		interval := time.Duration(device.Interval) * time.Second
		for time.Now().Before(expires) {
			if err := sleepContext(ctx, interval); err != nil {
				return err
			}
			var response OAuthResponse
			values := url.Values{"client_id": {g.Config.ClientID}, "device_code": {device.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
			if err := g.API.request(ctx, http.MethodPost, g.API.GitHubLogin+"/login/oauth/access_token", "", values, true, &response); err != nil {
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
				if err := g.verify(ctx, response.AccessToken); err != nil {
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
