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
}
type Broker struct {
	mu          sync.RWMutex
	Config      Config
	GitHubToken func(context.Context) (string, error)
	GitLabToken string
	Deadline    time.Time
	Now         func() time.Time
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
		path, ok := normalizeGitPath(request.Path)
		if !ok {
			return BrokerResponse{}, nil
		}
		var permitted bool
		if service == "github" {
			permitted = slices.ContainsFunc(b.Config.GitHub.Repositories, func(repo string) bool { return strings.EqualFold(repo, path) })
		} else {
			permitted = gitlabAllowed(b.Config.GitLab, path)
		}
		if !permitted {
			return BrokerResponse{}, nil
		}
	}
	switch service {
	case "github":
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
		if b.GitLabToken != "" {
			return BrokerResponse{Token: b.GitLabToken, Username: "oauth2"}, nil
		}
	}
	return BrokerResponse{}, errors.New("service not enabled")
}

// gitlabAllowed reports whether a Git path is inside the configured namespaces:
// whole leading segments only, so "gitlab-org" never matches "gitlab-org-foo".
// Explicit projects and the earlier format's repositories match exactly.
func gitlabAllowed(c GitLabConfig, path string) bool {
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
					response = BrokerResponse{Error: err.Error()}
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
	address := os.Getenv("AI_AUTH_SOCKET")
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
