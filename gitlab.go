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

// issuer performs the GitLab requests made with the issuer credential.
// Endpoints are relative to /api/v4, as in "user" or "projects/42".
type issuer interface {
	rest(ctx context.Context, method, endpoint string, out any) error
	graphql(ctx context.Context, query string, variables map[string]any, out any) error
}

// bearerIssuer sends the requests itself, with the PAT from issuer_token_file.
type bearerIssuer struct {
	api   *API
	token string
}

// The issuer's REST calls are all GET or DELETE, so they carry no body.
func (b bearerIssuer) rest(ctx context.Context, method, endpoint string, out any) error {
	return b.api.request(ctx, method, b.api.GitLabURL+"/api/v4/"+endpoint, b.token, nil, false, out)
}
func (b bearerIssuer) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": variables}
	return b.api.request(ctx, http.MethodPost, b.api.GitLabURL+"/api/graphql", b.token, payload, false, out)
}

type GitLab struct {
	Config  GitLabConfig
	Issuer  issuer
	Created []string
	Now     func() time.Time
}

func newGitLab(config GitLabConfig, api *API) (*GitLab, error) {
	raw, err := privateRead(config.IssuerTokenFile)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("empty GitLab issuer token")
	}
	return &GitLab{Config: config, Issuer: bearerIssuer{api: api, token: token}, Now: time.Now}, nil
}
func (g *GitLab) create(ctx context.Context, duration time.Duration) (string, error) {
	if err := validateScopes(g.Config); err != nil {
		return "", err
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := g.Issuer.rest(ctx, http.MethodGet, "user", &user); err != nil {
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
		if err := g.Issuer.rest(ctx, http.MethodGet, fmt.Sprintf("projects/%d", id), &project); err != nil {
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
	variables := map[string]any{"input": map[string]any{
		"name": name, "expiresAt": expiryForDuration(g.Now(), duration), "granularScopes": g.Config.Scopes,
	}}
	var result struct {
		Errors []any `json:"errors"`
		Data   struct {
			Create struct {
				Token  string   `json:"token"`
				Errors []string `json:"errors"`
			} `json:"personalAccessTokenCreate"`
		} `json:"data"`
	}
	if err := g.Issuer.graphql(ctx, createPAT, variables, &result); err != nil {
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
		if err := g.Issuer.rest(ctx, http.MethodGet, "personal_access_tokens?"+query, &matches); err != nil {
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
		if err := g.Issuer.rest(ctx, http.MethodDelete, fmt.Sprintf("personal_access_tokens/%d", id), nil); err != nil {
			return err
		}
		g.Created = g.Created[1:]
	}
	return nil
}
