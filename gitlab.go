package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// glabIssuer delegates to the user's own glab login, which renews its OAuth
// token itself, so revocation still works hours after the session started.
type glabIssuer struct {
	executable string // absolute, resolved before the session PATH exists
	lockPath   string // withFileLock adds ".lock": one OAuth refresh at a time
}

// glabUnset keeps the issuer on the user's own gitlab.com login: no token or
// temporary config from an enclosing session (without AI_AUTH_SOCKET, that
// session's glab wrapper fails instead of answering), no host override, and no
// HTTP debugging, which prints credentials.
var glabUnset = []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "GLAB_ENABLE_CI_AUTOLOGIN", "GLAB_CONFIG_DIR", "AI_AUTH_SOCKET", "GITLAB_HOST", "GITLAB_URI", "GLAB_DEBUG", "GLAB_DEBUG_HTTP"}

func newGlabIssuer() (glabIssuer, error) {
	executable, err := executableLookup("glab")
	if err != nil {
		return glabIssuer{}, errors.New("install glab and add it to PATH")
	}
	if executable, err = filepath.Abs(executable); err != nil {
		return glabIssuer{}, err
	}
	state, err := stateDirectory()
	if err != nil {
		return glabIssuer{}, err
	}
	return glabIssuer{executable: executable, lockPath: filepath.Join(state, "glab")}, nil
}
func stateDirectory() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		return filepath.Join(state, "ai-session-auth"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "ai-session-auth"), nil
}
func (g glabIssuer) rest(ctx context.Context, method, endpoint string, out any) error {
	return g.run(ctx, out, "--method", method, endpoint)
}

// glab sends GraphQL variables from fields, with -F parsing JSON objects and
// arrays; a JSON string would arrive with its quotes. A whole body through
// `--input -` arrives as an empty document (glab 1.120: "Unexpected end of
// document").
func (g glabIssuer) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	args := []string{"graphql", "-f", "query=" + query}
	for _, name := range slices.Sorted(maps.Keys(variables)) {
		raw, err := json.Marshal(variables[name])
		if err != nil {
			return err
		}
		if raw[0] != '{' && raw[0] != '[' {
			return errors.New("glab issuer GraphQL variables must be objects or arrays")
		}
		args = append(args, "-F", name+"="+string(raw))
	}
	return g.run(ctx, out, args...)
}
func (g glabIssuer) run(ctx context.Context, out any, args ...string) error {
	// Pinned: otherwise glab picks the host from the cwd's remote.
	args = append([]string{"api", "--hostname", "gitlab.com"}, args...)
	return withFileLock(ctx, g.lockPath, func() error {
		// A locked keychain must fail the request, not hang the session.
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, g.executable, args...)
		// Outside any repository, so no remote can fill glab's placeholders.
		command.Dir = "/"
		// Its own process group: the SIGHUP of a closing terminal, or another
		// Ctrl-C, must not cut a revocation short.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		environment := environmentMap(os.Environ())
		for _, key := range glabUnset {
			delete(environment, key)
		}
		command.Env = environmentList(environment)
		output, err := command.Output()
		if err != nil {
			// glab's output may quote the response; keep it out of the logs.
			return errors.New("glab issuer request failed")
		}
		if out == nil {
			return nil
		}
		if json.Unmarshal(output, out) != nil {
			return errors.New("glab issuer returned invalid JSON")
		}
		return nil
	})
}

type GitLab struct {
	Config  GitLabConfig
	Issuer  issuer
	Created []string
	Now     func() time.Time
}

func newGitLab(config GitLabConfig, api *API) (*GitLab, error) {
	if config.Issuer == "glab" {
		glab, err := newGlabIssuer()
		if err != nil {
			return nil, err
		}
		return &GitLab{Config: config, Issuer: glab, Now: time.Now}, nil
	}
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
	if err := validateGitLabAllowlist(g.Config); err != nil {
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
	scopes, err := g.scopes(ctx)
	if err != nil {
		return "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := "ai-session-" + hex.EncodeToString(random)
	variables := map[string]any{"input": map[string]any{
		"name": name, "expiresAt": expiryForDuration(g.Now(), duration), "granularScopes": scopes,
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

// scopes returns the session PAT's scopes. Groups and projects are resolved by
// path and refused unless GitLab reports the same path, so a renamed or
// mistyped namespace stops the session before any PAT exists. The earlier
// format lists project IDs and scopes itself; only the paths are checked.
func (g *GitLab) scopes(ctx context.Context) ([]Scope, error) {
	if !g.Config.namespaced() {
		paths := map[string]bool{}
		for _, id := range g.Config.ProjectIDs {
			var project struct {
				Path string `json:"path_with_namespace"`
			}
			if err := g.Issuer.rest(ctx, http.MethodGet, fmt.Sprintf("projects/%d", id), &project); err != nil {
				return nil, err
			}
			paths[project.Path] = true
		}
		if len(paths) != len(g.Config.Repositories) {
			return nil, errors.New("GitLab project IDs do not match repository allowlist")
		}
		for _, repo := range g.Config.Repositories {
			if !paths[repo] {
				return nil, errors.New("GitLab project IDs do not match repository allowlist")
			}
		}
		return g.Config.Scopes, nil
	}
	allowed := map[string]bool{}
	var ids []string
	for _, kind := range []struct {
		model, endpoint string
		paths           []string
	}{{"Group", "groups/", g.Config.Groups}, {"Project", "projects/", g.Config.Projects}} {
		for _, path := range kind.paths {
			var resolved struct {
				ID      int64  `json:"id"`
				Group   string `json:"full_path"`
				Project string `json:"path_with_namespace"`
			}
			if err := g.Issuer.rest(ctx, http.MethodGet, kind.endpoint+url.PathEscape(path), &resolved); err != nil {
				return nil, err
			}
			reported := resolved.Group
			if kind.model == "Project" {
				reported = resolved.Project
			}
			if resolved.ID <= 0 || reported != path {
				return nil, fmt.Errorf("GitLab %s %s resolves to another path; update the allowlist", strings.ToLower(kind.model), path)
			}
			id := fmt.Sprintf("gid://gitlab/%s/%d", kind.model, resolved.ID)
			allowed[id] = true
			ids = append(ids, id)
		}
	}
	scopes := namespacedScopes(g.Config, ids)
	if err := checkScopes(scopes, allowed, g.Config.PersonalProjects); err != nil {
		return nil, err
	}
	return scopes, nil
}

// namespacedScopes grants read_user, to verify the user, and the permissions
// on each resolved group and project and on personal projects when enabled.
// A group covers its subgroups and their projects.
func namespacedScopes(c GitLabConfig, resourceIDs []string) []Scope {
	scopes := []Scope{{Access: "USER", Permissions: []string{"read_user"}}}
	if len(resourceIDs) > 0 {
		scopes = append(scopes, Scope{Access: "SELECTED_MEMBERSHIPS", ResourceIDs: resourceIDs, Permissions: c.Permissions})
	}
	if c.PersonalProjects {
		scopes = append(scopes, Scope{Access: "PERSONAL_PROJECTS", Permissions: c.Permissions})
	}
	return scopes
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
