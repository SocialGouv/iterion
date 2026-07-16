//go:build integration

package message

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParse_LiveCodexExecFileChangeEvent(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("Codex CLI not installed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tmpDir := t.TempDir()
	targetFile := filepath.Join(tmpDir, "cli-proof.txt")
	prompt := "Create a new file named cli-proof.txt containing exactly: cli proof. Use the built-in file editing tool and do not use bash. Do not answer unless the file is created."

	cmd := exec.CommandContext(ctx,
		"codex", "exec",
		"--json",
		"--color", "never",
		"--ephemeral",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"-C", tmpDir,
		prompt,
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	require.NoError(t, err, "codex exec failed: %s", stderr.String())

	_, statErr := os.Stat(targetFile)
	require.NoError(t, statErr, "codex exec should create the target file before emitting a completed file_change")

	var rawEvent map[string]any

	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	for scanner.Scan() {
		line := scanner.Bytes()

		var candidate map[string]any
		if unmarshalErr := json.Unmarshal(line, &candidate); unmarshalErr != nil {
			continue
		}

		if candidate["type"] != "item.completed" {
			continue
		}

		item, _ := candidate["item"].(map[string]any)
		if item == nil || item["type"] != "file_change" {
			continue
		}

		rawEvent = candidate

		break
	}

	require.NoError(t, scanner.Err())
	require.NotNil(t, rawEvent, "expected codex exec --json to emit an item.completed file_change event")

	parsed, err := Parse(slog.Default(), rawEvent)
	require.NoError(t, err, "the SDK parser should accept the current live codex exec file_change event shape")

	assistant, ok := parsed.(*AssistantMessage)
	require.True(t, ok, "expected parsed file_change event to become an AssistantMessage")
	require.Len(t, assistant.Content, 1)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected parsed file_change event to surface a ToolUseBlock")
	require.Equal(t, "Write", toolUse.Name)

	filePath, ok := toolUse.Input["file_path"].(string)
	require.True(t, ok)
	require.True(t, filePath == targetFile || strings.HasSuffix(filePath, string(filepath.Separator)+"cli-proof.txt") || filePath == "cli-proof.txt")
}
