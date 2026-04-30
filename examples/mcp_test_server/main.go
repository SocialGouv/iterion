// Tiny MCP stdio server used by the live tests.
//
// Exposes two trivial tools and one trivial resource so the live tests
// can prove iterion's MCP integration end-to-end (server discovery,
// tools/list, tools/call, resources/list, resources/read, content
// extraction back into the agent's response):
//
//   - greet(name string) -> "Hello, {name}!"
//   - reverse(text string) -> text reversed character-by-character
//   - resource iterion://test/golden.txt -> a small fixture string
//
// Build:  go build -o /tmp/mcp_test_server ./examples/mcp_test_server
// Run:    invoked over stdio by iterion's MCP manager.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type greetIn struct {
	Name string `json:"name"`
}

type greetOut struct {
	Greeting string `json:"greeting"`
}

type reverseIn struct {
	Text string `json:"text"`
}

type reverseOut struct {
	Reversed string `json:"reversed"`
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "iterion-mcp-test-server",
		Version: "0.1.0",
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "greet",
		Description: "Compose a greeting for the given name and return it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args greetIn) (*mcp.CallToolResult, greetOut, error) {
		if args.Name == "" {
			return nil, greetOut{}, errors.New("name is required")
		}
		return nil, greetOut{Greeting: fmt.Sprintf("Hello, %s!", args.Name)}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "reverse",
		Description: "Reverse the given text character-by-character and return the result.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args reverseIn) (*mcp.CallToolResult, reverseOut, error) {
		runes := []rune(args.Text)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return nil, reverseOut{Reversed: string(runes)}, nil
	})

	const goldenURI = "iterion://test/golden.txt"
	const goldenBody = "iterion-mcp-test-resource\nline-2\n"
	srv.AddResource(&mcp.Resource{
		URI:         goldenURI,
		Name:        "golden",
		Description: "A small fixture resource the live tests assert is discoverable + readable end-to-end.",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      goldenURI,
				MIMEType: "text/plain",
				Text:     goldenBody,
			}},
		}, nil
	})

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "mcp_test_server: %v\n", err)
		os.Exit(1)
	}
}
