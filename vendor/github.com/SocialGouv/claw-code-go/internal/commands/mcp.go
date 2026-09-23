package commands

import (
	"context"
	"fmt"
	"strings"
)

// mcpLoopInterface is the subset of ConversationLoop used by the /mcp command.
type mcpLoopInterface interface {
	MCPConnect(ctx context.Context, name string) error
	MCPDisconnect(name string) error
	MCPList() string
}

// RegisterMCPCommand adds the /mcp command to the registry.
func RegisterMCPCommand(r *Registry) {
	r.Register(Command{
		Name:        "mcp",
		Description: "Manage MCP server connections. Usage: /mcp list | /mcp connect <name> | /mcp disconnect <name>",
		Handler: func(args string, loop interface{}) error {
			ml, ok := loop.(mcpLoopInterface)
			if !ok {
				fmt.Println("MCP commands not available in this context.")
				return nil
			}

			parts := strings.Fields(args)
			if len(parts) == 0 {
				fmt.Println("Usage: /mcp list | /mcp connect <name> | /mcp disconnect <name>")
				return nil
			}

			switch parts[0] {
			case "list":
				fmt.Print(ml.MCPList())

			case "connect":
				if len(parts) < 2 {
					fmt.Println("Usage: /mcp connect <name>")
					return nil
				}
				name := parts[1]
				if err := ml.MCPConnect(context.Background(), name); err != nil {
					fmt.Printf("Error connecting to MCP server %q: %v\n", name, err)
					return nil
				}
				fmt.Printf("Connected to MCP server %q.\n", name)

			case "disconnect":
				if len(parts) < 2 {
					fmt.Println("Usage: /mcp disconnect <name>")
					return nil
				}
				name := parts[1]
				if err := ml.MCPDisconnect(name); err != nil {
					fmt.Printf("Error disconnecting from MCP server %q: %v\n", name, err)
					return nil
				}
				fmt.Printf("Disconnected from MCP server %q.\n", name)

			default:
				fmt.Printf("Unknown mcp subcommand: %q. Use list, connect, or disconnect.\n", parts[0])
			}

			return nil
		},
	})
}
