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

// bearerIssuer sends the requests itself, to https://<host>, with the PAT
// from the issuer's token_file.
type bearerIssuer struct {
	client *api.API
	host   string
	token  string
}

// The issuer's REST calls are all GET or DELETE, so they carry no body.
func (b bearerIssuer) rest(ctx context.Context, method, endpoint string, out any) error {
	return b.client.Request(ctx, method, "https://"+b.host+"/api/v4/"+endpoint, b.token, nil, false, out)
}
func (b bearerIssuer) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	payload := map[string]any{"query": query, "variables": variables}
	return b.client.Request(ctx, http.MethodPost, "https://"+b.host+"/api/graphql", b.token, payload, false, out)
}

// glabIssuer delegates to the user's own glab login for its host, which
// renews its OAuth token itself, so revocation still works hours after the
// session started.
type glabIssuer struct {
	host       string
	executable string // absolute, resolved once, at the start
	// lockPath is the same for every host and every session, with ".lock"
	// added: glab keeps all hosts in one config.yml, so one call at a time,
	// whichever host it asks, can rotate a refresh token.
	lockPath string
}

// glabUnset keeps the issuer on the user's own login for its host: no token or
// session configuration from an enclosing session, no host override, and no
// HTTP debugging, which prints credentials.
var glabUnset = []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "GLAB_ENABLE_CI_AUTOLOGIN", "GLAB_CONFIG_DIR", "GITLAB_HOST", "GITLAB_URI", "GLAB_DEBUG", "GLAB_DEBUG_HTTP"}

func newGlabIssuer(host string) (glabIssuer, error) {
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
	return glabIssuer{host: host, executable: executable, lockPath: filepath.Join(state, "glab")}, nil
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
	args = append([]string{"api", "--hostname", g.host}, args...)
	return secretfile.HoldFileLock(ctx, g.lockPath, func(lock *os.File) error {
		// A locked keychain must fail the request, not hang the session.
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, g.executable, args...)
		// Outside any repository, so no remote can fill glab's placeholders.
		command.Dir = "/"
		// Its own process group: the SIGHUP of a closing terminal, or another
		// Ctrl-C, must not cut a revocation short.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// glab holds the lock for as long as it runs: if the launcher dies
		// meanwhile, the reaper's glab still waits for this one to finish
		// rotating the login.
		command.ExtraFiles = []*os.File{lock}
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
	// Steps times the issuer calls for GEAS_DEBUG=timing; nil when off.
	Steps *timing.Timer

	mu sync.Mutex
	// minted names the PATs this process minted: an ID it resolved for one
	// of them needs no check before the DELETE.
	minted map[string]bool
	// unresolved are the ledger entries of minted PATs whose ID Settle has
	// yet to resolve.
	unresolved []ledger.Entry
}

// New returns the provider for one GitLab host, whose session PATs last for
// duration. It reads the issuer's credential, or finds glab, but calls nothing.
func New(host config.GitLabConfig, duration time.Duration, a *api.API, steps *timing.Timer) (*Provider, error) {
	if host.Issuer.Type == "glab" {
		glab, err := newGlabIssuer(host.Host)
		if err != nil {
			return nil, err
		}
		return &Provider{Config: host, Issuer: glab, Duration: duration, Now: time.Now, Steps: steps}, nil
	}
	raw, err := secretfile.Read(host.Issuer.TokenFile)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, errors.New("empty GitLab issuer token")
	}
	return &Provider{Config: host, Issuer: bearerIssuer{client: a, host: host.Host, token: token}, Duration: duration, Now: time.Now, Steps: steps}, nil
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Settler  = (*Provider)(nil)
)

// ID names the instance, e.g. gitlab:ops.gitlab.net.
func (g *Provider) ID() string   { return "gitlab:" + g.Config.Host }
func (g *Provider) Kind() string { return "gitlab" }

// Setup points glab at the session's config.yml, which Renderer writes, and
// git at the session PAT for the allowlisted namespaces. It protects the
// user's own glab login and a stored issuer PAT.
func (g *Provider) Setup() provider.Setup {
	var protect []string
	if dir, err := glabConfigDir(os.Getenv, os.UserConfigDir); err == nil {
		protect = append(protect, dir)
	}
	if g.Config.Issuer.TokenFile != "" {
		protect = append(protect, g.Config.Issuer.TokenFile)
	}
	return provider.Setup{
		Env: map[string]string{"GLAB_CONFIG_DIR": "$GEAS_SESSION_DIR/" + configDir},
		Git: []provider.GitHost{{
			Host:     g.Config.Host,
			Username: "oauth2",
			// A namespace covers what is below it; a project's own path is
			// its repository. Which a path is, only the lookup knows.
			Namespaces:   slices.Clone(g.Config.Namespaces),
			Repositories: slices.Clone(g.Config.Namespaces),
		}},
		Protect: protect,
	}
}

// glabConfigDir is where glab keeps the user's login, resolved as glab
// resolves it: GLAB_CONFIG_DIR, else $XDG_CONFIG_HOME/glab-cli, else the OS's
// user configuration directory, which on macOS is
// ~/Library/Application Support/glab-cli.
func glabConfigDir(getenv func(string) string, userConfigDir func() (string, error)) (string, error) {
	if dir := getenv("GLAB_CONFIG_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	if xdg := getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Abs(filepath.Join(xdg, "glab-cli"))
	}
	base, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "glab-cli"), nil
}

// Acquire mints the session PAT. Its name is in the ledger before the mint;
// Settle resolves its ID afterwards, once the token is handed over.
func (g *Provider) Acquire(ctx context.Context, l *ledger.Ledger) (provider.Credential, error) {
	token, err := g.create(ctx, l, g.Duration)
	if err != nil {
		return provider.Credential{}, err
	}
	expires, err := time.Parse("2006-01-02", config.ExpiryForDuration(g.Now(), g.Duration))
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.Credential{Secret: token, User: g.Config.ExpectedUser, Expires: expires}, nil
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
	g.Steps.Step("lookup")
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
	// Nothing exists yet: a session that has ended mints nothing.
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
	// Once sent, the mint runs to its answer, within the issuer's own
	// timeout, even when the harness exits meanwhile: the exit then revokes a
	// PAT it knows exists, and never takes a mint cut short for one that
	// created nothing.
	if err := g.Issuer.graphql(context.WithoutCancel(ctx), createPAT, variables, &result); err != nil {
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
	g.unresolved = append(g.unresolved, entry)
	g.mu.Unlock()
	return result.Data.Create.Token, nil
}

// Settle resolves the IDs of the PATs this process minted, after their
// tokens are handed over: the mutation returns no ID, and the exit, which may
// have little time, then needs a single DELETE. Until it does, the ledger
// has the PAT's name, which the exit and the reaper search for.
func (g *Provider) Settle(ctx context.Context, l *ledger.Ledger) error {
	g.mu.Lock()
	entries := g.unresolved
	g.unresolved = nil
	g.mu.Unlock()
	var errs []error
	for _, entry := range entries {
		id, err := g.find(ctx, entry.Name)
		g.Steps.Step("id lookup")
		if errors.Is(err, ledger.ErrNotFound) {
			err = errUnresolved
		}
		if err == nil {
			entry.ID = id
			err = l.Record(entry)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// lookup is the issuer lookup's answer: the issuer's user and every namespace
// the allowlist names, in configuration order.
type lookup struct {
	user       string
	namespaces []namespace
}

// The kinds of namespace, by the model of the global ID namespace(fullPath:)
// answers with.
const (
	groupNamespace   = "Group"
	userNamespace    = "Namespaces::UserNamespace"
	projectNamespace = "Namespaces::ProjectNamespace"
)

// namespace is one configured path, resolved: its kind, and the global ID a
// granular scope names it by, which for a project namespace is the project's
// own, gid://gitlab/Project/<id>; a user namespace needs none.
type namespace struct {
	kind     string
	id       string
	fullPath string
}

// lookupDocument builds the one aliased query that replaces the REST user call
// and the per-namespace lookups: currentUser for the identity check, and for
// each configured path both namespace(fullPath:), whose global ID's model
// says whether the path is a group, a user's namespace or a project, and
// project(fullPath:), for a project's own ID. GraphQL never embeds a group's
// projects, so the 6 s REST answer disappears with it.
//
// The paths are written into the document as escaped string literals: an alias
// needs one literal argument each, and the glab issuer carries only objects and
// arrays in its -F variables, never the scalar a $path variable would need.
func lookupDocument(c config.GitLabConfig) string {
	var b strings.Builder
	b.WriteString("query SessionLookup {\n  currentUser { username }\n")
	for i, path := range c.Namespaces {
		fmt.Fprintf(&b, "  n%d: namespace(fullPath: %s) { id fullPath }\n", i, graphqlString(path))
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

// globalModel returns the model of a GraphQL global ID, such as Group for
// gid://gitlab/Group/9970, or "" when value is not one.
func globalModel(value string) string {
	rest, ok := strings.CutPrefix(value, "gid://gitlab/")
	model, id, found := strings.Cut(rest, "/")
	if !ok || !found {
		return ""
	}
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
		return ""
	}
	return model
}

// lookup resolves the user and every configured namespace in one request, and
// refuses a path that does not resolve, or resolves to another path, as the
// REST lookups did. Which kind each namespace is comes from its global ID's
// model; whether that kind may be granted is for scopes to decide.
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
	for i, path := range g.Config.Namespaces {
		node, err := pathNode(answer.Data, fmt.Sprintf("n%d", i), "namespace", path)
		if err != nil {
			return lookup{}, err
		}
		resolved.namespaces = append(resolved.namespaces, node)
		ns := &resolved.namespaces[i]
		switch ns.kind {
		case groupNamespace, userNamespace:
		case projectNamespace:
			// A granular scope names a project by its own ID, not by its
			// namespace's.
			project, err := pathNode(answer.Data, fmt.Sprintf("p%d", i), "project", path)
			if err != nil {
				return lookup{}, err
			}
			if project.kind != "Project" {
				return lookup{}, fmt.Errorf("GitLab project %s resolves without a project ID; update the allowlist", path)
			}
			ns.id = project.id
		default:
			return lookup{}, fmt.Errorf("GitLab namespace %s is neither a group, a user nor a project; update the allowlist", path)
		}
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

// pathNode resolves one alias of a configured path and refuses anything but
// the configured path, in the same case, and a well-formed global ID, whose
// model it returns as the kind.
func pathNode(data map[string]json.RawMessage, alias, what, path string) (namespace, error) {
	var node struct {
		ID       string `json:"id"`
		FullPath string `json:"fullPath"`
	}
	if err := decodeAlias(data, alias, what+" "+path, &node); err != nil {
		return namespace{}, err
	}
	if node.FullPath != path {
		return namespace{}, fmt.Errorf("GitLab %s %s resolves to another path; update the allowlist", what, path)
	}
	model := globalModel(node.ID)
	if model == "" {
		return namespace{}, fmt.Errorf("GitLab %s %s resolves without an ID; update the allowlist", what, path)
	}
	return namespace{kind: model, id: node.ID, fullPath: node.FullPath}, nil
}

// scopes returns the session PAT's scopes from the resolved namespaces: each
// group and project in SELECTED_MEMBERSHIPS, by its ID, and the user's own
// namespace as PERSONAL_PROJECTS. Another user's namespace is refused: it
// would take PERSONAL_PROJECTS, which only ever means the token owner's.
func (g *Provider) scopes(resolved lookup) ([]config.Scope, error) {
	allowed := map[string]bool{}
	var ids []string
	personal := false
	for _, ns := range resolved.namespaces {
		switch ns.kind {
		case userNamespace:
			if ns.fullPath != g.Config.ExpectedUser || ns.fullPath != resolved.user {
				return nil, fmt.Errorf("GitLab namespace %s belongs to another user; only expected_user's own is allowed", ns.fullPath)
			}
			personal = true
		case groupNamespace, projectNamespace:
			allowed[ns.id] = true
			ids = append(ids, ns.id)
		default:
			return nil, fmt.Errorf("GitLab namespace %s is neither a group, a user nor a project", ns.fullPath)
		}
	}
	scopes := namespacedScopes(g.Config.Permissions, ids, personal)
	if err := config.CheckScopes(scopes, allowed, personal); err != nil {
		return nil, err
	}
	return scopes, nil
}

// namespacedScopes grants read_user, to verify the user, and the permissions
// on each resolved group and project, and on the user's personal projects
// when personal. A group covers its subgroups and their projects.
func namespacedScopes(permissions, resourceIDs []string, personal bool) []config.Scope {
	scopes := []config.Scope{{Access: "USER", Permissions: []string{"read_user"}}}
	if len(resourceIDs) > 0 {
		scopes = append(scopes, config.Scope{Access: "SELECTED_MEMBERSHIPS", ResourceIDs: resourceIDs, Permissions: permissions})
	}
	if personal {
		scopes = append(scopes, config.Scope{Access: "PERSONAL_PROJECTS", Permissions: permissions})
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
