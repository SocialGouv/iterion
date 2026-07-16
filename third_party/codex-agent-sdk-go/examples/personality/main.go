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

// This example demonstrates WithPersonality, which controls the agent's
// response style. Valid values are "none", "friendly", and "pragmatic".
func runPersonalityExample(name, personality string) {
	fmt.Printf("=== Personality: %s ===\n", name)

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
		codexsdk.WithPersonality(personality),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	prompt := "What is a goroutine?"

	fmt.Printf("Prompt: %s\n", prompt)
	fmt.Println(strings.Repeat("-", 50))

	if err := client.Query(ctx, codexsdk.Text(prompt)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Error: %v\n", err)

			return
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Printf("Codex: %s\n", textBlock.Text)
				}
			}
		case *codexsdk.ResultMessage:
			if m.Usage != nil {
				fmt.Printf("\nTokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}

	fmt.Println()
}

func main() {
	fmt.Println("Personality Examples")
	fmt.Println("Demonstrating WithPersonality options: none, friendly, pragmatic")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	personalities := []struct {
		name  string
		value string
	}{
		{"None (neutral)", "none"},
		{"Friendly", "friendly"},
		{"Pragmatic", "pragmatic"},
	}

	example := "all"
	if len(os.Args) > 1 {
		example = os.Args[1]
	}

	for _, p := range personalities {
		if example == "all" || example == p.value {
			runPersonalityExample(p.name, p.value)
		}
	}
}
