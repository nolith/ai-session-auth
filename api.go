package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type API struct {
	Client      *http.Client
	GitHubAPI   string
	GitHubLogin string
	GitLabURL   string
}

func newAPI() *API {
	return &API{
		Client:    &http.Client{Timeout: 30 * time.Second},
		GitHubAPI: "https://api.github.com", GitHubLogin: "https://github.com", GitLabURL: "https://gitlab.com",
	}
}
func (a *API) request(ctx context.Context, method, address, token string, payload any, form bool, result any) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return errors.New("API requests require HTTPS without URL credentials")
	}
	var body io.Reader
	if payload != nil {
		if form {
			values, ok := payload.(url.Values)
			if !ok {
				return errors.New("invalid form payload")
			}
			body = strings.NewReader(values.Encode())
		} else {
			raw, err := json.Marshal(payload)
			if err != nil {
				return errors.New("invalid API payload")
			}
			body = bytes.NewReader(raw)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return errors.New("cannot construct API request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "ai-session-auth/0.2")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		if form {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	client := *a.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return errors.New("API request failed: network, cancellation or timeout")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("API request failed (HTTP %d); inspect permissions/configuration", response.StatusCode)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 2_000_000))
		return err
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2_000_000)).Decode(result); err != nil {
		return errors.New("API returned invalid JSON")
	}
	return nil
}
