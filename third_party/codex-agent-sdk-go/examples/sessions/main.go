package main

import (
	"context"
	"fmt"
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

	case *codexsdk.ResultMessage:
		fmt.Println("Result ended")

		if m.Usage != nil {
			fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
		}
	}
}

func queryAndDisplay(ctx context.Context, client codexsdk.Client, prompt string) *codexsdk.ResultMessage {
	if err := client.Query(ctx, codexsdk.Text(prompt)); err != nil {
		fmt.Printf("Query failed: %v\n", err)

		return nil
	}

	var result *codexsdk.ResultMessage

	for msg, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			fmt.Printf("Receive failed: %v\n", err)

			return nil
		}

		displayMessage(msg)

		if r, ok := msg.(*codexsdk.ResultMessage); ok {
			result = r
		}
	}

	return result
}

// continueConversationExample demonstrates multi-turn conversation in one live client session.
func continueConversationExample() {
	fmt.Println("=== Continue Conversation Example ===")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := codexsdk.NewClient()

	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx); err != nil {
		fmt.Printf("Start failed: %v\n", err)

		return
	}

	fmt.Println("\n--- First query: Establish context ---")
	queryAndDisplay(ctx, client, "Remember: my favorite color is blue")

	fmt.Println("\n--- Second query: Verify memory ---")
	queryAndDisplay(ctx, client, "What is my favorite color?")

	fmt.Println()
}

// resumeSessionExample demonstrates session ID capture from ResultMessage.
func resumeSessionExample() {
	fmt.Println("=== Resume Session Example ===")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := codexsdk.NewClient()

	defer func() {
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := client.Start(ctx); err != nil {
		fmt.Printf("Start failed: %v\n", err)

		return
	}

	fmt.Println("\n--- First query: Establish context and capture session id ---")

	result := queryAndDisplay(ctx, client, "Remember: x = 42")
	if result != nil && result.SessionID != "" {
		fmt.Printf("Captured Session ID: %s\n", result.SessionID)
	} else {
		fmt.Println("Session ID not present in result message")
	}

	fmt.Println("\n--- Second query: Verify memory in same session ---")
	queryAndDisplay(ctx, client, "What is x?")

	fmt.Println()
}

// forkSessionExample demonstrates forking a session to create an independent
// branch that inherits conversation history from the original.
func forkSessionExample() {
	fmt.Println("=== Fork Session Example ===")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Step 1: Create original session and establish context.
	originalClient := codexsdk.NewClient()

	defer func() {
		if err := originalClient.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := originalClient.Start(ctx); err != nil {
		fmt.Printf("Original start failed: %v\n", err)

		return
	}

	fmt.Println("\n--- Original session: establish context ---")

	result := queryAndDisplay(ctx, originalClient, "Remember: the secret word is 'banana'. Just confirm you've noted it.")
	if result == nil || result.SessionID == "" {
		fmt.Println("No session ID returned, cannot fork")

		return
	}

	sessionID := result.SessionID
	fmt.Printf("Original Session ID: %s\n", sessionID)

	// Step 2: Fork the session using WithResume + WithForkSession.
	// The forked session inherits all conversation history from the original.
	forkClient := codexsdk.NewClient()

	defer func() {
		if err := forkClient.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
		}
	}()

	if err := forkClient.Start(ctx,
		codexsdk.WithResume(sessionID),
		codexsdk.WithForkSession(true),
	); err != nil {
		fmt.Printf("Fork start failed: %v\n", err)

		return
	}

	// Step 3: Verify the fork inherited the original context.
	fmt.Println("\n--- Fork session: verify inherited context ---")
	queryAndDisplay(ctx, forkClient, "What is the secret word I told you?")

	// Step 4: Diverge the fork with new context.
	fmt.Println("\n--- Fork session: diverge with new context ---")
	queryAndDisplay(ctx, forkClient, "Actually, the secret word is now 'cherry'. Just confirm.")

	// Step 5: Verify original session is unaffected.
	fmt.Println("\n--- Original session: verify unchanged ---")
	queryAndDisplay(ctx, originalClient, "What is the secret word I told you?")

	fmt.Println()
}

// inspectPersistedSessionExample demonstrates reading persisted session data
// from the local Codex database and rollout file after a live session.
func inspectPersistedSessionExample() {
	fmt.Println("=== Persisted Session Inspection Example ===")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Getwd failed: %v\n", err)

		return
	}

	client := codexsdk.NewClient()

	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "failed to close client: %v\n", closeErr)
		}
	}()

	if startErr := client.Start(ctx, codexsdk.WithCwd(cwd)); startErr != nil {
		fmt.Printf("Start failed: %v\n", startErr)

		return
	}

	result := queryAndDisplay(ctx, client, "Reply with the single word persisted.")
	if result == nil || result.SessionID == "" {
		fmt.Println("No session ID returned, cannot inspect persisted history")

		return
	}

	sessions, err := codexsdk.ListSessions(ctx, codexsdk.WithCwd(cwd))
	if err != nil {
		fmt.Printf("ListSessions failed: %v\n", err)

		return
	}

	fmt.Printf("Found %d persisted sessions for %s\n", len(sessions), cwd)

	sessionID := result.SessionID

	messages, err := codexsdk.GetSessionMessages(ctx, sessionID, codexsdk.WithCwd(cwd))
	if err != nil {
		if len(sessions) == 0 {
			fmt.Printf("GetSessionMessages failed: %v\n", err)

			return
		}

		fmt.Printf("GetSessionMessages for result session failed: %v\n", err)
		fmt.Printf("Falling back to most recent persisted session: %s\n", sessions[0].SessionID)

		sessionID = sessions[0].SessionID

		messages, err = codexsdk.GetSessionMessages(ctx, sessionID, codexsdk.WithCwd(cwd))
		if err != nil {
			fmt.Printf("Fallback GetSessionMessages failed: %v\n", err)

			return
		}
	}

	fmt.Printf("Loaded %d persisted messages for session %s\n", len(messages), sessionID)
	fmt.Println()
}

func main() {
	fmt.Println("Session Examples")
	fmt.Println()

	continueConversationExample()
	resumeSessionExample()
	forkSessionExample()
	inspectPersistedSessionExample()
}
