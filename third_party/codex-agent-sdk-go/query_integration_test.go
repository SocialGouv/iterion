//go:build integration

package codexsdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// skipIfCLINotInstalled skips the test if err indicates the Codex CLI binary
// is not found. Call this immediately after receiving the first non-nil error
// from Query or Client.Start.
func skipIfCLINotInstalled(t *testing.T, err error) {
	t.Helper()

	if _, ok := errors.AsType[*CLINotFoundError](err); ok {
		t.Skip("Codex CLI not installed")
	}
}

// TestQuery_WithPersonality tests that WithPersonality passes through to the CLI.
func TestQuery_WithPersonality(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range Query(ctx, Text("Say hello"),
		WithPermissionMode("bypassPermissions"),
		WithPersonality("pragmatic"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestQuery_WithServiceTier tests that WithServiceTier passes through to the CLI.
func TestQuery_WithServiceTier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range Query(ctx, Text("Say hello"),
		WithPermissionMode("bypassPermissions"),
		WithServiceTier("fast"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestQuery_WithDeveloperInstructions tests that WithDeveloperInstructions
// passes through to the CLI and influences the response.
func TestQuery_WithDeveloperInstructions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range Query(ctx, Text("What is 2+2? Reply with just the number."),
		WithPermissionMode("bypassPermissions"),
		WithDeveloperInstructions("Always respond in plain text with no formatting."),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestQuery_WithEffortNone tests that WithEffort(EffortNone) is accepted.
func TestQuery_WithEffortNone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range Query(ctx, Text("What is 2+2? Reply with just the number."),
		WithPermissionMode("bypassPermissions"),
		WithEffort(EffortNone),
		WithConfig(map[string]string{"web_search": "disabled"}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestQuery_WithEffortMinimal_ErrorSurfaced verifies that an API error
// (e.g. incompatible tools with minimal effort) is surfaced as an
// AssistantMessage with an error type rather than being silently dropped.
func TestQuery_WithEffortMinimal_ErrorSurfaced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var sawError bool

	// Intentionally do NOT disallow web_search — this should trigger an API error
	// because web_search is incompatible with minimal effort.
	for msg, err := range Query(ctx, Text("What is 2+2?"),
		WithPermissionMode("bypassPermissions"),
		WithEffort(EffortMinimal),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if assistant, ok := msg.(*AssistantMessage); ok && assistant.Error != nil {
			sawError = true

			for _, block := range assistant.Content {
				if tb, ok := block.(*TextBlock); ok {
					t.Logf("Error surfaced: %s", tb.Text)
					require.Contains(t, tb.Text, "web_search",
						"Error should mention incompatible web_search tool")
				}
			}
		}
	}

	require.True(t, sawError, "Should have received an AssistantMessage with error for incompatible tools")
}

// TestQuery_WithEffortMinimal tests that WithEffort(EffortMinimal) is accepted
// by the SDK and passed to the CLI. Note: "minimal" effort is only supported
// by certain models; the test verifies the SDK passes it through correctly
// and the CLI returns a result (which may be an error if the model doesn't
// support "minimal").
func TestQuery_WithEffortMinimal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range Query(ctx, Text("What is 2+2? Reply with just the number."),
		WithPermissionMode("bypassPermissions"),
		WithEffort(EffortMinimal),
		WithConfig(map[string]string{"web_search": "disabled"}),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if _, ok := msg.(*ResultMessage); ok {
			receivedResult = true
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

func TestQuery_WriteOnlyAllowsCodexFileCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "write-only-proof.txt")

	for _, err := range Query(ctx,
		Text("Create a new file named write-only-proof.txt containing exactly: write only proof"),
		WithCwd(tmpDir),
		WithPermissionMode("default"),
		WithAllowedTools("Write"),
		WithDisallowedTools("Bash"),
		WithSystemPrompt(
			"Use the built-in file editing tool to create the file. Do not use Bash. Do not answer without creating the file.",
		),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}
	}

	data, err := os.ReadFile(targetFile)
	require.NoError(t, err, "codex should create the file when Write is the only allowed file-editing tool")
	require.Equal(t, "write only proof", strings.TrimSpace(string(data)))
}
