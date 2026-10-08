package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const createPAT = `mutation AgentToken($input: PersonalAccessTokenCreateInput!) {
 personalAccessTokenCreate(input: $input) { token errors }
}`

type GitLab struct {
	Config  GitLabConfig
	API     *API
	Issuer  string
	Created []string
	Now     func() time.Time
}

func newGitLab(config GitLabConfig, api *API) (*GitLab, error) {
	raw, err := privateRead(config.IssuerTokenFile)
	if err != nil {
		return nil, err
	}
	issuer := strings.TrimSpace(string(raw))
	if issuer == "" {
		return nil, errors.New("empty GitLab issuer token")
	}
	return &GitLab{Config: config, API: api, Issuer: issuer, Now: time.Now}, nil
}
func (g *GitLab) create(ctx context.Context, duration time.Duration) (string, error) {
	if err := validateScopes(g.Config); err != nil {
		return "", err
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := g.API.request(ctx, http.MethodGet, g.API.GitLabURL+"/api/v4/user", g.Issuer, nil, false, &user); err != nil {
		return "", err
	}
	if user.Username == "" || user.Username != g.Config.ExpectedUser {
		return "", errors.New("GitLab issuer user does not match expected_user")
	}
	paths := map[string]bool{}
	for _, id := range g.Config.ProjectIDs {
		var project struct {
			Path string `json:"path_with_namespace"`
		}
		if err := g.API.request(ctx, http.MethodGet, fmt.Sprintf("%s/api/v4/projects/%d", g.API.GitLabURL, id), g.Issuer, nil, false, &project); err != nil {
			return "", err
		}
		paths[project.Path] = true
	}
	if len(paths) != len(g.Config.Repositories) {
		return "", errors.New("GitLab project IDs do not match repository allowlist")
	}
	for _, repo := range g.Config.Repositories {
		if !paths[repo] {
			return "", errors.New("GitLab project IDs do not match repository allowlist")
		}
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := "ai-session-" + hex.EncodeToString(random)
	payload := map[string]any{"query": createPAT, "variables": map[string]any{"input": map[string]any{
		"name": name, "expiresAt": expiryForDuration(g.Now(), duration), "granularScopes": g.Config.Scopes,
	}}}
	var result struct {
		Errors []any `json:"errors"`
		Data   struct {
			Create struct {
				Token  string   `json:"token"`
				Errors []string `json:"errors"`
			} `json:"personalAccessTokenCreate"`
		} `json:"data"`
	}
	if err := g.API.request(ctx, http.MethodPost, g.API.GitLabURL+"/api/graphql", g.Issuer, payload, false, &result); err != nil {
		return "", err
	}
	if len(result.Errors) > 0 || len(result.Data.Create.Errors) > 0 || result.Data.Create.Token == "" {
		return "", errors.New("GitLab PAT creation failed: verify granular scopes and issuer permissions")
	}
	g.Created = append(g.Created, name)
	return result.Data.Create.Token, nil
}
func (g *GitLab) revokeCreated(ctx context.Context) error {
	for len(g.Created) > 0 {
		name := g.Created[0]
		query := url.Values{"search": {name}, "per_page": {"100"}}.Encode()
		var matches []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if err := g.API.request(ctx, http.MethodGet, g.API.GitLabURL+"/api/v4/personal_access_tokens?"+query, g.Issuer, nil, false, &matches); err != nil {
			return err
		}
		var id int64
		count := 0
		for _, match := range matches {
			if match.Name == name {
				id = match.ID
				count++
			}
		}
		if count != 1 || id <= 0 {
			return errors.New("cannot uniquely resolve session PAT for revocation")
		}
		if err := g.API.request(ctx, http.MethodDelete, fmt.Sprintf("%s/api/v4/personal_access_tokens/%d", g.API.GitLabURL, id), g.Issuer, nil, false, nil); err != nil {
			return err
		}
		g.Created = g.Created[1:]
	}
	return nil
}
