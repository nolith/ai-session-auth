package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/nolith/geas/internal/config"
)

type brokerRequest struct {
	Service string `json:"service"`
}
type brokerResponse struct {
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Error    string `json:"error,omitempty"`
	// Quit tells Git to stop asking other helpers and the user.
	Quit bool `json:"quit,omitempty"`
}
type broker struct {
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

func (b *broker) answer(ctx context.Context, request brokerRequest) (brokerResponse, error) {
	if !b.Now().Before(b.Deadline) {
		return brokerResponse{}, errors.New("session expired")
	}
	service := request.Service
	switch service {
	case "github":
		if b.GitHubSkipped != "" {
			return brokerResponse{}, unavailableError{"GitHub", b.GitHubSkipped}
		}
		if b.GitHubToken != nil {
			token, err := b.GitHubToken(ctx)
			if err != nil {
				return brokerResponse{}, err
			}
			return brokerResponse{Token: token, Username: "x-access-token"}, nil
		}
	case "gitlab":
		b.mu.RLock()
		defer b.mu.RUnlock()
		if b.GitLabSkipped != "" {
			return brokerResponse{}, unavailableError{"GitLab", b.GitLabSkipped}
		}
		if b.GitLabToken != "" {
			return brokerResponse{Token: b.GitLabToken, Username: "oauth2"}, nil
		}
	}
	return brokerResponse{}, errors.New("service not enabled")
}

type brokerServer struct {
	listener net.Listener
	broker   *broker
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func startBroker(parent context.Context, address string, broker *broker) (*brokerServer, error) {
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, fmt.Errorf("cannot create session UNIX socket: %w", err)
	}
	if err = os.Chmod(address, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	server := &brokerServer{listener: listener, broker: broker, ctx: ctx, cancel: cancel}
	server.wg.Add(1)
	go server.serve()
	return server, nil
}
func (s *brokerServer) serve() {
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
			response := brokerResponse{Error: "invalid broker request"}
			var request brokerRequest
			if err == nil && json.Unmarshal(line, &request) == nil {
				connection.SetWriteDeadline(time.Now().Add(60 * time.Second))
				value, err := s.broker.answer(s.ctx, request)
				if err != nil {
					response = brokerResponse{Error: err.Error(), Quit: errors.As(err, new(unavailableError))}
				} else {
					response = value
				}
			}
			json.NewEncoder(connection).Encode(response)
		}()
	}
}
func (s *brokerServer) close() { s.cancel(); s.listener.Close(); s.wg.Wait() }
func rpc(request brokerRequest) (brokerResponse, error) {
	var response brokerResponse
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
