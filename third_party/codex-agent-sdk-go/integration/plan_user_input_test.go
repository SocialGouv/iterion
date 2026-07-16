//go:build integration

package integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestPlanMode_UserInputCallback tests the full round-trip of plan mode with
// user input callbacks: the agent uses request_user_input, the SDK invokes
// the callback, and the agent receives the answer.
func TestPlanMode_UserInputCallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var callbackInvoked atomic.Bool
	var selectedAnswer string

	userInputCallback := func(
		_ context.Context,
		req *codexsdk.UserInputRequest,
	) (*codexsdk.UserInputResponse, error) {
		callbackInvoked.Store(true)

		answers := make(map[string]*codexsdk.UserInputAnswer, len(req.Questions))

		for _, q := range req.Questions {
			t.Logf("User input question: %s (options: %d)", q.Question, len(q.Options))

			if len(q.Options) > 0 {
				selectedAnswer = q.Options[0].Label
				t.Logf("Auto-selecting first option: %s", selectedAnswer)

				answers[q.ID] = &codexsdk.UserInputAnswer{
					Answers: []string{q.Options[0].Label},
				}
			} else {
				selectedAnswer = "Go"
				answers[q.ID] = &codexsdk.UserInputAnswer{
					Answers: []string{selectedAnswer},
				}
			}
		}

		return &codexsdk.UserInputResponse{Answers: answers}, nil
	}

	permissionCallback := func(
		_ context.Context,
		toolName string,
		_ map[string]any,
		_ *codexsdk.ToolPermissionContext,
	) (codexsdk.PermissionResult, error) {
		t.Logf("Auto-allowing tool: %s", toolName)

		return &codexsdk.PermissionResultAllow{Behavior: "allow"}, nil
	}

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithOnUserInput(userInputCallback),
		codexsdk.WithCanUseTool(permissionCallback),
		codexsdk.WithPermissionMode("plan"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Start failed: %v", err)
	}

	err = client.Query(ctx,
		codexsdk.Text(`Use the request_user_input tool to ask me to choose between: Go, Rust, Python. `+
			`Then tell me which one I selected.`))
	require.NoError(t, err, "Query should succeed")

	var gotResult bool
	var assistantText string

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			t.Logf("Receive error: %v", err)

			break
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if tb, ok := block.(*codexsdk.TextBlock); ok {
					t.Logf("Assistant: %s", tb.Text)
					assistantText += tb.Text
				}
			}
		case *codexsdk.ResultMessage:
			gotResult = true
			t.Logf("Result: isError=%v", m.IsError)

			if m.Usage != nil {
				t.Logf("Usage: %d in / %d out", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}

		if gotResult {
			break
		}
	}

	require.True(t, callbackInvoked.Load(), "User input callback should have been invoked")
	require.True(t, gotResult, "Should receive a ResultMessage")
	require.NotEmpty(t, selectedAnswer, "Should have selected an answer")
	t.Logf("Selected answer: %s", selectedAnswer)
	t.Logf("Full assistant text: %s", assistantText)
}

// TestPlanMode_StartWithContent tests plan mode using StartWithContent for
// a single-turn flow that triggers the user input callback.
func TestPlanMode_StartWithContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var callbackInvoked atomic.Bool

	userInputCallback := func(
		_ context.Context,
		req *codexsdk.UserInputRequest,
	) (*codexsdk.UserInputResponse, error) {
		callbackInvoked.Store(true)

		answers := make(map[string]*codexsdk.UserInputAnswer, len(req.Questions))

		for _, q := range req.Questions {
			t.Logf("User input question: %s", q.Question)

			answer := "alpha"
			if len(q.Options) > 0 {
				answer = q.Options[0].Label
			}

			answers[q.ID] = &codexsdk.UserInputAnswer{
				Answers: []string{answer},
			}
		}

		return &codexsdk.UserInputResponse{Answers: answers}, nil
	}

	permissionCallback := func(
		_ context.Context,
		_ string,
		_ map[string]any,
		_ *codexsdk.ToolPermissionContext,
	) (codexsdk.PermissionResult, error) {
		return &codexsdk.PermissionResultAllow{Behavior: "allow"}, nil
	}

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.StartWithContent(ctx,
		codexsdk.Text(`Use the request_user_input tool to ask me a yes/no question. Then confirm my answer.`),
		codexsdk.WithOnUserInput(userInputCallback),
		codexsdk.WithCanUseTool(permissionCallback),
		codexsdk.WithPermissionMode("plan"),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("StartWithContent failed: %v", err)
	}

	var gotResult bool

	for msg, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			t.Logf("Receive error: %v", err)

			break
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			gotResult = true
			t.Logf("Result: isError=%v", result.IsError)

			break
		}
	}

	require.True(t, callbackInvoked.Load(), "User input callback should have been invoked")
	require.True(t, gotResult, "Should receive a ResultMessage")
}
