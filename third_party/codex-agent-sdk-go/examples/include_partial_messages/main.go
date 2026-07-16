package main

import (
	"context"
	"fmt"
	"os"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	fmt.Println("=== Include Partial Messages Example ===")
	fmt.Println("Streaming deltas are labeled by their delta.type so tool output (shell")
	fmt.Println("stdout/stderr, file diffs) is never confused with assistant prose:")
	fmt.Println("  text_delta            -> assistant prose, printed inline")
	fmt.Println("  thinking_delta        -> reasoning, printed with a [thinking] label")
	fmt.Println("  command_output_delta  -> shell output, printed with [command_output id]")
	fmt.Println("  file_change_delta     -> file diff, printed with [file_change id]")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Use a slow-dripping shell command so codex emits per-chunk
	// commandExecution/outputDelta notifications instead of returning the
	// whole aggregated output via item.completed in one shot.
	prompt := "Run this exact shell command: " +
		"`for i in $(seq 1 10); do echo line-$i; sleep 0.1; done`. " +
		"Then say 'done' on a new line."

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text(prompt),
		codexsdk.WithIncludePartialMessages(true),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Query failed: %v\n", err)
			os.Exit(1)
		}

		switch m := msg.(type) {
		case *codexsdk.StreamEvent:
			event := m.Event

			eventType, _ := event["type"].(string)
			if eventType != "content_block_delta" {
				continue
			}

			delta, ok := event["delta"].(map[string]any)
			if !ok {
				continue
			}

			switch delta["type"] {
			case "text_delta":
				text, _ := delta["text"].(string)
				fmt.Print(text)
			case "thinking_delta":
				thinking, _ := delta["thinking"].(string)
				fmt.Printf("[thinking] %s", thinking)
			case "command_output_delta":
				text, _ := delta["text"].(string)
				itemID, _ := delta["item_id"].(string)
				fmt.Printf("[command_output %s] %s", itemID, text)
			case "file_change_delta":
				text, _ := delta["text"].(string)
				itemID, _ := delta["item_id"].(string)
				fmt.Printf("[file_change %s] %s", itemID, text)
			}

		case *codexsdk.AssistantMessage:
			renderAssistantMessage(m)

		case *codexsdk.ResultMessage:
			fmt.Println()

			if m.Usage != nil {
				fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}
}

// renderAssistantMessage prints completed assistant content, labeling tool
// activity separately from prose so the example output stays readable
// regardless of which block kinds the message carries.
func renderAssistantMessage(m *codexsdk.AssistantMessage) {
	for _, block := range m.Content {
		switch b := block.(type) {
		case *codexsdk.TextBlock:
			text := b.Text
			if text == "" {
				continue
			}

			fmt.Println()
			fmt.Println()
			fmt.Print("[assistant] ")
			fmt.Print(text)
			fmt.Println()
		case *codexsdk.ToolUseBlock:
			fmt.Println()
			fmt.Printf("[tool_use %s] %s\n", b.ID, b.Name)
		case *codexsdk.ToolResultBlock:
			fmt.Printf("[tool_result %s] aggregated output:\n", b.ToolUseID)

			for _, inner := range b.Content {
				if tb, ok := inner.(*codexsdk.TextBlock); ok {
					fmt.Print(tb.Text)
				}
			}

			fmt.Println()
		}
	}
}
