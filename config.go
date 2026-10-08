package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
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
	ClientID     string   `json:"client_id"`
	ExpectedUser string   `json:"expected_user"`
	StateFile    string   `json:"state_file"`
	Repositories []string `json:"repositories"`
}
type GitLabConfig struct {
	Enabled         *bool    `json:"enabled,omitempty"`
	ExpectedUser    string   `json:"expected_user"`
	IssuerTokenFile string   `json:"issuer_token_file"`
	Repositories    []string `json:"repositories"`
	ProjectIDs      []int64  `json:"project_ids"`
	Scopes          []Scope  `json:"granular_scopes"`
}
type Config struct {
	SessionHours float64      `json:"session_hours"`
	GitHub       GitHubConfig `json:"github"`
	GitLab       GitLabConfig `json:"gitlab"`
}

func enabled(value *bool) bool           { return value == nil || *value }
func (c Config) duration() time.Duration { return time.Duration(c.SessionHours * float64(time.Hour)) }
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
func loadConfig(path string) (Config, error) {
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
	base := filepath.Dir(absolute)
	if enabled(config.GitHub.Enabled) {
		config.GitHub.StateFile, err = expandPath(config.GitHub.StateFile, base)
		if err != nil {
			return config, err
		}
	}
	if enabled(config.GitLab.Enabled) {
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
	if !enabled(c.GitHub.Enabled) && !enabled(c.GitLab.Enabled) {
		return errors.New("enable at least one provider")
	}
	for name, repos := range map[string][]string{"github": c.GitHub.Repositories, "gitlab": c.GitLab.Repositories} {
		active := enabled(c.GitHub.Enabled)
		if name == "gitlab" {
			active = enabled(c.GitLab.Enabled)
		}
		if !active {
			continue
		}
		if len(repos) == 0 {
			return fmt.Errorf("configure an explicit %s repository allowlist", name)
		}
		seen := map[string]bool{}
		for _, repo := range repos {
			normalized, ok := normalizeGitPath(repo)
			if !ok || normalized != repo || !strings.Contains(repo, "/") || strings.Contains(repo, ":") {
				return errors.New("repository paths must look like owner/repo or group/project")
			}
			key := repo
			if name == "github" {
				key = strings.ToLower(key)
			}
			if seen[key] {
				return errors.New("duplicate repository in allowlist")
			}
			seen[key] = true
		}
	}
	if enabled(c.GitHub.Enabled) && (c.GitHub.ClientID == "" || c.GitHub.ExpectedUser == "" || c.GitHub.StateFile == "") {
		return errors.New("configure GitHub client_id, expected_user and state_file")
	}
	if enabled(c.GitLab.Enabled) {
		if c.GitLab.ExpectedUser == "" || c.GitLab.IssuerTokenFile == "" {
			return errors.New("configure GitLab expected_user and issuer_token_file")
		}
		if err := validateScopes(c.GitLab); err != nil {
			return err
		}
	}
	return nil
}
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
	for _, scope := range c.Scopes {
		if len(scope.Permissions) == 0 {
			return errors.New("empty granular permission list")
		}
		if scope.Access == "USER" {
			if len(scope.ResourceIDs) != 0 {
				return errors.New("USER scope cannot contain resourceIds")
			}
			for _, p := range scope.Permissions {
				if p != "read_user" {
					return errors.New("agent USER scope only allows read_user")
				}
			}
		} else {
			if scope.Access != "SELECTED_MEMBERSHIPS" || len(scope.ResourceIDs) == 0 {
				return errors.New("agent scopes must use selected projects")
			}
			for _, id := range scope.ResourceIDs {
				if !allowed[id] {
					return errors.New("granular scope references an unconfigured project")
				}
			}
		}
		for _, p := range scope.Permissions {
			if strings.Contains(p, "personal_access_token") {
				return errors.New("agent cannot receive PAT management permissions")
			}
		}
	}
	return nil
}
func normalizeGitPath(path string) (string, bool) {
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
func expiryForDuration(now time.Time, duration time.Duration) string {
	target := now.UTC().Add(duration)
	midnight := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.UTC)
	// Exact midnight already covers the requested duration.
	if target.After(midnight) {
		midnight = midnight.AddDate(0, 0, 1)
	}
	return midnight.Format("2006-01-02")
}
