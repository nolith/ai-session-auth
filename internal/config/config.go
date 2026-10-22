// Package config reads the configuration file and checks it: the schema, the
// validation every provider relies on, and the path expansion that turns the
// configuration's relative and ~-prefixed paths into absolute ones.
package config

import (
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

type Scope struct {
	Access      string   `json:"access"`
	ResourceIDs []string `json:"resourceIds,omitempty"`
	Permissions []string `json:"permissions"`
}
type GitHubConfig struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	Optional     bool     `json:"optional,omitempty"`
	ClientID     string   `json:"client_id"`
	ExpectedUser string   `json:"expected_user"`
	StateFile    string   `json:"state_file"`
	Repositories []string `json:"repositories"`
	// Accounts or organizations whose repositories are all allowed.
	Owners []string `json:"owners,omitempty"`
}
type GitLabConfig struct {
	Enabled          *bool    `json:"enabled,omitempty"`
	Optional         bool     `json:"optional,omitempty"`
	ExpectedUser     string   `json:"expected_user"`
	Issuer           string   `json:"issuer,omitempty"`
	IssuerTokenFile  string   `json:"issuer_token_file,omitempty"`
	Groups           []string `json:"groups,omitempty"`
	PersonalProjects bool     `json:"personal_projects,omitempty"`
	Projects         []string `json:"projects,omitempty"`
	Permissions      []string `json:"permissions,omitempty"`
	// The earlier format: exact paths, their numeric IDs, and scopes by hand.
	Repositories []string `json:"repositories,omitempty"`
	ProjectIDs   []int64  `json:"project_ids,omitempty"`
	Scopes       []Scope  `json:"granular_scopes,omitempty"`
}
type Config struct {
	SessionHours float64      `json:"session_hours"`
	GitHub       GitHubConfig `json:"github"`
	GitLab       GitLabConfig `json:"gitlab"`
	source       string       // absolute path, for the reaper to load it again
}

// Namespaced reports whether the GitLab allowlist names groups, personal
// projects and project paths, whose IDs are resolved at session start.
func (c GitLabConfig) Namespaced() bool {
	return len(c.Groups) > 0 || c.PersonalProjects || len(c.Projects) > 0 || len(c.Permissions) > 0
}

// Enabled reports whether a provider's enabled field is on: it is nil unless
// the configuration turns the provider off.
func Enabled(value *bool) bool { return value == nil || *value }

// Duration is the session's length.
func (c Config) Duration() time.Duration { return time.Duration(c.SessionHours * float64(time.Hour)) }

// Source is the configuration's absolute path, which the reaper loads again
// from its own process.
func (c Config) Source() string { return c.source }

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
	config.SessionHours = 24
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return config, errors.New("invalid configuration JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return config, errors.New("configuration must contain one JSON object")
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
	if Enabled(config.GitHub.Enabled) {
		config.GitHub.StateFile, err = expandPath(config.GitHub.StateFile, base)
		if err != nil {
			return config, err
		}
	}
	if Enabled(config.GitLab.Enabled) && config.GitLab.IssuerTokenFile != "" {
		config.GitLab.IssuerTokenFile, err = expandPath(config.GitLab.IssuerTokenFile, base)
		if err != nil {
			return config, err
		}
	}
	return config, config.validate()
}
func (c Config) validate() error {
	if math.IsNaN(c.SessionHours) || math.IsInf(c.SessionHours, 0) || c.SessionHours <= 0 || c.SessionHours > 48 {
		return errors.New("session_hours must be greater than zero and at most 48")
	}
	if !Enabled(c.GitHub.Enabled) && !Enabled(c.GitLab.Enabled) {
		return errors.New("enable at least one provider")
	}
	if Enabled(c.GitHub.Enabled) {
		if len(c.GitHub.Repositories) == 0 && len(c.GitHub.Owners) == 0 {
			return errors.New("configure GitHub repositories or owners")
		}
		if err := validatePaths(c.GitHub.Repositories, true, true); err != nil {
			return err
		}
		if err := validateOwners(c.GitHub.Owners); err != nil {
			return err
		}
		if c.GitHub.ClientID == "" || c.GitHub.ExpectedUser == "" || c.GitHub.StateFile == "" {
			return errors.New("configure GitHub client_id, expected_user and state_file")
		}
	}
	if Enabled(c.GitLab.Enabled) {
		if c.GitLab.ExpectedUser == "" {
			return errors.New("configure GitLab expected_user")
		}
		switch c.GitLab.Issuer {
		case "":
			if c.GitLab.IssuerTokenFile == "" {
				return errors.New(`configure GitLab issuer_token_file, or issuer "glab"`)
			}
		case "glab":
			if c.GitLab.IssuerTokenFile != "" {
				return errors.New(`GitLab issuer "glab" and issuer_token_file are mutually exclusive`)
			}
		default:
			return errors.New(`GitLab issuer must be "glab" or omitted`)
		}
		if err := ValidateGitLabAllowlist(c.GitLab); err != nil {
			return err
		}
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

// ValidateGitLabAllowlist checks the GitLab allowlist, in either format, and
// the permissions it asks for. The session and the provider both refuse a
// session PAT that would be broader than the configuration.
func ValidateGitLabAllowlist(c GitLabConfig) error {
	if !c.Namespaced() {
		if len(c.Repositories) == 0 {
			return errors.New("configure GitLab groups, personal_projects or projects")
		}
		if err := validatePaths(c.Repositories, true, false); err != nil {
			return err
		}
		return validateScopes(c)
	}
	if len(c.Repositories) > 0 || len(c.ProjectIDs) > 0 || len(c.Scopes) > 0 {
		return errors.New("GitLab groups, personal_projects, projects and permissions replace repositories, project_ids and granular_scopes: use one format")
	}
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

// validateScopes checks the earlier format's hand-written scopes against its
// project IDs.
func validateScopes(c GitLabConfig) error {
	allowed := map[string]bool{}
	for _, id := range c.ProjectIDs {
		if id <= 0 {
			return errors.New("GitLab project IDs must be positive")
		}
		key := fmt.Sprintf("gid://gitlab/Project/%d", id)
		if allowed[key] {
			return errors.New("duplicate GitLab project ID")
		}
		allowed[key] = true
	}
	if len(allowed) == 0 || len(c.Scopes) == 0 {
		return errors.New("configure GitLab project_ids and granular_scopes")
	}
	return CheckScopes(c.Scopes, allowed, false)
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
