package runtime

import (
	"context"
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/hooks"
	"github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"github.com/SocialGouv/iterion/pkg/claw/plugin"
	"strings"
	"testing"
)

// newPluginRegistryWithTool creates a registry with a bundled plugin that
// exposes a single tool executed via "echo" so it works in tests without
// external binaries.
func newPluginRegistryWithTool(toolName, echoOutput string) *plugin.PluginRegistry {
	bp := &plugin.BundledPlugin{
		Meta: plugin.PluginMetadata{
			ID:      "test-plugin@bundled",
			Name:    "test-plugin",
			Version: "1.0.0",
			Kind:    plugin.KindBundled,
		},
		Manifest: plugin.PluginManifest{
			Name:    "test-plugin",
			Version: "1.0.0",
			Tools: []plugin.PluginToolManifest{
				{
					Name:        toolName,
					Description: "A test plugin tool",
					InputSchema: json.RawMessage(`{"type":"object"}`),
					Command:     "echo",
					Args:        []string{echoOutput},
				},
			},
		},
	}
	return plugin.NewPluginRegistry([]plugin.RegisteredPlugin{
		{Plugin: bp, Enabled: true},
	})
}

func TestExecuteToolPluginDispatch(t *testing.T) {
	// A plugin tool registered in PluginRegistry should be dispatched correctly.
	registry := newPluginRegistryWithTool("my_plugin_tool", "plugin-output")

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		PluginRegistry: registry,
	}

	result := loop.ExecuteTool(context.Background(), "my_plugin_tool", map[string]any{"key": "value"})

	if result.IsError {
		t.Fatalf("expected no error, got: %s", result.Content[0].Text)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "plugin-output") {
		t.Errorf("expected plugin output in result, got %q", text)
	}
}

func TestExecuteToolPluginPostHooksRun(t *testing.T) {
	testutil.IsolateShellHome(t)
	// Post-hooks should run for plugin tool execution.
	registry := newPluginRegistryWithTool("my_plugin_tool", "ok")

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		PluginRegistry: registry,
		HookRunner: hooks.NewHookRunner(hooks.HookConfig{
			PostToolUse: []string{`echo '{"continue":false,"reason":"post-hook denied plugin"}'`},
		}),
	}

	result := loop.ExecuteTool(context.Background(), "my_plugin_tool", map[string]any{})

	if !result.IsError {
		t.Error("expected IsError=true when PostToolUse hook denies plugin tool")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "post-hook denied plugin") {
		t.Errorf("expected post-hook denial message, got %q", text)
	}
}

func TestExecuteToolPluginTelemetryRecorded(t *testing.T) {
	// Telemetry tool_execute_end event should be recorded for plugin tools.
	registry := newPluginRegistryWithTool("my_plugin_tool", "telemetry-test")

	sink := &apikit.MemoryTelemetrySink{}
	tracer := apikit.NewSessionTracer("test-session", sink)

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		PluginRegistry: registry,
		Tracer:         tracer,
	}

	result := loop.ExecuteTool(context.Background(), "my_plugin_tool", map[string]any{})

	if result.IsError {
		t.Fatalf("expected no error, got: %s", result.Content[0].Text)
	}

	events := sink.Events()
	found := false
	for _, ev := range events {
		if ev.Type == apikit.EventTypeSessionTrace && ev.SessionTrace != nil && ev.SessionTrace.Name == "tool_execute_end" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected tool_execute_end telemetry event for plugin tool")
	}
}

func TestExecuteToolUnknownFallsThrough(t *testing.T) {
	// Unknown tools should fall through plugin dispatch (no match) and MCP
	// (nil registry) to the "unknown tool" error path.
	registry := newPluginRegistryWithTool("my_plugin_tool", "should-not-match")

	loop := &ConversationLoop{
		Config:         &Config{},
		Permissions:    DefaultPermissions(),
		PluginRegistry: registry,
	}

	result := loop.ExecuteTool(context.Background(), "totally_unknown_tool", map[string]any{})

	if !result.IsError {
		t.Error("expected IsError for unknown tool")
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got %q", text)
	}
}
