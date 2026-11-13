package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// Client wraps a Transport and implements the MCP protocol handshake and tool calls.
type Client struct {
	transport    Transport
	capabilities ServerCapabilities
	serverInfo   ServerInfo
	idCounter    atomic.Int64
	mu           sync.RWMutex
}

// NewClient creates a new MCP client backed by the given transport.
// Call Initialize before using any other methods.
func NewClient(transport Transport) *Client {
	return &Client{transport: transport}
}

// nextID returns a monotonically increasing request ID.
func (c *Client) nextID() int64 {
	return c.idCounter.Add(1)
}

// Initialize performs the MCP handshake with the server.
// It must be called before ListTools or CallTool.
func (c *Client) Initialize(ctx context.Context) error {
	params := InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    ClientCapabilities{},
		ClientInfo: ClientInfo{
			Name:    "claw-code-go",
			Version: "0.1.0",
		},
	}

	req := Request{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "initialize",
		Params:  params,
	}

	resp, err := c.transport.Send(ctx, req)
	if err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("mcp initialize error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	data, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("mcp initialize: marshal result: %w", err)
	}

	var result InitializeResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("mcp initialize: parse result: %w", err)
	}

	c.mu.Lock()
	c.capabilities = result.Capabilities
	c.serverInfo = result.ServerInfo
	c.mu.Unlock()

	// Acknowledge the handshake.
	return c.transport.Notify(Notification{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	})
}

// ServerInfo returns the server's name and version (populated after Initialize).
func (c *Client) ServerInfo() ServerInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverInfo
}

// ListTools retrieves the list of tools available on the server.
func (c *Client) ListTools(ctx context.Context) ([]MCPTool, error) {
	req := Request{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "tools/list",
	}

	resp, err := c.transport.Send(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("mcp tools/list error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	data, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list: marshal result: %w", err)
	}

	var result struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("mcp tools/list: parse result: %w", err)
	}

	return result.Tools, nil
}

// CallTool invokes a named tool on the server with the given input arguments.
func (c *Client) CallTool(ctx context.Context, name string, input map[string]any) (MCPToolResult, error) {
	req := Request{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  "tools/call",
		Params: map[string]any{
			"name":      name,
			"arguments": input,
		},
	}

	resp, err := c.transport.Send(ctx, req)
	if err != nil {
		return MCPToolResult{}, fmt.Errorf("mcp tools/call %q: %w", name, err)
	}

	if resp.Error != nil {
		return MCPToolResult{}, fmt.Errorf("mcp tools/call %q error %d: %s", name, resp.Error.Code, resp.Error.Message)
	}

	data, err := json.Marshal(resp.Result)
	if err != nil {
		return MCPToolResult{}, fmt.Errorf("mcp tools/call %q: marshal result: %w", name, err)
	}

	var result MCPToolResult
	if err := json.Unmarshal(data, &result); err != nil {
		return MCPToolResult{}, fmt.Errorf("mcp tools/call %q: parse result: %w", name, err)
	}

	return result, nil
}

// call is a generic JSON-RPC helper that sends a request and unmarshals the result.
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	req := Request{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  method,
		Params:  params,
	}

	resp, err := c.transport.Send(ctx, req)
	if err != nil {
		return fmt.Errorf("mcp %s: %w", method, err)
	}

	if resp.Error != nil {
		return fmt.Errorf("mcp %s error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}

	data, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Errorf("mcp %s: marshal result: %w", method, err)
	}

	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("mcp %s: parse result: %w", method, err)
	}

	return nil
}

// Close shuts down the underlying transport.
func (c *Client) Close() error {
	return c.transport.Close()
}
