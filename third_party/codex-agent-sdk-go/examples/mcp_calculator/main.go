// Package main demonstrates how to create calculator tools using MCP servers.
//
// This example shows how to create an in-process MCP server with calculator
// tools using the Codex SDK with the official MCP SDK types.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// createCalculatorTools creates the 6 calculator tools: add, subtract, multiply, divide, sqrt, power.
func createCalculatorTools() []*codexsdk.SdkMcpTool {
	// Annotations shared by all calculator tools: read-only and idempotent.
	calcAnnotations := &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
	}

	// Add tool - using simple type schema
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
			result := a + b

			return codexsdk.TextResult(fmt.Sprintf("%v + %v = %v", a, b, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	// Subtract tool
	subtractTool := codexsdk.NewSdkMcpTool(
		"subtract",
		"Subtract one number from another",
		codexsdk.SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			result := a - b

			return codexsdk.TextResult(fmt.Sprintf("%v - %v = %v", a, b, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	// Multiply tool
	multiplyTool := codexsdk.NewSdkMcpTool(
		"multiply",
		"Multiply two numbers",
		codexsdk.SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			result := a * b

			return codexsdk.TextResult(fmt.Sprintf("%v × %v = %v", a, b, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	// Divide tool
	divideTool := codexsdk.NewSdkMcpTool(
		"divide",
		"Divide one number by another",
		codexsdk.SimpleSchema(map[string]string{"a": "float64", "b": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)

			if b == 0 {
				return codexsdk.ErrorResult("Error: Division by zero is not allowed"), nil
			}

			result := a / b

			return codexsdk.TextResult(fmt.Sprintf("%v ÷ %v = %v", a, b, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	// Square root tool
	sqrtTool := codexsdk.NewSdkMcpTool(
		"sqrt",
		"Calculate square root",
		codexsdk.SimpleSchema(map[string]string{"n": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			n, _ := args["n"].(float64)

			if n < 0 {
				return codexsdk.ErrorResult(
					fmt.Sprintf("Error: Cannot calculate square root of negative number %v", n),
				), nil
			}

			result := math.Sqrt(n)

			return codexsdk.TextResult(fmt.Sprintf("√%v = %v", n, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	// Power tool
	powerTool := codexsdk.NewSdkMcpTool(
		"power",
		"Raise a number to a power",
		codexsdk.SimpleSchema(map[string]string{"base": "float64", "exponent": "float64"}),
		func(_ context.Context, req *codexsdk.CallToolRequest) (*codexsdk.CallToolResult, error) {
			args, err := codexsdk.ParseArguments(req)
			if err != nil {
				return codexsdk.ErrorResult(err.Error()), nil
			}

			base, _ := args["base"].(float64)
			exponent, _ := args["exponent"].(float64)
			result := math.Pow(base, exponent)

			return codexsdk.TextResult(fmt.Sprintf("%v^%v = %v", base, exponent, result)), nil
		},
		codexsdk.WithAnnotations(calcAnnotations),
	)

	return []*codexsdk.SdkMcpTool{addTool, subtractTool, multiplyTool, divideTool, sqrtTool, powerTool}
}

// displayMessage displays message content in a clean format.
// Returns true when a tool-use block is observed.
func displayMessage(msg codexsdk.Message) bool {
	switch m := msg.(type) {
	case *codexsdk.UserMessage:
		for _, block := range m.Content.Blocks() {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				fmt.Printf("User: %s\n", textBlock.Text)
			}
		}

		return false

	case *codexsdk.AssistantMessage:
		usedTool := false

		for _, block := range m.Content {
			switch b := block.(type) {
			case *codexsdk.TextBlock:
				fmt.Printf("Codex: %s\n", b.Text)
			case *codexsdk.ToolUseBlock:
				if shouldIgnoreToolUse(b) {
					continue
				}

				usedTool = true

				fmt.Printf("Using tool: %s\n", b.Name)

				if len(b.Input) > 0 {
					fmt.Printf("  Input: ")

					first := true

					for k, v := range b.Input {
						if !first {
							fmt.Print(", ")
						}

						fmt.Printf("%s=%v", k, v)

						first = false
					}

					fmt.Println()
				}
			}
		}

		return usedTool

	case *codexsdk.SystemMessage:
		// Ignore system messages
		return false

	case *codexsdk.ResultMessage:
		fmt.Println("Result ended")

		if m.Usage != nil {
			fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
		}

		return false
	}

	return false
}

func shouldIgnoreToolUse(block *codexsdk.ToolUseBlock) bool {
	if block == nil || block.Name != "Bash" {
		return false
	}

	command, _ := block.Input["command"].(string)
	command = strings.TrimSpace(command)

	return command == "true" || strings.HasSuffix(command, " -lc true") || strings.HasSuffix(command, " -lc 'true'")
}

func main() {
	// Set up logging
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Create calculator tools
	tools := createCalculatorTools()

	// Create the calculator MCP server config
	// The name "calc" is used as the server key for tool naming (mcp__calc__<toolName>)
	calculator := codexsdk.CreateSdkMcpServer("calc", "2.0.0", tools...)

	fmt.Println("MCP Calculator Example")
	fmt.Println("This example registers a local MCP calculator server and asks arithmetic questions.")
	fmt.Println("Codex should use the calc MCP arithmetic tools instead of shell commands.")

	// Example prompts to demonstrate calculator usage
	prompts := []string{
		"Calculate 15 + 27 using the calculator tools.",
		"What is 100 divided by 7? Use the calculator tools.",
		"Calculate the square root of 144 using the calculator tools.",
		"What is 2 raised to the power of 8? Use the calculator tools.",
		"Calculate (12 + 8) * 3 - 10 using the calculator tools.",
	}

	for _, prompt := range prompts {
		fmt.Printf("\n%s\n", "==================================================")
		fmt.Printf("Prompt: %s\n", prompt)
		fmt.Printf("%s\n", "==================================================")

		client := codexsdk.NewClient()

		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)

		if err := client.Start(ctx,
			codexsdk.WithLogger(logger),
			codexsdk.WithPermissionMode("default"),
			codexsdk.WithMCPServers(map[string]codexsdk.MCPServerConfig{
				"calc": calculator,
			}),
			codexsdk.WithAllowedTools(
				"mcp__calc__add",
				"mcp__calc__subtract",
				"mcp__calc__multiply",
				"mcp__calc__divide",
				"mcp__calc__sqrt",
				"mcp__calc__power",
			),
			codexsdk.WithDisallowedTools(
				"Bash",
				"codex:list_mcp_resources",
				"codex:list_mcp_resource_templates",
				"calc:list_mcp_resources",
				"calc:list_mcp_resource_templates",
			),
			codexsdk.WithSystemPrompt(
				"You have calculator MCP tools on the calc server: add, subtract, multiply, divide, sqrt, and power. "+
					"Use those tools for arithmetic in this example. Do not use Bash or MCP discovery tools.",
			),
		); err != nil {
			logger.Error("Failed to connect", "error", err)
			cancel()
			client.Close()
			os.Exit(1)
		}

		if status, err := client.GetMCPStatus(ctx); err == nil {
			for _, server := range status.MCPServers {
				fmt.Printf("MCP status: %s = %s\n", server.Name, server.Status)
			}
		}

		if err := client.Query(ctx, codexsdk.Text(prompt)); err != nil {
			logger.Error("Failed to send query", "error", err)
			cancel()
			client.Close()
			os.Exit(1)
		}

		usedTool := false

		for msg, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				logger.Error("Failed to receive response", "error", err)
				cancel()
				client.Close()
				os.Exit(1)
			}

			if displayMessage(msg) {
				usedTool = true
			}
		}

		if !usedTool {
			fmt.Println("No explicit MCP tool call observed for this prompt.")
		}

		cancel()
		client.Close()
	}
}
