package mcp

import "context"

// ResourceClient abstracts a single MCP server's resource API. *Client
// (the registry's connected client) implements it natively; embedders
// can also implement it backed by their own MCP transport.
type ResourceClient interface {
	ListResources(ctx context.Context) ([]McpResourceInfo, error)
	ReadResource(ctx context.Context, uri string) (McpResourceContent, error)
}

// ServerStatus is the snapshot returned to callers of the mcp_auth tool.
// Status mirrors the McpConnectionStatus enum (e.g. "connected",
// "disconnected", "auth_required").
type ServerStatus struct {
	Name          string
	Status        string
	ServerInfo    string
	ToolCount     int
	ResourceCount int
}

// Provider is the surface the list_mcp_resources / read_mcp_resource /
// mcp_auth tools depend on. *Registry+*AuthState satisfy it via
// NewRegistryProvider; external embedders (e.g. iterion's MCP manager)
// implement it directly to avoid double-connecting.
type Provider interface {
	ServerNames() []string
	GetResourceClient(name string) (ResourceClient, bool)
	ServerStatus(name string) (ServerStatus, bool)
}

// NewRegistryProvider bundles a *Registry + *AuthState into a Provider.
// authState may be nil; in that case all known servers report status
// "connected" (matching the historical behaviour of mcp_auth before
// AuthState was introduced).
func NewRegistryProvider(r *Registry, authState *AuthState) Provider {
	return &registryProvider{r: r, auth: authState}
}

type registryProvider struct {
	r    *Registry
	auth *AuthState
}

func (p *registryProvider) ServerNames() []string {
	if p.r == nil {
		return nil
	}
	return p.r.ServerNames()
}

func (p *registryProvider) GetResourceClient(name string) (ResourceClient, bool) {
	if p.r == nil {
		return nil, false
	}
	c := p.r.GetClient(name)
	if c == nil {
		return nil, false
	}
	return c, true
}

func (p *registryProvider) ServerStatus(name string) (ServerStatus, bool) {
	if p.r == nil {
		return ServerStatus{Name: name, Status: "disconnected"}, false
	}
	found := false
	for _, n := range p.r.ServerNames() {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		return ServerStatus{Name: name, Status: "disconnected"}, false
	}
	var status string
	if p.auth != nil {
		status = p.auth.GetStatus(name).String()
	} else {
		status = "connected"
	}
	return ServerStatus{
		Name:          name,
		Status:        status,
		ServerInfo:    p.r.GetServerInfo(name),
		ToolCount:     len(p.r.ServerTools(name)),
		ResourceCount: p.r.GetResourceCount(name),
	}, true
}
