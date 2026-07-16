package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

const systemMessageSubtypeInit = "init"

// displayMessage standardizes message display across examples.
func displayMessage(msg codexsdk.Message) {
	switch m := msg.(type) {
	case *codexsdk.UserMessage:
		var text strings.Builder

		for _, block := range m.Content.Blocks() {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				text.WriteString(textBlock.Text)
			}
		}

		if text.Len() > 0 {
			fmt.Printf("User: %s\n", text.String())
		}

	case *codexsdk.AssistantMessage:
		var text strings.Builder

		for _, block := range m.Content {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				text.WriteString(textBlock.Text)
			}
		}

		if text.Len() > 0 {
			fmt.Printf("Codex: %s\n", text.String())
		}

	case *codexsdk.SystemMessage:
		// Ignore system messages in display

	case *codexsdk.ResultMessage:
		fmt.Println("Result ended")

		if m.Usage != nil {
			fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
		}
	}
}

// extractTools extracts tool names from a system message.
func extractTools(msg *codexsdk.SystemMessage) []string {
	if msg.Subtype != systemMessageSubtypeInit || msg.Data == nil {
		return nil
	}

	tools, ok := msg.Data["tools"].([]any)
	if !ok {
		return nil
	}

	result := make([]string, 0, len(tools))

	for _, tool := range tools {
		if toolStr, ok := tool.(string); ok {
			result = append(result, toolStr)
		}
	}

	return result
}

// toolsArrayExample demonstrates restricting tools to a specific array.
func toolsArrayExample() {
	fmt.Println("=== Tools Array Example ===")
	fmt.Println("Setting requested Tools=['Read', 'Glob', 'Grep']")
	fmt.Println("This run compares requested configuration vs observed runtime tools.")
	fmt.Println()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithTools(codexsdk.ToolsList{"Read", "Glob", "Grep"}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("List your currently available tools briefly.")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		// Special handling for init message to show tools
		if systemMsg, ok := msg.(*codexsdk.SystemMessage); ok && systemMsg.Subtype == systemMessageSubtypeInit {
			tools := extractTools(systemMsg)
			fmt.Printf("Tools from system message: %v\n", tools)
			fmt.Println()
		}

		displayMessage(msg)

		if _, ok := msg.(*codexsdk.ResultMessage); ok {
			break
		}
	}

	fmt.Println()
}

// toolsSingleToolExample demonstrates restricting to a single tool.
func toolsSingleToolExample() {
	fmt.Println("=== Tools Single Tool Example ===")
	fmt.Println("Setting requested Tools=['Read']")
	fmt.Println("This run compares requested configuration vs observed runtime tools.")
	fmt.Println()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithTools(codexsdk.ToolsList{"Read"}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("List your currently available tools briefly.")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		// Special handling for init message to show tools
		if systemMsg, ok := msg.(*codexsdk.SystemMessage); ok && systemMsg.Subtype == systemMessageSubtypeInit {
			tools := extractTools(systemMsg)
			fmt.Printf("Tools from system message: %v\n", tools)
			fmt.Println()
		}

		displayMessage(msg)

		if _, ok := msg.(*codexsdk.ResultMessage); ok {
			break
		}
	}

	fmt.Println()
}

// toolsPresetExample demonstrates using a preset configuration.
func toolsPresetExample() {
	fmt.Println("=== Tools Preset Example ===")
	fmt.Println("Setting requested Tools={type: 'preset', preset: 'claude_code'}")
	fmt.Println("This run compares requested configuration vs observed runtime tools.")
	fmt.Println()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithTools(&codexsdk.ToolsPreset{Type: "preset", Preset: "claude_code"}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("List your currently available tools briefly.")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		// Special handling for init message to show tools
		if systemMsg, ok := msg.(*codexsdk.SystemMessage); ok && systemMsg.Subtype == systemMessageSubtypeInit {
			tools := extractTools(systemMsg)

			if len(tools) > 5 {
				fmt.Printf("Tools from system message (%d tools): %v...\n", len(tools), tools[:5])
			} else {
				fmt.Printf("Tools from system message (%d tools): %v\n", len(tools), tools)
			}

			fmt.Println()
		}

		displayMessage(msg)

		if _, ok := msg.(*codexsdk.ResultMessage); ok {
			break
		}
	}

	fmt.Println()
}

func main() {
	fmt.Println("Tools Option Examples")
	fmt.Println()
	fmt.Println("This example demonstrates requested tool configuration and observed runtime tool reporting.")
	fmt.Println("Note: depending on runtime/backend behavior, requested tool limits may be treated as advisory.")
	fmt.Println()

	examples := map[string]func(){
		"array":  toolsArrayExample,
		"single": toolsSingleToolExample,
		"preset": toolsPresetExample,
	}

	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <example_name>")
		fmt.Println("\nAvailable examples:")
		fmt.Println("  array  - Request a specific tool list (Read, Glob, Grep)")
		fmt.Println("  single - Request a single tool (Read)")
		fmt.Println("  preset - Request claude_code preset for default tools")

		return
	}

	exampleName := os.Args[1]

	if exampleName == "all" {
		for _, name := range []string{"array", "single", "preset"} {
			examples[name]()
			fmt.Println("--------------------------------------------------")
			fmt.Println()
		}
	} else if fn, ok := examples[exampleName]; ok {
		fn()
	} else {
		fmt.Printf("Error: Unknown example '%s'\n", exampleName)
		fmt.Println("\nAvailable examples:")
		fmt.Println("  array  - Request specific tools")
		fmt.Println("  single - Request a single tool")
		fmt.Println("  preset - Request a preset")
		fmt.Println("  all    - Run all examples")

		os.Exit(1)
	}
}
