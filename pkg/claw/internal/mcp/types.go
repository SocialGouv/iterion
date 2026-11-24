package mcp

import "context"

// --- JSON-RPC 2.0 ---

// Request is a JSON-RPC 2.0 request object.
type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response object.
type Response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (no ID, no response expected).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// --- MCP Protocol ---

// InitializeParams are the parameters for the MCP initialize handshake.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      ClientInfo         `json:"clientInfo"`
}

// ClientInfo identifies this MCP client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities declares what the client supports.
type ClientCapabilities struct{}

// InitializeResult is the server's response to initialize.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerCapabilities declares what the server supports.
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
}

// ToolsCapability indicates the server exposes tools.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability indicates the server exposes resources.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// --- Tool Types ---

// MCPTool describes a tool exposed by an MCP server.
type MCPTool struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	InputSchema MCPToolInputSchema `json:"inputSchema"`
}

// MCPToolInputSchema is the JSON schema for a tool's input.
type MCPToolInputSchema struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties,omitempty"`
	Required   []string       `json:"required,omitempty"`
}

// MCPToolCall represents a call to an MCP tool.
type MCPToolCall struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// MCPToolResult is the result returned by an MCP tool call.
type MCPToolResult struct {
	Content []MCPToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// MCPToolContent is a single content item in a tool result.
type MCPToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// --- Transport ---

// Transport is the interface for communicating with an MCP server.
type Transport interface {
	Send(ctx context.Context, req Request) (Response, error)
	Notify(n Notification) error
	Close() error
}
