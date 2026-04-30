// Package mcp is the public façade over the internal MCP subsystem
// that backs the list_mcp_resources / read_mcp_resource / mcp_auth
// tools and the SSE/HTTP transports.
package mcp

import (
	"context"

	mcppkg "github.com/SocialGouv/claw-code-go/internal/mcp"
)

type Registry = mcppkg.Registry
type AuthState = mcppkg.AuthState
type Transport = mcppkg.Transport
type TransportConfig = mcppkg.TransportConfig
type TransportType = mcppkg.TransportType

// MCP resource introspection surface (used by list_mcp_resources,
// read_mcp_resource, mcp_auth tools).
type (
	ResourceClient    = mcppkg.ResourceClient
	ServerStatus      = mcppkg.ServerStatus
	Provider          = mcppkg.Provider
	McpResourceInfo    = mcppkg.McpResourceInfo
	McpResourceContent = mcppkg.McpResourceContent
)

const (
	TransportStdio        = mcppkg.TransportStdio
	TransportSSE          = mcppkg.TransportSSE
	TransportHTTP         = mcppkg.TransportHTTP
	TransportWebSocket    = mcppkg.TransportWebSocket
	TransportManagedProxy = mcppkg.TransportManagedProxy
	TransportSDK          = mcppkg.TransportSDK
)

func NewRegistry() *Registry  { return mcppkg.NewRegistry() }
func NewAuthState() *AuthState { return mcppkg.NewAuthState() }

// NewRegistryProvider bundles a *Registry + *AuthState into a Provider.
// authState may be nil; in that case all known servers report status
// "connected".
func NewRegistryProvider(r *Registry, a *AuthState) Provider {
	return mcppkg.NewRegistryProvider(r, a)
}

// NewTransport builds a Transport from cfg. SSE/HTTP transports honor
// cfg.AuthFunc when non-nil — the closure is invoked on every request
// to obtain a fresh "Authorization" header.
func NewTransport(cfg TransportConfig) (Transport, error) { return mcppkg.NewTransport(cfg) }

func AddServerFromConfig(ctx context.Context, r *Registry, name string, cfg TransportConfig) error {
	return r.AddServerFromConfig(ctx, name, cfg)
}
