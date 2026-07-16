// Package main demonstrates querying MCP server connection status.
//
// This example creates an in-process MCP server, starts a client with it
// configured, and queries the live connection status of all MCP servers.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create a simple calculator MCP server with one tool.
	addTool := codexsdk.NewSdkMcpTool(
		"add",
		"Add two numbers",
		codexsdk.SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)

			return codexsdk.TextResult(fmt.Sprintf("%v + %v = %v", a, b, a+b)), nil
		},
	)

	calculator := codexsdk.CreateSdkMcpServer("calc", "1.0.0", addTool)

	// Start client with the MCP server configured.
	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"calc": calculator,
		}),
	); err != nil {
		logger.Error("Failed to start client", "error", err)
		os.Exit(1)
	}

	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	// Query MCP server status.
	status, err := client.GetMCPStatus(ctx)
	if err != nil {
		logger.Error("Failed to get MCP status", "error", err)
		os.Exit(1)
	}

	fmt.Println("MCP Server Status:")

	for _, server := range status.MCPServers {
		fmt.Printf("  %s: %s", server.Name, server.Status)

		if server.AuthStatus != "" {
			fmt.Printf(" (auth=%s)", server.AuthStatus)
		}

		fmt.Println()

		if len(server.Tools) > 0 {
			fmt.Println("    tools:")

			for name := range server.Tools {
				fmt.Printf("      - %s\n", name)
			}
		}

		if len(server.Resources) > 0 || len(server.ResourceTemplates) > 0 {
			fmt.Printf(
				"    resources=%d resourceTemplates=%d\n",
				len(server.Resources),
				len(server.ResourceTemplates),
			)
		}
	}
}
