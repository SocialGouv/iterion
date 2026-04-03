package claude

// MCPServerConfig is a sealed interface for MCP server configurations.
// Concrete types: [MCPStdioServer], [MCPSSEServer], [MCPHTTPServer].
type MCPServerConfig interface {
	mcpType() string
	sealedMcp()
}

// MCPStdioServer runs an MCP server as a subprocess with stdin/stdout communication.
type MCPStdioServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
}

func (*MCPStdioServer) mcpType() string { return "stdio" }
func (*MCPStdioServer) sealedMcp()      {}

// MCPSSEServer connects to an MCP server over Server-Sent Events.
type MCPSSEServer struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (*MCPSSEServer) mcpType() string { return "sse" }
func (*MCPSSEServer) sealedMcp()      {}

// MCPHTTPServer connects to an MCP server over HTTP.
type MCPHTTPServer struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (*MCPHTTPServer) mcpType() string { return "http" }
func (*MCPHTTPServer) sealedMcp()      {}

// mcpServerJSON converts an MCPServerConfig to a JSON-serializable map.
func mcpServerJSON(srv MCPServerConfig) map[string]any {
	m := map[string]any{"type": srv.mcpType()}
	switch s := srv.(type) {
	case *MCPStdioServer:
		m["command"] = s.Command
		if len(s.Args) > 0 {
			m["args"] = s.Args
		}
		if len(s.Env) > 0 {
			m["env"] = s.Env
		}
		if s.Cwd != "" {
			m["cwd"] = s.Cwd
		}
	case *MCPSSEServer:
		m["url"] = s.URL
		if len(s.Headers) > 0 {
			m["headers"] = s.Headers
		}
	case *MCPHTTPServer:
		m["url"] = s.URL
		if len(s.Headers) > 0 {
			m["headers"] = s.Headers
		}
	}
	return m
}
