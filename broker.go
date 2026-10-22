package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nolith/geas/internal/config"
)

type BrokerRequest struct {
	Service  string `json:"service"`
	Protocol string `json:"protocol,omitempty"`
	Host     string `json:"host,omitempty"`
	Path     string `json:"path,omitempty"`
}
type BrokerResponse struct {
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Error    string `json:"error,omitempty"`
	// Quit tells Git to stop asking other helpers and the user.
	Quit bool `json:"quit,omitempty"`
}
type Broker struct {
	mu          sync.RWMutex
	Config      config.Config
	GitHubToken func(context.Context) (string, error)
	GitLabToken string
	// Why an optional provider was skipped at start; it then stays unavailable.
	GitHubSkipped string
	GitLabSkipped string
	Deadline      time.Time
	Now           func() time.Time
}

// unavailableError answers for a skipped provider, which never falls back to
// your own credentials.
type unavailableError struct{ provider, reason string }

func (e unavailableError) Error() string {
	return e.provider + " is not available in this session: " + e.reason
}

func (b *Broker) answer(ctx context.Context, request BrokerRequest) (BrokerResponse, error) {
	if !b.Now().Before(b.Deadline) {
		return BrokerResponse{}, errors.New("session expired")
	}
	service := request.Service
	if service == "git" {
		if request.Protocol != "https" {
			return BrokerResponse{}, nil
		}
		switch request.Host {
		case "github.com":
			service = "github"
		case "gitlab.com":
			service = "gitlab"
		default:
			return BrokerResponse{}, nil
		}
		path, ok := config.NormalizeGitPath(request.Path)
		if !ok {
			return BrokerResponse{}, nil
		}
		var permitted bool
		if service == "github" {
			permitted = githubAllowed(b.Config.GitHub, path)
		} else {
			permitted = gitlabAllowed(b.Config.GitLab, path)
		}
		if !permitted {
			return BrokerResponse{}, nil
		}
	}
	switch service {
	case "github":
		if b.GitHubSkipped != "" {
			return BrokerResponse{}, unavailableError{"GitHub", b.GitHubSkipped}
		}
		if b.GitHubToken != nil {
			token, err := b.GitHubToken(ctx)
			if err != nil {
				return BrokerResponse{}, err
			}
			return BrokerResponse{Token: token, Username: "x-access-token"}, nil
		}
	case "gitlab":
		b.mu.RLock()
		defer b.mu.RUnlock()
		if b.GitLabSkipped != "" {
			return BrokerResponse{}, unavailableError{"GitLab", b.GitLabSkipped}
		}
		if b.GitLabToken != "" {
			return BrokerResponse{Token: b.GitLabToken, Username: "oauth2"}, nil
		}
	}
	return BrokerResponse{}, errors.New("service not enabled")
}

// githubAllowed reports whether a Git path is an allowed repository or inside
// an allowed owner, by whole leading segment like gitlabAllowed. GitHub names
// are case-insensitive, so the comparison is too.
func githubAllowed(c config.GitHubConfig, path string) bool {
	if slices.ContainsFunc(c.Repositories, func(repo string) bool { return strings.EqualFold(repo, path) }) {
		return true
	}
	return slices.ContainsFunc(c.Owners, func(owner string) bool {
		prefix := owner + "/"
		return len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix)
	})
}

// gitlabAllowed reports whether a Git path is inside the configured namespaces:
// whole leading segments only, so "gitlab-org" never matches "gitlab-org-foo".
// Explicit projects and the earlier format's repositories match exactly.
func gitlabAllowed(c config.GitLabConfig, path string) bool {
	if slices.Contains(c.Repositories, path) || slices.Contains(c.Projects, path) {
		return true
	}
	// A copy: concurrent requests share the configuration.
	namespaces := slices.Clone(c.Groups)
	if c.PersonalProjects {
		namespaces = append(namespaces, c.ExpectedUser)
	}
	return slices.ContainsFunc(namespaces, func(namespace string) bool { return strings.HasPrefix(path, namespace+"/") })
}

type BrokerServer struct {
	listener net.Listener
	broker   *Broker
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func startBroker(parent context.Context, address string, broker *Broker) (*BrokerServer, error) {
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, fmt.Errorf("cannot create session UNIX socket: %w", err)
	}
	if err = os.Chmod(address, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	server := &BrokerServer{listener: listener, broker: broker, ctx: ctx, cancel: cancel}
	server.wg.Add(1)
	go server.serve()
	return server, nil
}
func (s *BrokerServer) serve() {
	defer s.wg.Done()
	capacity := make(chan struct{}, 32)
	for {
		connection, err := s.listener.Accept()
		if err != nil {
			return
		}
		select {
		case capacity <- struct{}{}:
		default:
			connection.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-capacity }()
			defer connection.Close()
			// A request may need a network refresh; slow request input is limited separately.
			connection.SetReadDeadline(time.Now().Add(10 * time.Second))
			line, err := bufio.NewReader(io.LimitReader(connection, 8192)).ReadBytes('\n')
			response := BrokerResponse{Error: "invalid broker request"}
			var request BrokerRequest
			if err == nil && json.Unmarshal(line, &request) == nil {
				connection.SetWriteDeadline(time.Now().Add(60 * time.Second))
				value, err := s.broker.answer(s.ctx, request)
				if err != nil {
					response = BrokerResponse{Error: err.Error(), Quit: errors.As(err, new(unavailableError))}
				} else {
					response = value
				}
			}
			json.NewEncoder(connection).Encode(response)
		}()
	}
}
func (s *BrokerServer) close() { s.cancel(); s.listener.Close(); s.wg.Wait() }
func rpc(request BrokerRequest) (BrokerResponse, error) {
	var response BrokerResponse
	address := os.Getenv("GEAS_SOCKET")
	if address == "" {
		return response, errors.New("no active authentication session")
	}
	connection, err := net.DialTimeout("unix", address, 10*time.Second)
	if err != nil {
		return response, errors.New("cannot connect to session broker")
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(60 * time.Second))
	if err = json.NewEncoder(connection).Encode(request); err != nil {
		return response, errors.New("cannot send broker request")
	}
	if err = json.NewDecoder(io.LimitReader(connection, 8192)).Decode(&response); err != nil {
		return response, errors.New("invalid broker response")
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}
