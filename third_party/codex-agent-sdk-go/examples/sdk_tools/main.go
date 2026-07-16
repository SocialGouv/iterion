// Package main demonstrates the high-level Tool API with WithSDKTools.
//
// This example shows how to create tools using NewTool and register them
// with WithSDKTools. Tools are sent as dynamicTools in the thread/start
// payload and called back via item/tool/call RPC using plain tool names.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	// Create tools using the high-level API.
	// No MCP protocol types needed — just name, description, schema, and handler.
	addTool := codexsdk.NewTool(
		"add",
		"Add two numbers together",
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
			a, _ := input["a"].(float64)
			b, _ := input["b"].(float64)

			return map[string]any{"result": a + b}, nil
		},
	)

	multiplyTool := codexsdk.NewTool(
		"multiply",
		"Multiply two numbers together",
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
			a, _ := input["a"].(float64)
			b, _ := input["b"].(float64)

			return map[string]any{"result": a * b}, nil
		},
	)

	// WithSDKTools registers tools and auto-allows them — no manual
	// MCP server setup or AllowedTools list needed.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Println("SDK Tools Example")
	fmt.Println("Asking the agent to use add and multiply tools...")
	fmt.Println()

	for msg, err := range codexsdk.Query(ctx,
		codexsdk.Text("Use the add tool to add 12 and 30, then use the multiply tool to multiply the result by 2. Report both results."),
		codexsdk.WithSDKTools(addTool, multiplyTool),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			log.Fatal(err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				switch b := block.(type) {
				case *codexsdk.TextBlock:
					fmt.Println(b.Text)
				case *codexsdk.ToolUseBlock:
					fmt.Printf("[tool: %s]\n", b.Name)
				}
			}

		case *codexsdk.ResultMessage:
			fmt.Println()

			if m.Usage != nil {
				fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}

			os.Exit(0)
		}
	}
}
