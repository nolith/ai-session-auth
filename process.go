package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func environmentMap(values []string) map[string]string {
	result := map[string]string{}
	for _, value := range values {
		key, v, ok := strings.Cut(value, "=")
		if ok {
			result[key] = v
		}
	}
	return result
}
func environmentList(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func appendGitConfig(env map[string]string, entries [][2]string) error {
	count := 0
	if value := env["GIT_CONFIG_COUNT"]; value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 0 || count > 256 {
			return errors.New("invalid inherited GIT_CONFIG_COUNT")
		}
	}
	for _, entry := range entries {
		env[fmt.Sprintf("GIT_CONFIG_KEY_%d", count)] = entry[0]
		env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)] = entry[1]
		count++
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(count)
	return nil
}
func sessionEnvironment(config Config, directory, address, binary string, base []string, lookup func(string) (string, error)) ([]string, error) {
	env := environmentMap(base)
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "CI_JOB_TOKEN", "SSH_AUTH_SOCK", "GH_DEBUG", "GLAB_DEBUG", "GLAB_DEBUG_HTTP"} {
		delete(env, key)
	}
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		return nil, err
	}
	for _, provider := range []struct {
		service, cli string
		active       bool
	}{{"github", "gh", enabled(config.GitHub.Enabled)}, {"gitlab", "glab", enabled(config.GitLab.Enabled)}} {
		if !provider.active {
			continue
		}
		executable, err := lookup(provider.cli)
		if err != nil {
			return nil, fmt.Errorf("install %s and add it to PATH", provider.cli)
		}
		absolute, err := filepath.Abs(executable)
		if err != nil {
			return nil, err
		}
		wrapper := "#!/bin/sh\nexec " + shellQuote(binary) + " _proxy " + shellQuote(provider.service) + " " + shellQuote(absolute) + " \"$@\"\n"
		if err = os.WriteFile(filepath.Join(bin, provider.cli), []byte(wrapper), 0700); err != nil {
			return nil, err
		}
	}
	env["PATH"] = bin + string(os.PathListSeparator) + env["PATH"]
	env["AI_AUTH_SOCKET"] = address
	env["GH_PROMPT_DISABLED"] = "1"
	env["GIT_TERMINAL_PROMPT"] = "0"
	for _, cli := range []string{"gh", "glab"} {
		path := filepath.Join(directory, cli)
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, err
		}
		content := "git_protocol: https\n"
		key := "GH_CONFIG_DIR"
		if cli == "glab" {
			key = "GLAB_CONFIG_DIR"
			content = "host: gitlab.com\nhosts:\n  gitlab.com:\n    git_protocol: https\n    api_protocol: https\n    api_host: gitlab.com\n"
		}
		if err := os.WriteFile(filepath.Join(path, "config.yml"), []byte(content), 0600); err != nil {
			return nil, err
		}
		env[key] = path
	}
	entries := [][2]string{{"credential.helper", ""}, {"credential.helper", "!" + shellQuote(binary) + " _credential"}, {"credential.useHttpPath", "true"}}
	for _, provider := range []struct {
		host   string
		active bool
	}{{"github.com", enabled(config.GitHub.Enabled)}, {"gitlab.com", enabled(config.GitLab.Enabled)}} {
		if provider.active {
			for _, prefix := range []string{"git@" + provider.host + ":", "ssh://git@" + provider.host + "/"} {
				entries = append(entries, [2]string{"url.https://" + provider.host + "/.insteadOf", prefix})
			}
		}
	}
	if err := appendGitConfig(env, entries); err != nil {
		return nil, err
	}
	return environmentList(env), nil
}
func proxyCLI(service, executable string, args []string) error {
	if service != "github" && service != "gitlab" {
		return errors.New("invalid provider")
	}
	response, err := rpc(BrokerRequest{Service: service})
	if err != nil {
		return err
	}
	if response.Token == "" {
		return errors.New("broker did not return a token")
	}
	environment := environmentMap(os.Environ())
	key := "GH_TOKEN"
	if service == "gitlab" {
		key = "GITLAB_TOKEN"
	}
	environment[key] = response.Token
	return syscall.Exec(executable, append([]string{executable}, args...), environmentList(environment))
}
func credentialWith(operation string, input io.Reader, output io.Writer, request func(BrokerRequest) (BrokerResponse, error)) error {
	if operation != "get" {
		return nil
	}
	scanner := bufio.NewScanner(io.LimitReader(input, 8192))
	values := map[string]string{}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return errors.New("invalid Git credential request")
	}
	response, err := request(BrokerRequest{Service: "git", Protocol: values["protocol"], Host: values["host"], Path: values["path"]})
	if err != nil {
		return err
	}
	if response.Token != "" {
		if strings.ContainsAny(response.Token+response.Username, "\r\n") {
			return errors.New("invalid credential response")
		}
		_, err = fmt.Fprintf(output, "username=%s\npassword=%s\n\n", response.Username, response.Token)
	}
	return err
}
func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}
func executableLookup(value string) (string, error) { return exec.LookPath(value) }
