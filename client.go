package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

const (
	sourceID  = "herdr-agent-time"
	tokenName = "turn_time"
	pluginID  = "cmalec.agent-time"
)

// Client talks to the running Herdr server over its Unix socket. One
// connection is reused; a failed call closes it so the next call reconnects.
// A Client is not safe for concurrent use.
type Client struct {
	socket string
	conn   net.Conn
	reader *bufio.Reader
	nextID uint64
}

// NewClient returns a client for the Herdr server listening on socket.
func NewClient(socket string) *Client {
	return &Client{socket: socket}
}

// Close closes the cached connection. It is safe to call more than once.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.reader = nil
	return err
}

func (c *Client) dial(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return err
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 64*1024)
	return nil
}

type apiRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// apiCallError is a deterministic error returned by the Herdr server. Retrying
// the same request with the same params cannot succeed.
type apiCallError struct {
	method  string
	code    string
	message string
}

func (e *apiCallError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.method, e.code, e.message)
}

type apiResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *apiError       `json:"error"`
}

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	var lastErr error
	for range 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.conn == nil {
			if err := c.dial(ctx); err != nil {
				lastErr = err
				continue
			}
		}
		if err := c.doCall(ctx, method, params, result); err != nil {
			lastErr = err
			var apiErr *apiCallError
			if errors.As(err, &apiErr) {
				return err
			}
			_ = c.Close()
			continue
		}
		return nil
	}
	return lastErr
}

func (c *Client) doCall(ctx context.Context, method string, params, result any) error {
	c.nextID++
	id := fmt.Sprintf("agent-time-%d", c.nextID)
	payload, err := json.Marshal(apiRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	conn := c.conn
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		close(interrupted)
	})
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return err
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	var response apiResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	if response.Error != nil {
		return &apiCallError{
			method:  method,
			code:    response.Error.Code,
			message: response.Error.Message,
		}
	}
	if result != nil {
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}
	return nil
}

type agentListResult struct {
	Agents []AgentInfo `json:"agents"`
}

type pluginListResult struct {
	Plugins []pluginInfo `json:"plugins"`
}

// pluginInfo is the subset of herdr's installed plugin record the daemon uses
// to detect disable, uninstall, and installation changes.
type pluginInfo struct {
	PluginID   string `json:"plugin_id"`
	Version    string `json:"version"`
	Enabled    bool   `json:"enabled"`
	PluginRoot string `json:"plugin_root"`
}

// ListPlugins returns the installed plugin registry.
func (c *Client) ListPlugins(ctx context.Context) ([]pluginInfo, error) {
	var result pluginListResult
	if err := c.call(ctx, "plugin.list", struct{}{}, &result); err != nil {
		return nil, err
	}
	return result.Plugins, nil
}

// ListAgents returns the current agent snapshot. The call respects ctx and
// applies a three-second deadline when ctx has no earlier deadline.
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	var result agentListResult
	if err := c.call(ctx, "agent.list", struct{}{}, &result); err != nil {
		return nil, err
	}
	return result.Agents, nil
}

// Report writes the turn_time token for a pane. A positive ttl makes the value
// expire unless it is refreshed.
func (c *Client) Report(ctx context.Context, paneID, value string, ttl time.Duration) error {
	params := map[string]any{
		"pane_id": paneID,
		"source":  sourceID,
		"tokens":  map[string]any{tokenName: value},
	}
	if ttl > 0 {
		params["ttl_ms"] = uint64(ttl / time.Millisecond)
	}
	return c.call(ctx, "pane.report_metadata", params, nil)
}

// Clear removes the turn_time token for a pane.
func (c *Client) Clear(ctx context.Context, paneID string) error {
	params := map[string]any{
		"pane_id": paneID,
		"source":  sourceID,
		"tokens":  map[string]any{tokenName: nil},
	}
	return c.call(ctx, "pane.report_metadata", params, nil)
}
