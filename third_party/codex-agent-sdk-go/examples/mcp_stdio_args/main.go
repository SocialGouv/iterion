// Package main demonstrates configuring a stdio MCP server with command-line
// arguments (Args).
//
// The placeholder command `/bin/sh -c "cat"` is used so the example can run
// without depending on a real MCP binary being installed — it is enough for
// codex to load the config and complete the app-server handshake. Replace the
// Command and Args with a real stdio MCP launcher (for example
// `npx -y @playwright/mcp@latest`) in practice.
//
// Historical context: before Args were serialized as a TOML inline array,
// running this example failed with "transport closed while waiting for
// response" because codex rejected the config with
// `invalid type: string ..., expected a sequence`.
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := codexsdk.NewClient()

	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
			"stdio-args": &codexsdk.MCPStdioServerConfig{
				Command: "/bin/sh",
				Args:    []string{"-c", "cat"},
			},
		}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client.Start failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("client started: stdio MCP server with Args configured successfully")
}
