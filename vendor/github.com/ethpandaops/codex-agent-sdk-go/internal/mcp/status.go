package mcp

// AuthStatus describes the authentication state of an MCP server.
type AuthStatus string

const (
	// AuthStatusUnsupported means the server does not use authentication.
	AuthStatusUnsupported AuthStatus = "unsupported"
	// AuthStatusNotLoggedIn means the server requires login before use.
	AuthStatusNotLoggedIn AuthStatus = "notLoggedIn"
	// AuthStatusBearerToken means the server is authenticated with a bearer token.
	AuthStatusBearerToken AuthStatus = "bearerToken"
	// AuthStatusOAuth means the server is authenticated with OAuth.
	AuthStatusOAuth AuthStatus = "oAuth"
)

// Tool describes an MCP tool exposed by a server.
type Tool struct {
	Name         string         `json:"name"`
	Description  *string        `json:"description,omitempty"`
	Title        *string        `json:"title,omitempty"`
	InputSchema  any            `json:"inputSchema"`
	OutputSchema any            `json:"outputSchema,omitempty"`
	Annotations  map[string]any `json:"annotations,omitempty"`
	Icons        []any          `json:"icons,omitempty"`
	Meta         any            `json:"_meta,omitempty"` //nolint:tagliatelle // MCP protocol uses _meta.
}

// Resource describes a concrete resource available from an MCP server.
type Resource struct {
	URI         string         `json:"uri"`
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	MIMEType    *string        `json:"mimeType,omitempty"`
	Size        *int64         `json:"size,omitempty"`
	Title       *string        `json:"title,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Icons       []any          `json:"icons,omitempty"`
	Meta        any            `json:"_meta,omitempty"` //nolint:tagliatelle // MCP protocol uses _meta.
}

// ResourceTemplate describes a parameterized resource exposed by an MCP server.
type ResourceTemplate struct {
	URITemplate string         `json:"uriTemplate"`
	Name        string         `json:"name"`
	Description *string        `json:"description,omitempty"`
	MIMEType    *string        `json:"mimeType,omitempty"`
	Title       *string        `json:"title,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// ServerStatus represents the connection status of a single MCP server.
type ServerStatus struct {
	Name              string             `json:"name"`
	Status            string             `json:"status"`
	AuthStatus        AuthStatus         `json:"authStatus,omitempty"`
	Tools             map[string]Tool    `json:"tools,omitempty"`
	Resources         []Resource         `json:"resources,omitempty"`
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates,omitempty"`
}

// Status represents the connection status of all configured MCP servers.
type Status struct {
	MCPServers []ServerStatus `json:"mcpServers"`
}
