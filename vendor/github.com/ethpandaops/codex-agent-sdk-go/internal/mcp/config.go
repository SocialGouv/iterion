package mcp

import "context"

// ServerType represents the type of MCP server.
type ServerType string

const (
	// ServerTypeStdio uses stdio for communication.
	ServerTypeStdio ServerType = "stdio"
	// ServerTypeSSE uses Server-Sent Events.
	ServerTypeSSE ServerType = "sse"
	// ServerTypeHTTP uses HTTP for communication.
	ServerTypeHTTP ServerType = "http"
	// ServerTypeSDK uses the SDK interface.
	ServerTypeSDK ServerType = "sdk"
)

// ServerConfig is the interface for MCP server configurations.
type ServerConfig interface {
	GetType() ServerType
}

// Compile-time verification that all MCP server config types implement ServerConfig.
var (
	_ ServerConfig = (*StdioServerConfig)(nil)
	_ ServerConfig = (*SSEServerConfig)(nil)
	_ ServerConfig = (*HTTPServerConfig)(nil)
	_ ServerConfig = (*SdkServerConfig)(nil)
)

// StdioServerConfig configures a stdio-based MCP server.
type StdioServerConfig struct {
	Type    *ServerType       `json:"type,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// GetType implements ServerConfig.
func (m *StdioServerConfig) GetType() ServerType {
	if m.Type != nil {
		return *m.Type
	}

	return ServerTypeStdio
}

// SSEServerConfig configures a Server-Sent Events MCP server.
type SSEServerConfig struct {
	Type    ServerType        `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// GetType implements ServerConfig.
func (m *SSEServerConfig) GetType() ServerType { return m.Type }

// HTTPServerConfig configures an HTTP-based MCP server.
type HTTPServerConfig struct {
	Type    ServerType        `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// GetType implements ServerConfig.
func (m *HTTPServerConfig) GetType() ServerType { return m.Type }

// ServerInstance is the interface that SDK MCP servers must implement.
type ServerInstance interface {
	// Name returns the server name.
	Name() string
	// Version returns the server version.
	Version() string
	// ListTools returns metadata for all registered tools.
	ListTools() []map[string]any
	// CallTool executes a tool by name with the given input.
	CallTool(ctx context.Context, name string, input map[string]any) (map[string]any, error)
}

// SdkServerConfig configures an SDK-provided MCP server.
type SdkServerConfig struct {
	Type     ServerType `json:"type"`
	Name     string     `json:"name"`
	Instance any        `json:"-"`
}

// GetType implements ServerConfig.
func (m *SdkServerConfig) GetType() ServerType { return m.Type }
