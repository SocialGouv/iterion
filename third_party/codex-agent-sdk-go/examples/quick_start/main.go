package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// displayMessage standardizes message display across examples.
func displayMessage(msg codexsdk.Message) {
	switch m := msg.(type) {
	case *codexsdk.UserMessage:
		for _, block := range m.Content.Blocks() {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				fmt.Printf("User: %s\n", textBlock.Text)
			}
		}

	case *codexsdk.AssistantMessage:
		for _, block := range m.Content {
			if textBlock, ok := block.(*codexsdk.TextBlock); ok {
				fmt.Printf("Codex: %s\n", textBlock.Text)
			}
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

// basicExample demonstrates a simple question.
func basicExample() {
	fmt.Println("=== Basic Example ===")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2 + 2?")) {
		if err != nil {
			fmt.Printf("Query failed: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// withOptionsExample demonstrates using custom options.
func withOptionsExample() {
	fmt.Println("=== With Options Example ===")

	_ = slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Explain what Golang is in one sentence."),
		codexsdk.WithSystemPrompt("You are a helpful assistant that explains things simply."),
	) {
		if err != nil {
			fmt.Printf("Query failed: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// withToolsExample demonstrates using allowed tools with cost reporting.
func withToolsExample() {
	fmt.Println("=== With Tools Example ===")

	_ = slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Create a file called hello.txt with 'Hello, World!' in it"),
		codexsdk.WithAllowedTools("Read", "Write"),
		codexsdk.WithSystemPrompt("You are a helpful file assistant."),
	) {
		if err != nil {
			fmt.Printf("Query failed: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

func main() {
	fmt.Println("Quick Start Examples")
	fmt.Println()

	basicExample()
	withOptionsExample()
	withToolsExample()
}
