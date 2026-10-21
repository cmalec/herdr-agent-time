package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"
)

const (
	sourceID  = "herdr-agent-time"
	tokenName = "turn_time"
)

// Client talks to the running Herdr server over its Unix socket. One
// connection is reused; a failed call closes it so the next call reconnects.
type Client struct {
	socket string
	logger *log.Logger
	conn   net.Conn
	reader *bufio.Reader
	nextID uint64
}

func NewClient(socket string, logger *log.Logger) *Client {
	return &Client{socket: socket, logger: logger}
}

func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.reader = nil
	return err
}

func (c *Client) dial() error {
	conn, err := net.DialTimeout("unix", c.socket, 2*time.Second)
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

type apiResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *apiError       `json:"error"`
}

func (c *Client) call(method string, params, result any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if c.conn == nil {
			if err := c.dial(); err != nil {
				lastErr = err
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		if err := c.doCall(method, params, result); err != nil {
			lastErr = err
			_ = c.Close()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return nil
	}
	return lastErr
}

func (c *Client) doCall(method string, params, result any) error {
	c.nextID++
	id := fmt.Sprintf("agent-time-%d", c.nextID)
	payload, err := json.Marshal(apiRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	if err := c.conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if _, err := c.conn.Write(append(payload, '\n')); err != nil {
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
		return fmt.Errorf("%s: %s: %s", method, response.Error.Code, response.Error.Message)
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

func (c *Client) ListAgents() ([]AgentInfo, error) {
	var result agentListResult
	if err := c.call("agent.list", struct{}{}, &result); err != nil {
		return nil, err
	}
	return result.Agents, nil
}

func (c *Client) Report(paneID, value string, ttl time.Duration) error {
	params := map[string]any{
		"pane_id": paneID,
		"source":  sourceID,
		"tokens":  map[string]any{tokenName: value},
	}
	if ttl > 0 {
		params["ttl_ms"] = uint64(ttl / time.Millisecond)
	}
	return c.call("pane.report_metadata", params, nil)
}

func (c *Client) Clear(paneID string) error {
	params := map[string]any{
		"pane_id": paneID,
		"source":  sourceID,
		"tokens":  map[string]any{tokenName: nil},
	}
	return c.call("pane.report_metadata", params, nil)
}
