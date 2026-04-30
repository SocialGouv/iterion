package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/mcp"
)

// list_mcp_resources / read_mcp_resource / mcp_auth share a single
// mcp.Provider. A *Registry+*AuthState pair satisfies the interface via
// mcp.NewRegistryProvider; embedders (e.g. iterion) implement the
// interface directly atop their own MCP infrastructure to avoid
// double-connecting servers.

func ListMcpResourcesTool() api.Tool { return intl.ListMcpResourcesTool() }

func ExecuteListMcpResources(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	return intl.ExecuteListMcpResources(ctx, input, provider)
}

func ReadMcpResourceTool() api.Tool { return intl.ReadMcpResourceTool() }

func ExecuteReadMcpResource(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	return intl.ExecuteReadMcpResource(ctx, input, provider)
}

func McpAuthTool() api.Tool { return intl.McpAuthTool() }

func ExecuteMcpAuth(ctx context.Context, input map[string]any, provider mcp.Provider) (string, error) {
	return intl.ExecuteMcpAuth(ctx, input, provider)
}
