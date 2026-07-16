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

func noSystemPrompt() {
	fmt.Println("=== No System Prompt (Vanilla Codex) ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx, codexsdk.WithLogger(logger)); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("What is 2 + 2?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

func stringSystemPrompt() {
	fmt.Println("=== String System Prompt ===")

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
		codexsdk.WithSystemPrompt("You are a pirate assistant. Respond in pirate speak."),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("What is 2 + 2?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

func presetSystemPrompt() {
	fmt.Println("=== Preset System Prompt (Default) ===")

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
		codexsdk.WithSystemPromptPreset(&codexsdk.SystemPromptPreset{
			Type:   "preset",
			Preset: "claude_code",
		}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("What is 2 + 2?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

func presetWithAppend() {
	fmt.Println("=== Preset System Prompt with Append ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	appendText := "Always end your response with a fun fact."

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithSystemPromptPreset(&codexsdk.SystemPromptPreset{
			Type:   "preset",
			Preset: "claude_code",
			Append: &appendText,
		}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	if err := client.Query(ctx, codexsdk.Text("What is 2 + 2?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

func main() {
	fmt.Println("System Prompt Examples")
	fmt.Println()

	noSystemPrompt()
	stringSystemPrompt()
	presetSystemPrompt()
	presetWithAppend()
}
