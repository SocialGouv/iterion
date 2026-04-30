package mcp

import (
	"context"

	clawmcp "github.com/SocialGouv/claw-code-go/pkg/api/mcp"
)

// ClawProvider satisfies clawmcp.Provider on top of iterion's *Manager
// (and an optional OAuthBroker for AuthStatus). Constructed via
// (*Manager).ClawProvider; passed into ClawDefaults so claw's
// list_mcp_resources / read_mcp_resource / mcp_auth tools see the same
// MCP servers iterion has connected — no double-connection.
type ClawProvider struct {
	m     *Manager
	oauth *OAuthBroker
}

// ClawProvider returns a Provider backed by this Manager. `oauth` may
// be nil; in that case AuthStatus reports "disconnected"/"auth_required"
// for HTTP/SSE transports and "stdio" for stdio servers (which don't
// authenticate).
func (m *Manager) ClawProvider(oauth *OAuthBroker) *ClawProvider {
	return &ClawProvider{m: m, oauth: oauth}
}

func (p *ClawProvider) ServerNames() []string {
	if p.m == nil {
		return nil
	}
	return p.m.ServerNames()
}

func (p *ClawProvider) GetResourceClient(name string) (clawmcp.ResourceClient, bool) {
	if p.m == nil {
		return nil, false
	}
	if _, ok := p.m.ServerConfig(name); !ok {
		return nil, false
	}
	return &clawResourceClient{m: p.m, server: name}, true
}

func (p *ClawProvider) ServerStatus(name string) (clawmcp.ServerStatus, bool) {
	if p.m == nil {
		return clawmcp.ServerStatus{Name: name, Status: "disconnected"}, false
	}
	cfg, ok := p.m.ServerConfig(name)
	if !ok {
		return clawmcp.ServerStatus{Name: name, Status: "disconnected"}, false
	}
	switch cfg.Transport {
	case TransportStdio:
		return clawmcp.ServerStatus{
			Name:       name,
			Status:     "connected",
			ServerInfo: "stdio (" + cfg.Command + ")",
		}, true
	case TransportHTTP, TransportSSE:
		if p.oauth == nil {
			return clawmcp.ServerStatus{
				Name:       name,
				Status:     "connected",
				ServerInfo: string(cfg.Transport) + " (no oauth broker)",
			}, true
		}
		st := p.oauth.AuthStatus(name)
		st.Name = name
		return st, true
	default:
		return clawmcp.ServerStatus{Name: name, Status: "connected"}, true
	}
}

// clawResourceClient adapts a single (manager, server) pair to claw's
// ResourceClient interface, deferring to Manager.ListResources /
// ReadResource so the underlying connection is shared.
type clawResourceClient struct {
	m      *Manager
	server string
}

func (c *clawResourceClient) ListResources(ctx context.Context) ([]clawmcp.McpResourceInfo, error) {
	res, err := c.m.ListResources(ctx, c.server)
	if err != nil {
		return nil, err
	}
	out := make([]clawmcp.McpResourceInfo, len(res))
	for i, r := range res {
		out[i] = clawmcp.McpResourceInfo{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MimeType:    r.MimeType,
		}
	}
	return out, nil
}

func (c *clawResourceClient) ReadResource(ctx context.Context, uri string) (clawmcp.McpResourceContent, error) {
	res, err := c.m.ReadResource(ctx, c.server, uri)
	if err != nil {
		return clawmcp.McpResourceContent{}, err
	}
	return clawmcp.McpResourceContent{
		URI:         res.URI,
		Name:        res.Name,
		Description: res.Description,
		MimeType:    res.MimeType,
		Content:     res.Text,
	}, nil
}
