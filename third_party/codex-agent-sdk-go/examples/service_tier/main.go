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

// This example demonstrates WithServiceTier, which controls the API service
// tier for requests. Valid values are "fast" (optimized for speed) and "flex"
// (optimized for cost/throughput).
//
// Note: "flex" tier availability is model-dependent and may not yet be
// supported by all API endpoints. This example demonstrates both tiers
// and shows how unsupported tiers surface errors through the SDK.
func runServiceTierExample(tier string) {
	fmt.Printf("=== Service Tier: %s ===\n", tier)

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
		codexsdk.WithServiceTier(tier),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	prompt := "What is 2+2? Reply with just the number."
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
			if m.Error != nil {
				fmt.Printf("API Error (expected for unsupported tiers):\n")
			}

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
	fmt.Println("Service Tier Examples")
	fmt.Println("Demonstrating WithServiceTier options: fast, flex")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	example := "all"
	if len(os.Args) > 1 {
		example = os.Args[1]
	}

	switch example {
	case "all":
		runServiceTierExample("fast")
		fmt.Println(strings.Repeat("-", 60))
		fmt.Println()
		runServiceTierExample("flex")
	case "fast", "flex":
		runServiceTierExample(example)
	default:
		fmt.Printf("Unknown tier %q. Valid values: fast, flex, all\n", example)
		os.Exit(1)
	}
}
