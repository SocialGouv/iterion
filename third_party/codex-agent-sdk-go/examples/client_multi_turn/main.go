package main

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
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

// exampleBasicStreaming demonstrates basic streaming with a simple query.
func exampleBasicStreaming() {
	fmt.Println("=== Basic Streaming Example ===")

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

	fmt.Println("User: What is 2+2?")

	if err := client.Query(ctx, codexsdk.Text("What is 2+2?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	// Receive complete response using iterator
	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// exampleMultiTurn demonstrates multi-turn conversations.
func exampleMultiTurn() {
	fmt.Println("=== Multi-Turn Conversation Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// First turn
	fmt.Println("User: What's the capital of France?")

	if err := client.Query(ctx, codexsdk.Text("What's the capital of France?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, msgErr := range client.ReceiveResponse(ctx) {
		if msgErr != nil {
			fmt.Printf("Failed to receive response: %v\n", msgErr)

			return
		}

		displayMessage(msg)
	}

	// Second turn - follow-up
	fmt.Println("\nUser: What's the population of that city?")

	if err := client.Query(ctx, codexsdk.Text("What's the population of that city?")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	for msg, msgErr := range client.ReceiveResponse(ctx) {
		if msgErr != nil {
			fmt.Printf("Failed to receive response: %v\n", msgErr)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// exampleConcurrent demonstrates concurrent send/receive using goroutines.
func exampleConcurrent() {
	fmt.Println("=== Concurrent Send/Receive Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	var wg sync.WaitGroup

	// Create a cancellable context for the receiver goroutine
	recvCtx, recvCancel := context.WithCancel(ctx)

	// Background goroutine to continuously receive messages

	wg.Go(func() {
		for msg, err := range client.ReceiveMessages(recvCtx) {
			if err != nil {
				return
			}

			displayMessage(msg)
		}
	})

	// Send multiple messages with delays
	questions := []string{
		"What is 2 + 2?",
		"What is the square root of 144?",
		"What is 10% of 80?",
	}

	for _, question := range questions {
		fmt.Printf("\nUser: %s\n", question)

		if err := client.Query(ctx, codexsdk.Text(question)); err != nil {
			fmt.Printf("Failed to send query: %v\n", err)

			break
		}

		time.Sleep(3 * time.Second) // Wait between messages
	}

	// Give time for final responses
	time.Sleep(2 * time.Second)

	// Cancel receiver context to unblock the goroutine
	recvCancel()
	wg.Wait()

	fmt.Println()
}

// exampleInterrupt demonstrates the interrupt capability.
func exampleInterrupt() {
	fmt.Println("=== Interrupt Example ===")
	fmt.Println("IMPORTANT: Interrupts require active message consumption.")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// Start a long-running task
	fmt.Println("\nUser: Count from 1 to 100 slowly")

	queryText := "Count from 1 to 100 slowly, with a brief pause between each number"
	if err := client.Query(ctx, codexsdk.Text(queryText)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	var wg sync.WaitGroup

	done := make(chan struct{})

	var messagesReceived []codexsdk.Message

	// Background goroutine to consume messages

	wg.Go(func() {
		// Use iter.Pull2 to convert iterator to pull-based for use with select
		next, stop := iter.Pull2(client.ReceiveMessages(ctx))
		defer stop()

		for {
			select {
			case <-done:
				return
			default:
				msg, err, ok := next()
				if !ok || err != nil {
					return
				}

				messagesReceived = append(messagesReceived, msg)
				displayMessage(msg)

				// Check if we got a result (interrupt processed)
				if _, ok := msg.(*codexsdk.ResultMessage); ok {
					return
				}
			}
		}
	})

	// Wait 2 seconds then send interrupt
	time.Sleep(2 * time.Second)
	fmt.Println("\n[After 2 seconds, sending interrupt...]")

	if err := client.Interrupt(ctx); err != nil {
		fmt.Printf("Failed to send interrupt: %v\n", err)
	}

	// Wait for the consume task to finish
	wg.Wait()
	close(done)

	// Send new instruction after interrupt
	fmt.Println("\nUser: Never mind, just tell me a quick joke")

	if err := client.Query(ctx, codexsdk.Text("Never mind, just tell me a quick joke")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	// Get the joke
	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// exampleManualHandling demonstrates manual message stream handling.
func exampleManualHandling() {
	fmt.Println("=== Manual Message Handling Example ===")

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

	fmt.Println("User: List 5 programming languages and their main use cases")

	queryText := "List 5 programming languages and their main use cases"
	if err := client.Query(ctx, codexsdk.Text(queryText)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	// Manually process messages with custom logic
	languagesFound := []string{}
	targetLanguages := []string{"Golang", "JavaScript", "Java", "C++", "Go", "Rust", "Ruby"}

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		if assistantMsg, ok := msg.(*codexsdk.AssistantMessage); ok {
			for _, block := range assistantMsg.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					text := textBlock.Text
					fmt.Printf("Codex: %s\n", text)

					// Custom logic: extract language names
					for _, lang := range targetLanguages {
						if strings.Contains(text, lang) && !slices.Contains(languagesFound, lang) {
							languagesFound = append(languagesFound, lang)
							fmt.Printf("Found language: %s\n", lang)
						}
					}
				}
			}
		}

		if resultMsg, ok := msg.(*codexsdk.ResultMessage); ok {
			displayMessage(resultMsg)
			fmt.Printf("Total languages mentioned: %d\n", len(languagesFound))

			break
		}
	}

	fmt.Println()
}

// exampleWithOptions demonstrates using ClaudeAgentOptions.
func exampleWithOptions() {
	fmt.Println("=== Custom Options Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	// Configure options
	if err := client.Start(ctx,
		codexsdk.WithLogger(logger),
		codexsdk.WithAllowedTools("Read", "Write"),
		codexsdk.WithSystemPrompt("You are a helpful coding assistant."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithEnv(map[string]string{
			"ANTHROPIC_MODEL": "claude-sonnet-4-5",
		}),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	fmt.Println("User: Create a simple hello.txt file with a greeting message")

	queryText := "Create a simple hello.txt file with a greeting message"
	if err := client.Query(ctx, codexsdk.Text(queryText)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	toolUses := []string{}

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		if assistantMsg, ok := msg.(*codexsdk.AssistantMessage); ok {
			displayMessage(msg)

			for _, block := range assistantMsg.Content {
				if toolUseBlock, ok := block.(*codexsdk.ToolUseBlock); ok {
					toolUses = append(toolUses, toolUseBlock.Name)
				}
			}
		} else {
			displayMessage(msg)
		}
	}

	if len(toolUses) > 0 {
		fmt.Printf("Tools used: %s\n", strings.Join(toolUses, ", "))
	}

	fmt.Println()
}

// exampleBashCommand demonstrates tool use blocks when running bash commands.
func exampleBashCommand() {
	fmt.Println("=== Bash Command Example ===")

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
		codexsdk.WithPermissionMode("bypassPermissions"),
	); err != nil {
		fmt.Printf("Failed to connect: %v\n", err)

		return
	}

	fmt.Println("User: Run a bash echo command")

	queryText := "Run a bash echo command that says 'Hello from bash!'"
	if err := client.Query(ctx, codexsdk.Text(queryText)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	messageTypes := make(map[string]bool)

messageLoop:
	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			break
		}

		messageTypes[fmt.Sprintf("%T", msg)] = true

		switch m := msg.(type) {
		case *codexsdk.UserMessage:
			for _, block := range m.Content.Blocks() {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Printf("User: %s\n", textBlock.Text)
				} else if toolResultBlock, ok := block.(*codexsdk.ToolResultBlock); ok {
					// Extract text from ContentBlocks
					var contentText string

					for _, cb := range toolResultBlock.Content {
						if tb, ok := cb.(*codexsdk.TextBlock); ok {
							contentText += tb.Text
						}
					}

					if len(contentText) > 100 {
						contentText = contentText[:100]
					}

					fmt.Printf("Tool Result (id: %s): %s...\n", toolResultBlock.ToolUseID, contentText)
				}
			}

		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Printf("Codex: %s\n", textBlock.Text)
				} else if toolUseBlock, ok := block.(*codexsdk.ToolUseBlock); ok {
					fmt.Printf("Tool Use: %s (id: %s)\n", toolUseBlock.Name, toolUseBlock.ID)

					if toolUseBlock.Name == "Bash" {
						if command, ok := toolUseBlock.Input["command"].(string); ok {
							fmt.Printf("  Command: %s\n", command)
						}
					}
				}
			}

		case *codexsdk.ResultMessage:
			displayMessage(msg)

			typeNames := []string{}
			for typeName := range messageTypes {
				typeNames = append(typeNames, typeName)
			}

			fmt.Printf("\nMessage types received: %s\n", strings.Join(typeNames, ", "))

			break messageLoop
		}
	}

	fmt.Println()
}

// exampleControlProtocol demonstrates control protocol capabilities.
func exampleControlProtocol() {
	fmt.Println("=== Control Protocol Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	fmt.Println("✓ Connected to Codex CLI")

	// 1. Get server initialization info
	fmt.Println("\n1. Getting server info...")

	serverInfo := client.GetServerInfo()

	if serverInfo != nil {
		fmt.Println("✓ Server info retrieved successfully!")

		if commands, ok := serverInfo["commands"].([]any); ok {
			fmt.Printf("  - Available commands: %d\n", len(commands))
		}

		if outputStyle, ok := serverInfo["output_style"].(string); ok {
			fmt.Printf("  - Output style: %s\n", outputStyle)
		}

		if styles, ok := serverInfo["available_output_styles"].([]any); ok {
			styleStrs := make([]string, 0, len(styles))

			for _, s := range styles {
				if str, ok := s.(string); ok {
					styleStrs = append(styleStrs, str)
				}
			}

			if len(styleStrs) > 0 {
				fmt.Printf("  - Available output styles: %s\n", strings.Join(styleStrs, ", "))
			}
		}
	} else {
		fmt.Println("✗ No server info available (may not be in streaming mode)")
	}

	// 2. Demonstrate interrupt capability with a long-running task
	fmt.Println("\n2. Testing interrupt capability...")
	fmt.Println("User: Count from 1 to 100 slowly")

	queryText := "Count from 1 to 100 slowly, with a brief pause between each number"
	if err := client.Query(ctx, codexsdk.Text(queryText)); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	var wg sync.WaitGroup

	done := make(chan struct{})

	// Background goroutine to consume messages

	wg.Go(func() {
		// Use iter.Pull2 to convert iterator to pull-based for use with select
		next, stop := iter.Pull2(client.ReceiveMessages(ctx))
		defer stop()

		for {
			select {
			case <-done:
				return
			default:
				msg, recvErr, ok := next()
				if !ok || recvErr != nil {
					return
				}

				displayMessage(msg)

				// Check if we got a result (interrupt processed)
				if _, ok := msg.(*codexsdk.ResultMessage); ok {
					return
				}
			}
		}
	})

	// Wait 2 seconds then send interrupt
	time.Sleep(2 * time.Second)
	fmt.Println("\n[After 2 seconds, sending interrupt...]")

	if err := client.Interrupt(ctx); err != nil {
		fmt.Printf("Failed to send interrupt: %v\n", err)
	} else {
		fmt.Println("✓ Interrupt sent successfully")
	}

	// Wait for the consume task to finish
	wg.Wait()
	close(done)

	// Send new instruction after interrupt
	fmt.Println("\nUser: Never mind, just tell me a quick joke")

	if err := client.Query(ctx, codexsdk.Text("Never mind, just tell me a quick joke")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	// Get the joke
	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Failed to receive response: %v\n", err)

			return
		}

		displayMessage(msg)
	}

	fmt.Println()
}

// exampleChannelPrompt demonstrates sending multiple queries via channels (Go equivalent of async iterable).
func exampleChannelPrompt() {
	fmt.Println("=== Channel Prompt Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// Send multiple questions and receive responses
	questions := []string{
		"Hello! I have multiple questions.",
		"First, what's the capital of Japan?",
		"Second, what's 15% of 200?",
	}

	for _, question := range questions {
		fmt.Printf("User: %s\n", question)

		if err := client.Query(ctx, codexsdk.Text(question)); err != nil {
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
}

// exampleErrorHandling demonstrates proper error handling with timeouts.
func exampleErrorHandling() {
	fmt.Println("=== Error Handling Example ===")

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	client := codexsdk.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// Send a message that will take time to process
	fmt.Println("User: Run a bash sleep command for 60 seconds not in the background")

	if err := client.Query(ctx, codexsdk.Text("Run a bash sleep command for 60 seconds not in the background")); err != nil {
		fmt.Printf("Failed to send query: %v\n", err)

		return
	}

	// Try to receive response with a short timeout
	shortCtx, shortCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shortCancel()

	messageCount := 0

	for msg, err := range client.ReceiveMessages(shortCtx) {
		if err != nil {
			// Check if it's a timeout
			if shortCtx.Err() == context.DeadlineExceeded {
				fmt.Println("\nResponse timeout after 10 seconds - demonstrating graceful handling")
				fmt.Printf("Received %d messages before timeout\n", messageCount)
			} else {
				fmt.Printf("Error receiving message: %v\n", err)
			}

			break
		}

		messageCount++

		if assistantMsg, ok := msg.(*codexsdk.AssistantMessage); ok {
			for _, block := range assistantMsg.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					// Print first 50 chars to show progress
					text := textBlock.Text
					if len(text) > 50 {
						text = text[:50] + "..."
					}

					fmt.Printf("Codex: %s\n", text)
				}
			}
		} else if _, ok := msg.(*codexsdk.ResultMessage); ok {
			displayMessage(msg)

			break
		}
	}

	fmt.Println()
}

func main() {
	examples := map[string]func(){
		"basic_streaming":         exampleBasicStreaming,
		"multi_turn_conversation": exampleMultiTurn,
		"concurrent_responses":    exampleConcurrent,
		"with_interrupt":          exampleInterrupt,
		"manual_message_handling": exampleManualHandling,
		"with_options":            exampleWithOptions,
		"async_iterable_prompt":   exampleChannelPrompt,
		"bash_command":            exampleBashCommand,
		"control_protocol":        exampleControlProtocol,
		"error_handling":          exampleErrorHandling,
	}

	if len(os.Args) < 2 {
		fmt.Println("Streaming Client Examples")
		fmt.Println("\nUsage:")
		fmt.Println("  go run main.go <example_name>")
		fmt.Println("  go run main.go all")
		fmt.Println("\nAvailable examples:")

		for name := range examples {
			fmt.Printf("  - %s\n", name)
		}

		return
	}

	exampleName := os.Args[1]

	if exampleName == "all" {
		// Run a representative subset by default to keep runtime predictable.
		// The remaining examples are still available via explicit example names.
		exampleOrder := []string{
			"basic_streaming",
			"multi_turn_conversation",
			"async_iterable_prompt",
			"with_options",
		}

		for _, name := range exampleOrder {
			if fn, ok := examples[name]; ok {
				fn()
			}
		}
	} else if fn, ok := examples[exampleName]; ok {
		fn()
	} else {
		fmt.Printf("Unknown example: %s\n", exampleName)
		fmt.Println("\nAvailable examples:")

		for name := range examples {
			fmt.Printf("  - %s\n", name)
		}

		os.Exit(1)
	}
}
