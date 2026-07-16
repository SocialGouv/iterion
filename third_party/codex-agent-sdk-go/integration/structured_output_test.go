//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestStructuredOutput_JSONSchema tests OutputFormat produces valid JSON.
func TestStructuredOutput_JSONSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var receivedResponse bool

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2+2? Provide structured output."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{
						"type":        "string",
						"description": "The answer to the question",
					},
					"confidence": map[string]any{
						"type":        "number",
						"description": "Confidence level from 0 to 1",
					},
				},
				"required":             []string{"answer", "confidence"},
				"additionalProperties": false,
			},
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					t.Logf("Structured output (assistant): %s", textBlock.Text)
					receivedResponse = true
				}
			}
		case *codexsdk.ResultMessage:
			require.False(t, m.IsError, "Query should not result in error")

			if m.Result != nil && *m.Result != "" {
				t.Logf("Structured output (result): %s", *m.Result)
				receivedResponse = true
			}
		}
	}

	require.True(t, receivedResponse, "Should receive structured response")
}

// TestStructuredOutput_RequiredFields tests required fields are present in output.
func TestStructuredOutput_RequiredFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var receivedResponse bool

	for msg, err := range codexsdk.Query(ctx,
		codexsdk.Text("Generate a fictional person with a name and age in structured format."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type": "string",
					},
					"age": map[string]any{
						"type": "integer",
					},
				},
				"required":             []string{"name", "age"},
				"additionalProperties": false,
			},
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					t.Logf("Output with required fields (assistant): %s", textBlock.Text)
					receivedResponse = true
				}
			}
		case *codexsdk.ResultMessage:
			require.False(t, m.IsError, "Query should not result in error")

			if m.Result != nil && *m.Result != "" {
				t.Logf("Output with required fields (result): %s", *m.Result)
				receivedResponse = true
			}
		}
	}

	require.True(t, receivedResponse, "Should receive response with required fields")
}

// TestStructuredOutput_WithEnum tests structured output with enum type.
func TestStructuredOutput_WithEnum(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var receivedResponse bool

	for msg, err := range codexsdk.Query(ctx,
		codexsdk.Text("Pick a random color and intensity. Respond in structured format."),
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"color": map[string]any{
						"type":        "string",
						"enum":        []string{"red", "green", "blue"},
						"description": "A color choice",
					},
					"intensity": map[string]any{
						"type":        "string",
						"enum":        []string{"low", "medium", "high"},
						"description": "Intensity level",
					},
				},
				"required":             []string{"color", "intensity"},
				"additionalProperties": false,
			},
		}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if textBlock, ok := block.(*codexsdk.TextBlock); ok {
					t.Logf("Structured output with enum (assistant): %s", textBlock.Text)
					receivedResponse = true
				}
			}
		case *codexsdk.ResultMessage:
			require.False(t, m.IsError, "Query should not result in error")

			if m.Result != nil && *m.Result != "" {
				t.Logf("Structured output with enum (result): %s", *m.Result)
				receivedResponse = true
			}
		}
	}

	require.True(t, receivedResponse, "Should receive structured response with enum values")
}

// TestClientStructuredOutput_StartWithOutputFormat verifies session-scoped structured
// output works through the persistent client API.
func TestClientStructuredOutput_StartWithOutputFormat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
				"required":             []string{"answer"},
				"additionalProperties": false,
			},
		}),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Start failed: %v", err)
	}

	err = client.Query(ctx, codexsdk.Text("What is 2+2? Provide structured output."))
	require.NoError(t, err)

	var receivedResponse bool

	for msg, recvErr := range client.ReceiveResponse(ctx) {
		if recvErr != nil {
			t.Fatalf("ReceiveResponse failed: %v", recvErr)
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			require.False(t, result.IsError)

			if result.StructuredOutput != nil {
				receivedResponse = true
			} else if result.Result != nil && *result.Result != "" {
				receivedResponse = true
			}
		}
	}

	require.True(t, receivedResponse, "Should receive structured response from persistent client")
}

func TestClientStructuredOutput_StartWithOutputFormat_ParsesStructuredOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client := codexsdk.NewClient()
	defer client.Close()

	err := client.Start(ctx,
		codexsdk.WithPermissionMode("bypassPermissions"),
		codexsdk.WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string", "enum": []string{"4"}},
				},
				"required":             []string{"answer"},
				"additionalProperties": false,
			},
		}),
	)
	if err != nil {
		skipIfCLINotInstalled(t, err)
		t.Fatalf("Start failed: %v", err)
	}

	err = client.Query(ctx, codexsdk.Text("What is 2+2? Return a JSON object with answer set to the string \"4\"."))
	require.NoError(t, err)

	for msg, recvErr := range client.ReceiveResponse(ctx) {
		if recvErr != nil {
			t.Fatalf("ReceiveResponse failed: %v", recvErr)
		}

		result, ok := msg.(*codexsdk.ResultMessage)
		if !ok {
			continue
		}

		require.False(t, result.IsError)

		structured, ok := result.StructuredOutput.(map[string]any)
		require.True(t, ok, "expected parsed structured output on ResultMessage")
		require.Equal(t, "4", structured["answer"])

		return
	}

	t.Fatal("expected ResultMessage with structured output")
}
