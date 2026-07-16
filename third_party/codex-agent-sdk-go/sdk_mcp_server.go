package codexsdk

import (
	internalmcp "github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
)

// CreateSdkMcpServer creates an in-process MCP server configuration with SdkMcpTool tools.
//
// The returned config can be used directly in CodexAgentOptions.MCPServers:
//
//	addTool := codexsdk.NewSdkMcpTool("add", "Add two numbers",
//	    codexsdk.SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
//	    func(ctx context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
//	        args, _ := codexsdk.ParseArguments(req)
//	        a, b := args["a"].(float64), args["b"].(float64)
//	        return codexsdk.TextResult(fmt.Sprintf("Result: %v", a+b)), nil
//	    },
//	)
//
//	calculator := codexsdk.CreateSdkMcpServer("calculator", "1.0.0", addTool)
//
//	options := &codexsdk.CodexAgentOptions{
//	    MCPServers: map[string]codexsdk.MCPServerConfig{
//	        "calculator": calculator,
//	    },
//	    AllowedTools: []string{"mcp__calculator__add"},
//	}
func CreateSdkMcpServer(name, version string, tools ...*SdkMcpTool) *MCPSdkServerConfig {
	server := internalmcp.NewSDKServer(name, version)

	for _, tool := range tools {
		mcpTool := internalmcp.NewTool(tool.ToolName, tool.ToolDescription, tool.ToolSchema)
		mcpTool.Annotations = tool.ToolAnnotations
		server.AddTool(mcpTool, tool.ToolHandler)
	}

	return &MCPSdkServerConfig{
		Type:     MCPServerTypeSDK,
		Name:     name,
		Instance: server,
	}
}
