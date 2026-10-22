package gitlab

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nolith/geas/internal/api"
	"github.com/nolith/geas/internal/config"
	"github.com/nolith/geas/internal/ledger"
	"github.com/nolith/geas/internal/procenv"
	"github.com/nolith/geas/internal/provider"
	"github.com/nolith/geas/internal/secretfile"
	"github.com/nolith/geas/internal/timing"
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
	client *api.API
	token  string
}

// The issuer's REST calls are all GET or DELETE, so they carry no body.
func (b bearerIssuer) rest(ctx context.Context, method, endpoint string, out any) error {
	return b.client.Request(ctx, method, b.client.GitLabURL+"/api/v4/"+endpoint, b.token, nil, false, out)
}
func (b bearerIssuer) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": variables}
	return b.client.Request(ctx, http.MethodPost, b.client.GitLabURL+"/api/graphql", b.token, payload, false, out)
}

// glabIssuer delegates to the user's own glab login, which renews its OAuth
// token itself, so revocation still works hours after the session started.
type glabIssuer struct {
	executable string // absolute, resolved before the session PATH exists
	lockPath   string // withFileLock adds ".lock": one OAuth refresh at a time
}

// glabUnset keeps the issuer on the user's own gitlab.com login: no token or
// temporary config from an enclosing session (without GEAS_SOCKET, that
// session's glab wrapper fails instead of answering), no host override, and no
// HTTP debugging, which prints credentials.
var glabUnset = []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "GLAB_ENABLE_CI_AUTOLOGIN", "GLAB_CONFIG_DIR", "GEAS_SOCKET", "GITLAB_HOST", "GITLAB_URI", "GLAB_DEBUG", "GLAB_DEBUG_HTTP"}

func newGlabIssuer() (glabIssuer, error) {
	executable, err := exec.LookPath("glab")
	if err != nil {
		return glabIssuer{}, errors.New("install glab and add it to PATH")
	}
	if executable, err = filepath.Abs(executable); err != nil {
		return glabIssuer{}, err
	}
	state, err := secretfile.StateDirectory()
	if err != nil {
		return glabIssuer{}, err
	}
	return glabIssuer{executable: executable, lockPath: filepath.Join(state, "glab")}, nil
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
	return secretfile.WithFileLock(ctx, g.lockPath, func() error {
		// A locked keychain must fail the request, not hang the session.
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, g.executable, args...)
		// Outside any repository, so no remote can fill glab's placeholders.
		command.Dir = "/"
		// Its own process group: the SIGHUP of a closing terminal, or another
		// Ctrl-C, must not cut a revocation short.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		environment := procenv.Map(os.Environ())
		for _, key := range glabUnset {
			delete(environment, key)
		}
		command.Env = procenv.List(environment)
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

type Provider struct {
	Config config.GitLabConfig
	Issuer issuer
	// Duration is the session's: a session PAT lives until the UTC midnight
	// after it.
	Duration time.Duration
	Now      func() time.Time
	// Steps times the start-up calls for GEAS_DEBUG=timing; nil when off.
	Steps *timing.Timer

	mu sync.Mutex
	// minted names the PATs this process minted: an ID it resolved for one
	// of them needs no check before the DELETE.
	minted map[string]bool
}

// New returns the provider for one GitLab host, whose session PATs last for
// duration. It reads the issuer's credential, or finds glab, but calls nothing.
func New(host config.GitLabConfig, duration time.Duration, a *api.API, steps *timing.Timer) (*Provider, error) {
	if host.Issuer == "glab" {
		glab, err := newGlabIssuer()
		if err != nil {
			return nil, err
		}
		return &Provider{Config: host, Issuer: glab, Duration: duration, Now: time.Now, Steps: steps}, nil
	}
	raw, err := secretfile.Read(host.IssuerTokenFile)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("empty GitLab issuer token")
	}
	return &Provider{Config: host, Issuer: bearerIssuer{client: a, token: token}, Duration: duration, Now: time.Now, Steps: steps}, nil
}

var _ provider.Provider = (*Provider)(nil)

// ID names the instance; only gitlab.com is configurable so far.
func (g *Provider) ID() string   { return "gitlab:gitlab.com" }
func (g *Provider) Kind() string { return "gitlab" }

// Acquire mints the session PAT and resolves its ID, so that the exit needs a
// single DELETE. The PAT's name is in the ledger before the mint, and its ID
// once it is resolved.
func (g *Provider) Acquire(ctx context.Context, l *ledger.Ledger) (provider.Credential, error) {
	token, err := g.create(ctx, l, g.Duration)
	if err != nil {
		return provider.Credential{}, err
	}
	expires, err := time.Parse("2006-01-02", config.ExpiryForDuration(g.Now(), g.Duration))
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.Credential{Secret: token, Expires: expires}, nil
}

// Refresh returns c: a session PAT outlives the session.
func (g *Provider) Refresh(ctx context.Context, c provider.Credential) (provider.Credential, error) {
	return c, nil
}

func (g *Provider) create(ctx context.Context, l *ledger.Ledger, duration time.Duration) (string, error) {
	if err := config.ValidateGitLabAllowlist(g.Config); err != nil {
		return "", err
	}
	resolved, err := g.lookup(ctx)
	g.Steps.Step("GitLab issuer lookup")
	if err != nil {
		return "", err
	}
	if resolved.user == "" || resolved.user != g.Config.ExpectedUser {
		return "", errors.New("GitLab issuer user does not match expected_user")
	}
	scopes, err := g.scopes(resolved)
	if err != nil {
		return "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := "geas-" + hex.EncodeToString(random)
	// The mutation returns no ID: with the name on disk first, a session that
	// dies before it resolves the ID still leaves the reaper a PAT to find.
	entry := ledger.Entry{Provider: g.ID(), Name: name, Created: g.Now().UTC()}
	if err := l.Record(entry); err != nil {
		return "", err
	}
	variables := map[string]any{"input": map[string]any{
		"name": name, "expiresAt": config.ExpiryForDuration(g.Now(), duration), "granularScopes": scopes,
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
	g.Steps.Step("mint")
	if len(result.Errors) > 0 || len(result.Data.Create.Errors) > 0 || result.Data.Create.Token == "" {
		// GitLab answered and refused: no PAT exists under this name, so the
		// entry is settled and no revoker chases it. A transport error above
		// leaves the entry open: the PAT may exist, and only a search by
		// name can tell.
		entry.Revoked = true
		if err := l.Record(entry); err != nil {
			return "", err
		}
		return "", errors.New("GitLab PAT creation failed: verify granular scopes and issuer permissions")
	}
	g.mu.Lock()
	if g.minted == nil {
		g.minted = map[string]bool{}
	}
	g.minted[name] = true
	g.mu.Unlock()
	// The mutation returns no ID. Resolving it now leaves a single DELETE for
	// the exit, which may have little time, and gives the reaper its target.
	id, err := g.find(ctx, name)
	g.Steps.Step("id lookup")
	if errors.Is(err, ledger.ErrNotFound) {
		err = errUnresolved
	}
	if err != nil {
		return "", provider.CreatedError{Err: err}
	}
	entry.ID = id
	if err := l.Record(entry); err != nil {
		return "", provider.CreatedError{Err: err}
	}
	return result.Data.Create.Token, nil
}

// lookup is the issuer lookup's answer: the issuer's user and every namespace
// the allowlist names, in configuration order.
type lookup struct {
	user     string
	groups   []namespace
	projects []namespace
}

// namespace is one group or project resolved by path or ID. id is the GraphQL
// global ID, which granular scopes use as it is.
type namespace struct {
	id       string
	fullPath string
}

// lookupDocument builds the one aliased query that replaces the REST user call
// and the per-namespace lookups: currentUser for the identity check, one
// group(fullPath:) or project(fullPath:) alias per configured path, and
// projects(ids:) for the earlier format. GraphQL never embeds a group's
// projects, so the 6 s REST answer disappears with it.
//
// The paths are written into the document as escaped string literals: an alias
// needs one literal argument each, and the glab issuer carries only objects and
// arrays in its -F variables, never the scalar a $path variable would need.
func lookupDocument(c config.GitLabConfig) string {
	var b strings.Builder
	b.WriteString("query SessionLookup {\n  currentUser { username }\n")
	if !c.Namespaced() {
		ids := make([]string, 0, len(c.ProjectIDs))
		for _, id := range c.ProjectIDs {
			ids = append(ids, graphqlString(fmt.Sprintf("gid://gitlab/Project/%d", id)))
		}
		fmt.Fprintf(&b, "  l0: projects(ids: [%s]) { nodes { id fullPath } }\n", strings.Join(ids, ", "))
	}
	for i, path := range c.Groups {
		fmt.Fprintf(&b, "  g%d: group(fullPath: %s) { id fullPath }\n", i, graphqlString(path))
	}
	for i, path := range c.Projects {
		fmt.Fprintf(&b, "  p%d: project(fullPath: %s) { id fullPath }\n", i, graphqlString(path))
	}
	b.WriteString("}\n")
	return b.String()
}

// graphqlString writes value as a GraphQL string literal. JSON's escaping is a
// subset of GraphQL's, and the configuration validates the paths.
func graphqlString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// parseGlobalID reads the numeric ID out of a GraphQL global ID such as
// gid://gitlab/Group/9970, and refuses another model's.
func parseGlobalID(value, model string) (int64, error) {
	rest, ok := strings.CutPrefix(value, "gid://gitlab/"+model+"/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if !ok || err != nil || id <= 0 {
		return 0, fmt.Errorf("unexpected %s global ID", strings.ToLower(model))
	}
	return id, nil
}

// lookup resolves the user and every configured namespace in one request, and
// refuses a path that does not resolve, as the REST lookups did. The earlier
// format resolves its project IDs instead.
func (g *Provider) lookup(ctx context.Context) (lookup, error) {
	var answer struct {
		Errors []any                      `json:"errors"`
		Data   map[string]json.RawMessage `json:"data"`
	}
	if err := g.Issuer.graphql(ctx, lookupDocument(g.Config), map[string]any{}, &answer); err != nil {
		return lookup{}, err
	}
	if len(answer.Errors) > 0 || answer.Data == nil {
		return lookup{}, errors.New("GitLab lookup query failed; check the issuer and the allowlist")
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := decodeAlias(answer.Data, "currentUser", "issuer user", &user); err != nil {
		return lookup{}, err
	}
	resolved := lookup{user: user.Username}
	if !g.Config.Namespaced() {
		projects, err := projectNodes(answer.Data)
		if err != nil {
			return lookup{}, err
		}
		resolved.projects = projects
		return resolved, nil
	}
	for i, path := range g.Config.Groups {
		node, err := pathNode(answer.Data, fmt.Sprintf("g%d", i), "Group", path)
		if err != nil {
			return lookup{}, err
		}
		resolved.groups = append(resolved.groups, node)
	}
	for i, path := range g.Config.Projects {
		node, err := pathNode(answer.Data, fmt.Sprintf("p%d", i), "Project", path)
		if err != nil {
			return lookup{}, err
		}
		resolved.projects = append(resolved.projects, node)
	}
	return resolved, nil
}

// decodeAlias reads one alias of the lookup answer, refusing null: GraphQL
// answers null for a path that does not resolve.
func decodeAlias(data map[string]json.RawMessage, alias, what string, out any) error {
	raw, ok := data[alias]
	if !ok || string(raw) == "null" {
		return fmt.Errorf("GitLab %s does not resolve; update the allowlist", what)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("GitLab lookup returned an unexpected answer")
	}
	return nil
}

// pathNode resolves one configured group or project and refuses anything but
// the configured path and a global ID of the expected model.
func pathNode(data map[string]json.RawMessage, alias, model, path string) (namespace, error) {
	var node struct {
		ID       string `json:"id"`
		FullPath string `json:"fullPath"`
	}
	kind := strings.ToLower(model)
	if err := decodeAlias(data, alias, kind+" "+path, &node); err != nil {
		return namespace{}, err
	}
	if node.FullPath != path {
		return namespace{}, fmt.Errorf("GitLab %s %s resolves to another path; update the allowlist", kind, path)
	}
	if _, err := parseGlobalID(node.ID, model); err != nil {
		return namespace{}, fmt.Errorf("GitLab %s %s resolves without an ID; update the allowlist", kind, path)
	}
	return namespace{id: node.ID, fullPath: node.FullPath}, nil
}

// projectNodes resolves the earlier format's projects(ids:), one node per ID.
func projectNodes(data map[string]json.RawMessage) ([]namespace, error) {
	var answer struct {
		Nodes []struct {
			ID       string `json:"id"`
			FullPath string `json:"fullPath"`
		} `json:"nodes"`
	}
	if err := decodeAlias(data, "l0", "project IDs", &answer); err != nil {
		return nil, err
	}
	projects := make([]namespace, 0, len(answer.Nodes))
	for _, node := range answer.Nodes {
		if _, err := parseGlobalID(node.ID, "Project"); err != nil || node.FullPath == "" {
			return nil, errors.New("GitLab project IDs resolve to an unexpected answer; update the allowlist")
		}
		projects = append(projects, namespace{id: node.ID, fullPath: node.FullPath})
	}
	return projects, nil
}

// scopes returns the session PAT's scopes from the resolved namespaces. Groups
// and projects are refused unless GitLab reported the same path, so a renamed
// or mistyped namespace stops the session before any PAT exists. The earlier
// format lists project IDs and scopes itself; only the paths are checked.
func (g *Provider) scopes(resolved lookup) ([]config.Scope, error) {
	if !g.Config.Namespaced() {
		paths := map[string]bool{}
		for _, project := range resolved.projects {
			paths[project.fullPath] = true
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
	for _, namespaces := range [][]namespace{resolved.groups, resolved.projects} {
		for _, node := range namespaces {
			allowed[node.id] = true
			ids = append(ids, node.id)
		}
	}
	scopes := namespacedScopes(g.Config, ids)
	if err := config.CheckScopes(scopes, allowed, g.Config.PersonalProjects); err != nil {
		return nil, err
	}
	return scopes, nil
}

// namespacedScopes grants read_user, to verify the user, and the permissions
// on each resolved group and project and on personal projects when enabled.
// A group covers its subgroups and their projects.
func namespacedScopes(c config.GitLabConfig, resourceIDs []string) []config.Scope {
	scopes := []config.Scope{{Access: "USER", Permissions: []string{"read_user"}}}
	if len(resourceIDs) > 0 {
		scopes = append(scopes, config.Scope{Access: "SELECTED_MEMBERSHIPS", ResourceIDs: resourceIDs, Permissions: c.Permissions})
	}
	if c.PersonalProjects {
		scopes = append(scopes, config.Scope{Access: "PERSONAL_PROJECTS", Permissions: c.Permissions})
	}
	return scopes
}

// findDocument builds the id lookup for one PAT name. The name is written into
// the document for the same reason as the lookup paths: the glab issuer cannot
// carry the scalar a $name variable would need.
func findDocument(name string) string {
	return fmt.Sprintf("query SessionPAT {\n  currentUser { personalAccessTokens(search: %s) { nodes { id name } } }\n}\n", graphqlString(name))
}

var errUnresolved = errors.New("cannot uniquely resolve session PAT for revocation")

// find resolves a PAT's numeric ID by name, for the revocation: the mutation
// returns no ID. A name that matches no token is ledger.ErrNotFound, and one
// that matches more than one is refused.
func (g *Provider) find(ctx context.Context, name string) (int64, error) {
	var answer struct {
		Errors []any `json:"errors"`
		Data   struct {
			CurrentUser struct {
				PersonalAccessTokens struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"personalAccessTokens"`
			} `json:"currentUser"`
		} `json:"data"`
	}
	if err := g.Issuer.graphql(ctx, findDocument(name), map[string]any{}, &answer); err != nil {
		return 0, err
	}
	if len(answer.Errors) > 0 {
		return 0, errors.New("GitLab id lookup query failed")
	}
	var id int64
	count := 0
	for _, match := range answer.Data.CurrentUser.PersonalAccessTokens.Nodes {
		if match.Name != name {
			continue
		}
		numeric, err := parseGlobalID(match.ID, "PersonalAccessToken")
		if err != nil {
			return 0, errUnresolved
		}
		id = numeric
		count++
	}
	if count == 0 {
		return 0, ledger.ErrNotFound
	}
	if count != 1 || id <= 0 {
		return 0, errUnresolved
	}
	return id, nil
}

// Revoke revokes one session PAT, finding it by name when the ledger has no
// ID for it. A PAT this process minted is deleted at once, which leaves the
// exit a single DELETE; any other, such as the reaper's, is checked first.
func (g *Provider) Revoke(ctx context.Context, e ledger.Entry) error {
	id := e.ID
	if id == 0 {
		found, err := g.find(ctx, e.Name)
		if err != nil {
			return err
		}
		id = found
	}
	g.mu.Lock()
	minted := g.minted[e.Name]
	g.mu.Unlock()
	if minted {
		return g.Issuer.rest(ctx, http.MethodDelete, fmt.Sprintf("personal_access_tokens/%d", id), nil)
	}
	revoked, err := g.revokeChecked(ctx, e.Name, id)
	if err == nil && !revoked {
		return ledger.ErrGone
	}
	return err
}

var ErrOtherPAT = errors.New("the PAT ID names another token; not revoking it")

// revokeChecked revokes a session PAT by ID once GitLab confirms that the ID
// still names it. One already revoked or expired is left alone, so a launcher
// that revoked just before dying causes no second DELETE.
func (g *Provider) revokeChecked(ctx context.Context, name string, id int64) (bool, error) {
	var current struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	if err := g.Issuer.rest(ctx, http.MethodGet, fmt.Sprintf("personal_access_tokens/%d", id), &current); err != nil {
		return false, err
	}
	if current.Name != name {
		return false, ErrOtherPAT
	}
	if !current.Active {
		return false, nil
	}
	if err := g.Issuer.rest(ctx, http.MethodDelete, fmt.Sprintf("personal_access_tokens/%d", id), nil); err != nil {
		return false, err
	}
	return true, nil
}
