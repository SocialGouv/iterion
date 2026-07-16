//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestMCPTools_Registration tests tool registration with the MCP server.
func TestMCPTools_Registration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	echoTool := codexsdk.NewSdkMcpTool(
		"test_echo",
		"Echoes the input message back",
		codexsdk.SimpleSchema(map[string]string{
			"message": "string",
		}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			msg, _ := args["message"].(string)

			return codexsdk.TextResult(fmt.Sprintf(`{"echo": %q}`, msg)), nil
		},
	)

	server := codexsdk.CreateSdkMcpServer("sdk", "1.0.0", echoTool)
	receivedResult := false

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Say hello"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"sdk": server,
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should complete successfully with registered tool")
}

func TestMCPTools_AllowedTools_PublicNameExecutesSDKTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var toolExecuted bool

	addTool := codexsdk.NewSdkMcpTool(
		"add",
		"Adds two numbers together",
		codexsdk.SimpleSchema(map[string]string{
			"a": "float64",
			"b": "float64",
		}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			toolExecuted = true

			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)

			return codexsdk.TextResult(fmt.Sprintf(`{"result": %.0f}`, a+b)), nil
		},
	)

	server := codexsdk.CreateSdkMcpServer("sdk", "1.0.0", addTool)

	for _, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the add MCP tool on the sdk server to calculate 15 + 27."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSystemPrompt(
			"You have one MCP tool on the sdk server named add. Use it for the calculation and do not answer from memory.",
		),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"sdk": server,
		}),
		codexsdk.WithAllowedTools("mcp__sdk__add"),
		codexsdk.WithDisallowedTools("Bash"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	require.True(t, toolExecuted, "SDK MCP tool should execute when allowed by its public name")
}

func TestSDKTools_WithSDKMCPServers_PreservesDynamicTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var dynamicToolExecuted bool

	revealSecretTool := codexsdk.NewTool(
		"reveal_secret",
		"Returns the secret string for this test",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(_ context.Context, _ map[string]any) (map[string]any, error) {
			dynamicToolExecuted = true

			return map[string]any{
				"secret": "sdk-tool-secret",
			}, nil
		},
	)

	noopTool := codexsdk.NewSdkMcpTool(
		"noop",
		"No-op MCP tool used to force SDK MCP server initialization",
		codexsdk.SimpleSchema(map[string]string{}),
		func(_ context.Context, _ *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			return codexsdk.TextResult(`{"status":"ok"}`), nil
		},
	)

	server := codexsdk.CreateSdkMcpServer("sdk", "1.0.0", noopTool)

	for _, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the reveal_secret tool and report the secret string it returns. Do not answer from memory."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSystemPrompt(
			"You have one dynamic tool named reveal_secret and one MCP tool named noop on the sdk server. "+
				"Use reveal_secret for this task and do not answer without using it.",
		),
		codexsdk.WithSDKTools(revealSecretTool),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"sdk": server,
		}),
		codexsdk.WithAllowedTools("reveal_secret"),
		codexsdk.WithDisallowedTools("Bash", "mcp__sdk__noop"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	require.True(t, dynamicToolExecuted, "SDK dynamic tool should still execute when SDK MCP servers are also configured")
}

func TestSDKTools_WithReservedSDKMCPPrefixWithoutServer_PreservesDynamicToolName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var dynamicToolExecuted bool

	prefixedTool := codexsdk.NewTool(
		"sdkmcp__plain_dynamic_tool",
		"Returns a secret string for this test",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(_ context.Context, _ map[string]any) (map[string]any, error) {
			dynamicToolExecuted = true

			return map[string]any{
				"secret": "prefixed-dynamic-tool-secret",
			}, nil
		},
	)

	for _, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the sdkmcp__plain_dynamic_tool tool and report the secret string it returns. Do not answer from memory."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSystemPrompt(
			"You have one dynamic tool named sdkmcp__plain_dynamic_tool. "+
				"Use it for this task and do not answer without using it.",
		),
		codexsdk.WithSDKTools(prefixedTool),
		codexsdk.WithAllowedTools("sdkmcp__plain_dynamic_tool"),
		codexsdk.WithDisallowedTools("Bash"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	require.True(t, dynamicToolExecuted, "prefixed SDK dynamic tool should execute without being rewritten")
}

// TestSDKTools_Registration tests high-level Tool registration with WithSDKTools.
func TestSDKTools_Registration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	echoTool := codexsdk.NewTool(
		"test_echo",
		"Echoes the input message back",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{
					"type":        "string",
					"description": "Message to echo",
				},
			},
			"required": []string{"message"},
		},
		func(_ context.Context, input map[string]any) (map[string]any, error) {
			msg, _ := input["message"].(string)

			return map[string]any{
				"echo": msg,
			}, nil
		},
	)

	receivedResult := false

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Say hello"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSDKTools(echoTool),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should complete successfully with registered tool")
}

// TestSDKTools_Execution tests high-level Tool called with correct input.
func TestSDKTools_Execution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var toolExecuted bool
	var receivedInput string

	calculatorTool := codexsdk.NewTool(
		"add_numbers",
		"Adds two numbers together and returns the result",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{
					"type":        "number",
					"description": "First number",
				},
				"b": map[string]any{
					"type":        "number",
					"description": "Second number",
				},
			},
			"required": []string{"a", "b"},
		},
		func(_ context.Context, input map[string]any) (map[string]any, error) {
			toolExecuted = true
			a, _ := input["a"].(float64)
			b, _ := input["b"].(float64)
			receivedInput = fmt.Sprintf("a=%g, b=%g", a, b)
			t.Logf("Tool executed with a=%v, b=%v", a, b)

			return map[string]any{
				"result": a + b,
			}, nil
		},
	)

	for _, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the add_numbers tool to add 5 and 3"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSDKTools(calculatorTool),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	require.True(t, toolExecuted, "Tool should have been executed")
	t.Logf("Received input: %s", receivedInput)
}

// TestSDKTools_ReturnValue tests high-level Tool result used by the agent.
func TestSDKTools_ReturnValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var toolExecuted bool
	expectedResult := 42.0

	magicTool := codexsdk.NewTool(
		"get_magic_number",
		"Returns a magic number",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		func(_ context.Context, _ map[string]any) (map[string]any, error) {
			toolExecuted = true

			return map[string]any{
				"number": expectedResult,
			}, nil
		},
	)

	var mentionedNumber bool

	for msg, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the get_magic_number tool and tell me what number it returns"),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithSDKTools(magicTool),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if assistantMsg, ok := msg.(*codexsdk.AssistantMessage); ok {
			for _, block := range assistantMsg.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					t.Logf("Response: %s", textBlock.Text)

					if contains42(textBlock.Text) {
						mentionedNumber = true
					}
				}
			}
		}
	}

	require.True(t, toolExecuted, "Tool should have been executed")
	require.True(t, mentionedNumber, "Agent should mention the returned number (42)")
}

// TestMCPStdio_ConfigLoadsWithArgs is a regression test for the bug where
// MCPStdioServerConfig.Args was serialized as repeated scalar -c overrides,
// each replacing the prior value at the same dotted path. That produced a
// single string for `args` instead of a TOML array, and codex exited during
// config validation before the app-server handshake could complete.
//
// The surfaced error was "transport closed while waiting for response" —
// which pointed at the transport layer, not at config serialization.
//
// This test uses `/bin/sh -c 'cat'` as a placeholder stdio "server": it never
// speaks MCP, but codex only needs the config to load cleanly for client.Start
// to succeed. Inner MCP handshake failures surface via GetMCPStatus, not Start.
func TestMCPStdio_ConfigLoadsWithArgs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"stdio-args": &codexsdk.MCPStdioServerConfig{
				Command: "/bin/sh",
				Args:    []string{"-c", "cat"},
			},
		}),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("client.Start failed with stdio MCP args (likely Args serialization regression): %v", err)
	}
}

func TestMCPStatus_IncludesSDKServerMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	echoTool := codexsdk.NewSdkMcpTool(
		"test_echo",
		"Echoes text back",
		codexsdk.SimpleSchema(map[string]string{"message": "string"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			msg, _ := args["message"].(string)

			return codexsdk.TextResult(msg), nil
		},
	)

	server := codexsdk.CreateSdkMcpServer("sdk", "1.0.0", echoTool)
	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"sdk": server,
		}),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Connect failed: %v", err)
	}

	status, err := client.GetMCPStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, status)

	var sdkStatus *codexsdk.MCPServerStatus

	for i := range status.MCPServers {
		if status.MCPServers[i].Name == "sdk" {
			sdkStatus = &status.MCPServers[i]
			break
		}
	}

	require.NotNil(t, sdkStatus, "sdk MCP server should be listed")
	require.Equal(t, "connected", sdkStatus.Status)
	require.Equal(t, codexsdk.MCPAuthStatusUnsupported, sdkStatus.AuthStatus)
	require.Contains(t, sdkStatus.Tools, "test_echo")
	require.Equal(t, "test_echo", sdkStatus.Tools["test_echo"].Name)
}
