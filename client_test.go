package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeHerdr struct {
	listener   net.Listener
	path       string
	handler    func(apiRequest) apiResponse
	closeAfter int

	mu       sync.Mutex
	requests []apiRequest
}

func newFakeHerdr(t *testing.T, closeAfter int, handler func(apiRequest) apiResponse) *fakeHerdr {
	t.Helper()
	path := filepath.Join(os.TempDir(), fmt.Sprintf("herdr-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)

	server := &fakeHerdr{listener: listener, path: path, handler: handler, closeAfter: closeAfter}
	go server.accept()
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	return server
}

func (s *fakeHerdr) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeHerdr) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var request apiRequest
		if err := json.Unmarshal(line, &request); err != nil {
			return
		}

		s.mu.Lock()
		index := len(s.requests)
		s.requests = append(s.requests, request)
		s.mu.Unlock()

		response := s.handler(request)
		payload, err := json.Marshal(response)
		if err != nil {
			return
		}
		if _, err := conn.Write(append(payload, '\n')); err != nil {
			return
		}
		if s.closeAfter > 0 && index+1 >= s.closeAfter {
			return
		}
	}
}

func (s *fakeHerdr) requestLog() []apiRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]apiRequest(nil), s.requests...)
}

func TestClientOperations(t *testing.T) {
	tests := []struct {
		name         string
		result       any
		apiErr       *apiError
		call         func(context.Context, *Client) (any, error)
		want         any
		wantErr      string
		wantRequests int
		check        func(*testing.T, apiRequest)
	}{
		{
			name: "lists agents",
			result: agentListResult{Agents: []AgentInfo{
				{PaneID: "w1:p1", Agent: "omp", AgentStatus: statusWorking, StateChangeSeq: 7},
			}},
			call: func(ctx context.Context, client *Client) (any, error) {
				return client.ListAgents(ctx)
			},
			want:         []AgentInfo{{PaneID: "w1:p1", Agent: "omp", AgentStatus: statusWorking, StateChangeSeq: 7}},
			wantRequests: 1,
			check: func(t *testing.T, request apiRequest) {
				assert.Equal(t, "agent.list", request.Method)
			},
		},
		{
			name: "reports a live value with a TTL",
			call: func(ctx context.Context, client *Client) (any, error) {
				return nil, client.Report(ctx, "w1:p1", "1:05", 3*time.Second)
			},
			wantRequests: 1,
			check: func(t *testing.T, request apiRequest) {
				assert.Equal(t, "pane.report_metadata", request.Method)
				params := request.Params.(map[string]any)
				assert.Equal(t, "w1:p1", params["pane_id"])
				assert.Equal(t, sourceID, params["source"])
				assert.Equal(t, float64(3000), params["ttl_ms"])
				tokens := params["tokens"].(map[string]any)
				assert.Equal(t, "1:05", tokens[tokenName])
			},
		},
		{
			name: "reports a completed value without a TTL",
			call: func(ctx context.Context, client *Client) (any, error) {
				return nil, client.Report(ctx, "w1:p1", "1:42", 0)
			},
			wantRequests: 1,
			check: func(t *testing.T, request apiRequest) {
				params := request.Params.(map[string]any)
				assert.NotContains(t, params, "ttl_ms")
				tokens := params["tokens"].(map[string]any)
				assert.Equal(t, "1:42", tokens[tokenName])
			},
		},
		{
			name: "clears a token",
			call: func(ctx context.Context, client *Client) (any, error) {
				return nil, client.Clear(ctx, "w1:p1")
			},
			wantRequests: 1,
			check: func(t *testing.T, request apiRequest) {
				assert.Equal(t, "pane.report_metadata", request.Method)
				params := request.Params.(map[string]any)
				tokens := params["tokens"].(map[string]any)
				assert.Nil(t, tokens[tokenName])
			},
		},
		{
			name: "lists plugins",
			result: pluginListResult{Plugins: []pluginInfo{
				{PluginID: pluginID, Version: "0.2.0", Enabled: true, PluginRoot: "/plugin"},
			}},
			call: func(ctx context.Context, client *Client) (any, error) {
				return client.ListPlugins(ctx)
			},
			want:         []pluginInfo{{PluginID: pluginID, Version: "0.2.0", Enabled: true, PluginRoot: "/plugin"}},
			wantRequests: 1,
			check: func(t *testing.T, request apiRequest) {
				assert.Equal(t, "plugin.list", request.Method)
			},
		},
		{
			name:   "returns an API error without retrying",
			apiErr: &apiError{Code: "bad_request", Message: "missing pane"},
			call: func(ctx context.Context, client *Client) (any, error) {
				return client.ListAgents(ctx)
			},
			wantErr:      "agent.list: bad_request: missing pane",
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := json.Marshal(tt.result)
			require.NoError(t, err)
			server := newFakeHerdr(t, 0, func(request apiRequest) apiResponse {
				if tt.apiErr != nil {
					return apiResponse{ID: request.ID, Error: tt.apiErr}
				}
				return apiResponse{ID: request.ID, Result: result}
			})
			client := NewClient(server.path)
			defer func() { _ = client.Close() }()

			got, err := tt.call(context.Background(), client)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}

			requests := server.requestLog()
			require.Len(t, requests, tt.wantRequests)
			assert.Equal(t, "agent-time-1", requests[0].ID)
			if tt.check != nil {
				tt.check(t, requests[0])
			}
		})
	}
}

func TestClientReconnects(t *testing.T) {
	tests := []struct {
		name       string
		closeAfter int
	}{
		{name: "reuses a healthy connection"},
		{name: "reconnects after the server closes", closeAfter: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := json.Marshal(agentListResult{Agents: []AgentInfo{
				{PaneID: "w1:p1", Agent: "omp", AgentStatus: statusWorking, StateChangeSeq: 1},
			}})
			require.NoError(t, err)
			server := newFakeHerdr(t, tt.closeAfter, func(request apiRequest) apiResponse {
				return apiResponse{ID: request.ID, Result: result}
			})
			client := NewClient(server.path)
			defer func() { _ = client.Close() }()

			for range 2 {
				agents, err := client.ListAgents(context.Background())
				require.NoError(t, err)
				require.Len(t, agents, 1)
				assert.Equal(t, "w1:p1", agents[0].PaneID)
			}
			assert.Len(t, server.requestLog(), 2)
		})
	}
}

func TestClientCanceledContext(t *testing.T) {
	tests := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		{
			name: "canceled context",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		{
			name: "expired deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.context()
			defer cancel()
			client := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
			_, err := client.ListAgents(ctx)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestClientCancelInterruptsRead(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := newFakeHerdr(t, 0, func(request apiRequest) apiResponse {
		close(started)
		<-release
		return apiResponse{ID: request.ID}
	})
	t.Cleanup(func() { close(release) })

	client := NewClient(server.path)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	start := time.Now()
	_, err := client.ListAgents(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 2*time.Second, "cancel must interrupt the blocking read")
}
