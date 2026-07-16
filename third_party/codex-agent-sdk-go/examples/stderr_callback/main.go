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
				fmt.Printf("Response: %s\n", textBlock.Text)
			}
		}
	case *codexsdk.ResultMessage:
		fmt.Println("Result ended")

		if m.Usage != nil {
			fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
		}
	}
}

func main() {
	fmt.Println("Stderr Callback Example")
	fmt.Println("Capturing CLI stderr output via callback")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	client := codexsdk.NewClient()

	var stderrMessages []string

	stderrCallback := func(message string) {
		stderrMessages = append(stderrMessages, message)
		if strings.Contains(strings.ToLower(message), "error") {
			fmt.Printf("stderr contains error text: %s\n", message)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithStderr(stderrCallback),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	fmt.Println("Running query with stderr capture...")

	if err := client.Query(ctx, codexsdk.Text("What is 2+2?")); err != nil {
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

	fmt.Printf("\nCaptured %d stderr lines\n", len(stderrMessages))

	if len(stderrMessages) > 0 {
		firstLine := stderrMessages[0]
		if len(firstLine) > 100 {
			firstLine = firstLine[:100]
		}

		fmt.Printf("First stderr line: %s\n", firstLine)
	} else {
		fmt.Println("No stderr output observed in this run.")
	}
}
