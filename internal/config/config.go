// Package config reads the configuration file and checks it: the schema, the
// validation every provider relies on, and the path expansion that turns the
// configuration's relative and ~-prefixed paths into absolute ones.
//
// The file is version 2: a list of providers, one per credential source and
// host. Version 1, with its github and gitlab sections, is refused with the
// first of its fields the file still has.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Version is the configuration format this geas reads.
const Version = 2

type Scope struct {
	Access      string   `json:"access"`
	ResourceIDs []string `json:"resourceIds,omitempty"`
	Permissions []string `json:"permissions"`
}

// Issuer is the credential a GitLab provider mints, finds and revokes its
// session PATs with: the user's own glab login for the host, or a PAT stored
// in TokenFile and sent as a bearer token.
type Issuer struct {
	Type      string `json:"type"`
	TokenFile string `json:"token_file,omitempty"`
}

// GitLabConfig is one GitLab instance.
type GitLabConfig struct {
	Host             string   `json:"host"`
	Issuer           Issuer   `json:"issuer"`
	ExpectedUser     string   `json:"expected_user"`
	Groups           []string `json:"groups,omitempty"`
	PersonalProjects bool     `json:"personal_projects,omitempty"`
	Projects         []string `json:"projects,omitempty"`
	Permissions      []string `json:"permissions"`
}

// GitHubConfig is GitHub, through a GitHub App's user tokens.
type GitHubConfig struct {
	Host         string `json:"host"`
	ClientID     string `json:"client_id"`
	ExpectedUser string `json:"expected_user"`
	StateFile    string `json:"state_file"`
	// Accounts or organizations whose repositories are all allowed.
	Owners       []string `json:"owners,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
}

// Config is the configuration, its providers grouped by type, each type's in
// the file's order.
type Config struct {
	SessionHours float64
	GitLab       []GitLabConfig
	GitHub       []GitHubConfig
	source       string // absolute path, for the reaper to load it again
}

// file is the configuration as it is written.
type file struct {
	Version      int               `json:"version"`
	SessionHours float64           `json:"session_hours,omitempty"`
	Providers    []json.RawMessage `json:"providers"`
}

// The provider types, as "type" names them.
const (
	TypeGitLab = "gitlab"
	TypeGitHub = "github"
	// TypeSSH is reserved for SSH certificates, which a later version adds.
	TypeSSH = "ssh"
)

// gitlabEntry and githubEntry are provider entries as they are written,
// with their type.
type gitlabEntry struct {
	Type string `json:"type"`
	GitLabConfig
}
type githubEntry struct {
	Type string `json:"type"`
	GitHubConfig
}

// MarshalJSON writes the configuration as version 2.
func (c Config) MarshalJSON() ([]byte, error) {
	f := file{Version: Version, SessionHours: c.SessionHours}
	for _, g := range c.GitLab {
		raw, err := json.Marshal(gitlabEntry{TypeGitLab, g})
		if err != nil {
			return nil, err
		}
		f.Providers = append(f.Providers, raw)
	}
	for _, g := range c.GitHub {
		raw, err := json.Marshal(githubEntry{TypeGitHub, g})
		if err != nil {
			return nil, err
		}
		f.Providers = append(f.Providers, raw)
	}
	return json.Marshal(f)
}

// Duration is the session's length.
func (c Config) Duration() time.Duration { return time.Duration(c.SessionHours * float64(time.Hour)) }

// Source is the configuration's absolute path, which the reaper loads again
// from its own process.
func (c Config) Source() string { return c.source }

// DefaultPath is where the configuration is unless --config names another:
// $XDG_CONFIG_HOME/geas/config.json, else ~/.config/geas/config.json, on
// macOS too.
func DefaultPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "geas", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("cannot find the configuration: set XDG_CONFIG_HOME or HOME, or pass --config")
	}
	return filepath.Join(home, ".config", "geas", "config.json"), nil
}

func expandPath(value, base string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return filepath.Clean(value), nil
}

func Load(path string) (Config, error) {
	var config Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return config, errors.New("cannot read configuration")
	}
	if config, err = parse(raw); err != nil {
		return config, err
	}
	if err = config.validate(); err != nil {
		return config, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return config, err
	}
	config.source = absolute
	base := filepath.Dir(absolute)
	for i := range config.GitHub {
		if config.GitHub[i].StateFile, err = expandPath(config.GitHub[i].StateFile, base); err != nil {
			return config, err
		}
	}
	for i := range config.GitLab {
		if file := &config.GitLab[i].Issuer.TokenFile; *file != "" {
			if *file, err = expandPath(*file, base); err != nil {
				return config, err
			}
		}
	}
	return config, config.validate()
}

// parse decodes a version 2 configuration, refusing unknown fields, and
// names the first version 1 field it finds in a file of the earlier format.
func parse(raw []byte) (Config, error) {
	var config Config
	if err := findVersion1(raw); err != nil {
		return config, err
	}
	var f file
	if err := strictDecode(raw, &f); err != nil {
		return config, err
	}
	if f.Version != Version {
		return config, fmt.Errorf("configuration version %d is not supported: geas reads version %d", f.Version, Version)
	}
	config.SessionHours = f.SessionHours
	if config.SessionHours == 0 {
		config.SessionHours = 24
	}
	for i, entry := range f.Providers {
		where := fmt.Sprintf("providers[%d]", i)
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry, &kind) != nil {
			return config, errors.New(where + " must be an object")
		}
		switch kind.Type {
		case TypeGitLab:
			var g gitlabEntry
			if err := strictDecode(entry, &g); err != nil {
				return config, fmt.Errorf("%s: %w", where, err)
			}
			config.GitLab = append(config.GitLab, g.GitLabConfig)
		case TypeGitHub:
			var g githubEntry
			if err := strictDecode(entry, &g); err != nil {
				return config, fmt.Errorf("%s: %w", where, err)
			}
			config.GitHub = append(config.GitHub, g.GitHubConfig)
		case TypeSSH:
			return config, fmt.Errorf("%s: provider type %q is not supported yet", where, kind.Type)
		case "":
			return config, errors.New(where + ` needs a "type": gitlab or github`)
		default:
			return config, fmt.Errorf("%s: unknown provider type %q", where, kind.Type)
		}
	}
	return config, nil
}

// strictDecode decodes one JSON object into out, refusing unknown fields and
// anything after the object.
func strictDecode(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return errors.New("unknown field " + field)
		}
		return errors.New("invalid configuration JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("configuration must contain one JSON object")
	}
	return nil
}

// version1 are the top-level fields of version 1, with what replaced them.
var version1 = map[string]string{
	"github": `each provider is an entry of "providers" now, with "type": "github"`,
	"gitlab": `each provider is an entry of "providers" now, with "type": "gitlab" and a "host"`,
}

// version1Provider are the fields version 1 had in a provider's section, by
// provider type, with what replaced them.
var version1Provider = map[string]map[string]string{
	TypeGitLab: {
		"enabled":           "leave the provider out instead",
		"optional":          "a provider that fails never stops the session",
		"issuer_token_file": `write "issuer": {"type": "bearer", "token_file": ...}`,
		"repositories":      "list groups and projects by path",
		"project_ids":       "list groups and projects by path",
		"granular_scopes":   "list groups and projects by path, and permissions",
	},
	TypeGitHub: {
		"enabled":  "leave the provider out instead",
		"optional": "a provider that fails never stops the session",
	},
}

// findVersion1 names the first version 1 field of raw, in the file's order:
// version 1 had no version field, and its sections have no version 2
// equivalent to decode into.
func findVersion1(raw []byte) error {
	keys, values, err := objectFields(raw)
	if err != nil {
		return errors.New("invalid configuration JSON")
	}
	for i, key := range keys {
		if hint, ok := version1[key]; ok {
			return v1Error("", key, hint)
		}
		if key != "providers" {
			continue
		}
		var providers []json.RawMessage
		if json.Unmarshal(values[i], &providers) != nil {
			continue
		}
		for n, entry := range providers {
			fields, entryValues, err := objectFields(entry)
			if err != nil {
				continue
			}
			kind := ""
			if at := slices.Index(fields, "type"); at >= 0 {
				json.Unmarshal(entryValues[at], &kind)
			}
			for j, field := range fields {
				if hint, ok := version1Provider[kind][field]; ok {
					return v1Error(fmt.Sprintf("providers[%d]: ", n), field, hint)
				}
				// Version 1 named the issuer with a string.
				if field == "issuer" && kind == TypeGitLab && bytes.HasPrefix(bytes.TrimSpace(entryValues[j]), []byte(`"`)) {
					return v1Error(fmt.Sprintf("providers[%d]: ", n), field, `write "issuer": {"type": "glab"}`)
				}
			}
		}
	}
	if !slices.Contains(keys, "version") {
		return fmt.Errorf(`the configuration has no "version": write "version": %d, see "Migrating from version 1" in the README`, Version)
	}
	return nil
}

func v1Error(where, field, hint string) error {
	return fmt.Errorf(`%s%q is a version 1 field: %s; see "Migrating from version 1" in the README`, where, field, hint)
}

// objectFields returns a JSON object's keys and values in the file's order.
func objectFields(raw []byte) ([]string, []json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, errors.New("not a JSON object")
	}
	var keys []string
	var values []json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, errors.New("not a JSON object")
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		values = append(values, value)
	}
	return keys, values, nil
}

func (c Config) validate() error {
	if math.IsNaN(c.SessionHours) || math.IsInf(c.SessionHours, 0) || c.SessionHours <= 0 || c.SessionHours > 48 {
		return errors.New("session_hours must be greater than zero and at most 48")
	}
	if len(c.GitLab) == 0 && len(c.GitHub) == 0 {
		return errors.New("configure at least one provider")
	}
	// A host has one provider: Git asks the credential helper by host.
	hosts := map[string]bool{}
	claim := func(host string) error {
		if hosts[host] {
			return fmt.Errorf("two providers for host %s", host)
		}
		hosts[host] = true
		return nil
	}
	for _, g := range c.GitLab {
		if err := validateGitLab(g); err != nil {
			return err
		}
		if err := claim(g.Host); err != nil {
			return err
		}
	}
	for _, g := range c.GitHub {
		if err := validateGitHub(g); err != nil {
			return err
		}
		if err := claim(g.Host); err != nil {
			return err
		}
	}
	return nil
}

func validateGitHub(g GitHubConfig) error {
	if g.Host != "github.com" {
		return errors.New(`the github provider's host must be "github.com", the only GitHub host supported`)
	}
	if len(g.Repositories) == 0 && len(g.Owners) == 0 {
		return errors.New("configure GitHub repositories or owners")
	}
	if err := validatePaths(g.Repositories, true, true); err != nil {
		return err
	}
	if err := validateOwners(g.Owners); err != nil {
		return err
	}
	if g.ClientID == "" || g.ExpectedUser == "" || g.StateFile == "" {
		return errors.New("configure GitHub client_id, expected_user and state_file")
	}
	return nil
}

func validateGitLab(g GitLabConfig) error {
	if g.Host != "gitlab.com" {
		return errors.New(`the gitlab provider's host must be "gitlab.com"`)
	}
	where := "gitlab provider " + g.Host + ": "
	if g.ExpectedUser == "" {
		return errors.New(where + "configure expected_user")
	}
	switch g.Issuer.Type {
	case "glab":
		if g.Issuer.TokenFile != "" {
			return errors.New(where + `the glab issuer takes no token_file`)
		}
	case "bearer":
		if g.Issuer.TokenFile == "" {
			return errors.New(where + `the bearer issuer needs a token_file`)
		}
	default:
		return errors.New(where + `the issuer's type must be "glab" or "bearer"`)
	}
	if err := ValidateGitLabAllowlist(g); err != nil {
		return errors.New(where + err.Error())
	}
	return nil
}

// validatePaths accepts canonical, distinct Git paths; a project needs a
// namespace, a group may be top-level. GitHub compares them case-insensitively.
func validatePaths(paths []string, project, fold bool) error {
	seen := map[string]bool{}
	for _, path := range paths {
		normalized, ok := NormalizeGitPath(path)
		if !ok || normalized != path || strings.Contains(path, ":") || (project && !strings.Contains(path, "/")) {
			if project {
				return errors.New("repository paths must look like owner/repo or group/project")
			}
			return errors.New("GitLab groups must look like group or group/subgroup")
		}
		key := path
		if fold {
			key = strings.ToLower(key)
		}
		if seen[key] {
			return errors.New("duplicate path in allowlist")
		}
		seen[key] = true
	}
	return nil
}

// validateOwners accepts distinct GitHub accounts or organizations, one path
// segment each, compared case-insensitively.
func validateOwners(owners []string) error {
	seen := map[string]bool{}
	for _, owner := range owners {
		normalized, ok := NormalizeGitPath(owner)
		if !ok || normalized != owner || strings.ContainsAny(owner, "/:*") {
			return errors.New("GitHub owners must be an account or organization name, without slash or wildcard")
		}
		if seen[strings.ToLower(owner)] {
			return errors.New("duplicate path in allowlist")
		}
		seen[strings.ToLower(owner)] = true
	}
	return nil
}

// ValidateGitLabAllowlist checks the GitLab allowlist and the permissions it
// asks for. The session and the provider both refuse a session PAT that
// would be broader than the configuration.
func ValidateGitLabAllowlist(c GitLabConfig) error {
	if len(c.Groups) == 0 && !c.PersonalProjects && len(c.Projects) == 0 {
		return errors.New("configure GitLab groups, personal_projects or projects")
	}
	if err := validatePaths(c.Groups, false, false); err != nil {
		return err
	}
	if slices.Contains(c.Groups, c.ExpectedUser) {
		return errors.New("your own namespace is not a group: use personal_projects")
	}
	if err := validatePaths(c.Projects, true, false); err != nil {
		return err
	}
	return checkPermissions(c.Permissions)
}

// CheckScopes refuses what a session PAT must never get: PAT management, USER
// beyond read_user, resources outside allowed, and ALL_MEMBERSHIPS, INSTANCE or
// (unless personal) PERSONAL_PROJECTS.
func CheckScopes(scopes []Scope, allowed map[string]bool, personal bool) error {
	for _, scope := range scopes {
		switch scope.Access {
		case "USER":
			if len(scope.ResourceIDs) != 0 {
				return errors.New("USER scope cannot contain resourceIds")
			}
			for _, p := range scope.Permissions {
				if p != "read_user" {
					return errors.New("agent USER scope only allows read_user")
				}
			}
		case "SELECTED_MEMBERSHIPS":
			if len(scope.ResourceIDs) == 0 {
				return errors.New("agent scopes must use selected groups or projects")
			}
			for _, id := range scope.ResourceIDs {
				if !allowed[id] {
					return errors.New("granular scope references an unconfigured project")
				}
			}
		case "PERSONAL_PROJECTS":
			if !personal || len(scope.ResourceIDs) != 0 {
				return errors.New("PERSONAL_PROJECTS needs personal_projects and no resourceIds")
			}
		default:
			return errors.New("agent scopes must use selected groups or projects")
		}
		if err := checkPermissions(scope.Permissions); err != nil {
			return err
		}
	}
	return nil
}
func checkPermissions(permissions []string) error {
	if len(permissions) == 0 {
		return errors.New("empty granular permission list")
	}
	for _, p := range permissions {
		if strings.Contains(p, "personal_access_token") {
			return errors.New("agent cannot receive PAT management permissions")
		}
	}
	return nil
}

// NormalizeGitPath accepts a canonical Git path, as a repository's owner or
// group path is written in a URL or in the configuration, and refuses
// anything that could name another repository: empty segments, ".", "..",
// escapes and separators.
func NormalizeGitPath(path string) (string, bool) {
	if path == "" || strings.ContainsAny(path, "%\\\r\n?#\x00") {
		return "", false
	}
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	return path, true
}

// ExpiryForDuration is the UTC date a session PAT may live until: the
// requested duration rounded up to the next midnight, so a token never
// expires in the middle of a session.
func ExpiryForDuration(now time.Time, duration time.Duration) string {
	target := now.UTC().Add(duration)
	midnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.UTC)
	// Exact midnight already covers the requested duration.
	if target.After(midnight) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight.Format("2006-01-02")
}
