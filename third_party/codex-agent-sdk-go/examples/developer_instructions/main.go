package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// This example demonstrates WithDeveloperInstructions, which provides
// additional instructions to the agent separately from WithSystemPrompt.
// DeveloperInstructions maps to the "developerInstructions" field in the
// Codex CLI protocol and takes precedence over the systemPrompt mapping.
func main() {
	fmt.Println("Developer Instructions Example")
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
		codexsdk.WithDeveloperInstructions(
			"Always respond in exactly three bullet points. "+
				"Each bullet point must be a single sentence.",
		),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	prompt := "Explain why Go is a good language for backend development."
	fmt.Printf("Prompt: %s\n\n", prompt)

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
}
