package main

import (
	"context"
	"fmt"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	fmt.Println("=== Error Handling Example ===")
	fmt.Println("Demonstrates checking AssistantMessage.Error for API errors.")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Hello, Codex!")) {
		if err != nil {
			fmt.Printf("Query failed: %v\n", err)

			return
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			// Check for API-level errors on the assistant message.
			// The CLI reports errors like authentication failures and rate
			// limits via the Error field on assistant messages.
			if m.Error != nil {
				switch *m.Error {
				case codexsdk.AssistantMessageErrorAuthFailed:
					fmt.Println("Authentication failed — check your API key.")
				case codexsdk.AssistantMessageErrorRateLimit:
					fmt.Println("Rate limited — retry after a delay.")
				case codexsdk.AssistantMessageErrorBilling:
					fmt.Println("Billing error — check your account.")
				case codexsdk.AssistantMessageErrorInvalidReq:
					fmt.Println("Invalid request — check parameters.")
				case codexsdk.AssistantMessageErrorServer:
					fmt.Println("Server error — retry later.")
				default:
					fmt.Printf("Unknown error: %s\n", *m.Error)
				}

				return
			}

			// Normal response — print text content.
			for _, block := range m.Content {
				if tb, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Printf("Codex: %s\n", tb.Text)
				}
			}

		case *codexsdk.ResultMessage:
			if m.Usage != nil {
				fmt.Printf("Tokens: %d in / %d out\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}
}
