package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/mcp"
)

// list_mcp_resources / read_mcp_resource share an *mcp.Registry;
// mcp_auth additionally needs an *mcp.AuthState.

func ListMcpResourcesTool() api.Tool { return intl.ListMcpResourcesTool() }

func ExecuteListMcpResources(ctx context.Context, input map[string]any, registry *mcp.Registry) (string, error) {
	return intl.ExecuteListMcpResources(input, registry)
}

func ReadMcpResourceTool() api.Tool { return intl.ReadMcpResourceTool() }

func ExecuteReadMcpResource(ctx context.Context, input map[string]any, registry *mcp.Registry) (string, error) {
	return intl.ExecuteReadMcpResource(input, registry)
}

func McpAuthTool() api.Tool { return intl.McpAuthTool() }

func ExecuteMcpAuth(ctx context.Context, input map[string]any, registry *mcp.Registry, authState *mcp.AuthState) (string, error) {
	return intl.ExecuteMcpAuth(input, registry, authState)
}
