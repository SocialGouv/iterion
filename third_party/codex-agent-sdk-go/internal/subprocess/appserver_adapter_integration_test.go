//go:build integration

package subprocess

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func generateAppServerSchemaDir(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("Codex CLI not installed")
	}

	dir := t.TempDir()
	cmd := exec.Command("codex", "app-server", "generate-json-schema", "--out", dir)

	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "generate schema: %s", string(output))

	return dir
}

func readSchema(t *testing.T, dir string, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)

	return string(data)
}

func TestCamelToSnake_CoversCurrentCodexThreadItemTypes(t *testing.T) {
	schemaDir := generateAppServerSchemaDir(t)
	itemCompletedJSON := readSchema(t, schemaDir, filepath.Join("v2", "ItemCompletedNotification.json"))

	tests := []struct {
		camelCase string
		snakeCase string
	}{
		{"userMessage", "user_message"},
		{"plan", "plan"},
		{"mcpToolCall", "mcp_tool_call"},
		{"collabAgentToolCall", "collab_agent_tool_call"},
		{"imageView", "image_view"},
		{"enteredReviewMode", "entered_review_mode"},
		{"exitedReviewMode", "exited_review_mode"},
		{"contextCompaction", "context_compaction"},
	}

	for _, tc := range tests {
		require.Contains(t, itemCompletedJSON, `"`+tc.camelCase+`"`,
			"installed codex schema no longer contains %s; update this proof test", tc.camelCase)
		require.Equal(t, tc.snakeCase, camelToSnake(tc.camelCase),
			"current codex item type %q should translate to %q", tc.camelCase, tc.snakeCase)
	}
}

func TestCurrentCodexSchema_ContainsAdditionalStreamingDeltaNotifications(t *testing.T) {
	schemaDir := generateAppServerSchemaDir(t)
	serverNotificationJSON := readSchema(t, schemaDir, "ServerNotification.json")

	liveDeltaMethods := []string{
		"item/agentMessage/delta",
		"item/reasoning/textDelta",
		"item/commandExecution/outputDelta",
		"item/fileChange/outputDelta",
		"item/plan/delta",
	}

	for _, method := range liveDeltaMethods {
		require.Contains(t, serverNotificationJSON, `"`+method+`"`,
			"installed codex schema no longer contains %s; update this proof test", method)
	}
}
