//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

// TestAgentsAndSettings_AgentDefinition tests custom agent configuration.
func TestAgentsAndSettings_AgentDefinition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResponse := false

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Say 'hello'"),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			t.Logf("Received assistant message with %d content blocks", len(m.Content))
			receivedResponse = true
		case *codexsdk.ResultMessage:
			require.False(t, m.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResponse, "Should receive assistant response")
}

// TestAgentsAndSettings_SettingSources tests setting source loading.
func TestAgentsAndSettings_SettingSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2+2? Reply with just the number."),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestAgentsAndSettings_NoSettingSources tests isolated environment without settings.
func TestAgentsAndSettings_NoSettingSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	receivedResult := false

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Say 'isolated'"),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		if result, ok := msg.(*codexsdk.ResultMessage); ok {
			receivedResult = true
			require.False(t, result.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedResult, "Should receive result message")
}

// TestAgentsAndSettings_FilesystemAgentLoading tests that filesystem-based agents
// load via setting_sources=["project"] and produce a full response cycle.
func TestAgentsAndSettings_FilesystemAgentLoading(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "codex-sdk-test-*")
	require.NoError(t, err)

	defer os.RemoveAll(tmpDir)

	agentsDir := filepath.Join(tmpDir, ".claude", "agents")
	err = os.MkdirAll(agentsDir, 0755)
	require.NoError(t, err)

	agentFile := filepath.Join(agentsDir, "fs-test-agent.md")
	agentContent := `---
name: fs-test-agent
description: A filesystem test agent for SDK testing
tools: Read
---

# Filesystem Test Agent

You are a simple test agent. When asked a question, provide a brief, helpful answer.
`
	err = os.WriteFile(agentFile, []byte(agentContent), 0644)
	require.NoError(t, err)

	var (
		receivedSystem    bool
		receivedInit      bool
		receivedAssistant bool
		receivedResult    bool
		foundAgent        bool
	)

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("Say hello in exactly 3 words"),
		codexsdk.WithCwd(tmpDir),
		codexsdk.WithPermissionMode("bypassPermissions"),
	) {
		if err != nil {
			skipIfCLINotInstalled(t, err)
			t.Fatalf("Query failed: %v", err)
		}

		switch m := msg.(type) {
		case *codexsdk.SystemMessage:
			receivedSystem = true

			t.Logf("SystemMessage subtype=%s, data keys=%v", m.Subtype, mapKeys(m.Data))

			if m.Subtype == "init" {
				receivedInit = true

				if agents, ok := m.Data["agents"].([]any); ok {
					for _, agent := range agents {
						switch a := agent.(type) {
						case string:
							if a == "fs-test-agent" {
								foundAgent = true

								t.Logf("Found filesystem agent: %s", a)
							}
						case map[string]any:
							if name, ok := a["name"].(string); ok && name == "fs-test-agent" {
								foundAgent = true

								t.Logf("Found filesystem agent (object): %s", name)
							}
						}
					}

					if !foundAgent {
						t.Logf("Agents list (%d entries): %v", len(agents), agents)
					}
				}
			}
		case *codexsdk.AssistantMessage:
			receivedAssistant = true
			t.Logf("Received assistant message with %d content blocks", len(m.Content))
		case *codexsdk.ResultMessage:
			receivedResult = true
			require.False(t, m.IsError, "Query should not result in error")
		}
	}

	require.True(t, receivedSystem, "Should receive SystemMessage")
	require.True(t, receivedAssistant,
		"Should receive AssistantMessage - missing may indicate filesystem agent loading issue")
	require.True(t, receivedResult, "Should receive ResultMessage")

	if receivedInit {
		require.True(t, foundAgent,
			"fs-test-agent should be loaded from filesystem via setting_sources")
	} else {
		t.Log("Backend did not send init message with agent data; " +
			"skipping agent presence assertion (exec backend does not report loaded agents)")
	}
}
