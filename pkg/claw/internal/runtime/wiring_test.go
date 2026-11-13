package runtime

import (
	"github.com/SocialGouv/iterion/pkg/claw/hooks"
	"github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
	"github.com/SocialGouv/iterion/pkg/claw/plugin"
	"testing"
)

func TestInitHooksFromConfigEmpty(t *testing.T) {
	loop := &ConversationLoop{Config: &Config{}}
	loop.InitHooksFromConfig(hooks.HookConfig{})
	if loop.HookRunner != nil {
		t.Error("empty config should not create a HookRunner")
	}
}

func TestInitHooksFromConfigWithCommands(t *testing.T) {
	loop := &ConversationLoop{Config: &Config{}}
	loop.InitHooksFromConfig(hooks.HookConfig{
		PreToolUse: []string{"echo pre"},
	})
	if loop.HookRunner == nil {
		t.Fatal("expected HookRunner to be created")
	}
}

func TestInitHooksFromConfigMergesPluginHooks(t *testing.T) {
	// Create a plugin registry with a plugin that has hooks.
	bp := &plugin.BuiltinPlugin{
		Meta: plugin.PluginMetadata{
			ID:      "test-plugin@builtin",
			Name:    "test-plugin",
			Version: "1.0.0",
			Kind:    plugin.KindBuiltin,
		},
		HooksDef: plugin.PluginHooks{
			PostToolUse: []string{"echo plugin-post"},
		},
	}

	registry := plugin.NewPluginRegistry([]plugin.RegisteredPlugin{
		{Plugin: bp, Enabled: true},
	})

	loop := &ConversationLoop{Config: &Config{}}
	loop.PluginRegistry = registry
	loop.InitHooksFromConfig(hooks.HookConfig{
		PreToolUse: []string{"echo config-pre"},
	})

	if loop.HookRunner == nil {
		t.Fatal("expected HookRunner from merged config + plugin hooks")
	}
}

func TestInitTelemetryEmpty(t *testing.T) {
	loop := &ConversationLoop{
		Config:  &Config{},
		Session: NewSession(),
	}
	loop.InitTelemetry("")
	if loop.TelemetrySink == nil {
		t.Fatal("expected NopTelemetrySink")
	}
	if loop.Tracer != nil {
		t.Error("expected nil tracer for empty path")
	}
}

func TestInitTelemetryWithPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/telemetry.jsonl"

	loop := &ConversationLoop{
		Config:  &Config{},
		Session: NewSession(),
	}
	loop.InitTelemetry(path)

	if loop.TelemetrySink == nil {
		t.Fatal("expected sink")
	}
	if loop.Tracer == nil {
		t.Fatal("expected tracer")
	}
	if loop.Tracer.SessionID() != loop.Session.ID {
		t.Errorf("tracer session ID = %q, want %q", loop.Tracer.SessionID(), loop.Session.ID)
	}

	// Clean up
	if c, ok := loop.TelemetrySink.(*apikit.JsonlTelemetrySink); ok {
		c.Close()
	}
}

func TestInitPluginsAddsTools(t *testing.T) {
	bp := &plugin.BuiltinPlugin{
		Meta: plugin.PluginMetadata{
			ID:      "tool-plugin@builtin",
			Name:    "tool-plugin",
			Version: "1.0.0",
			Kind:    plugin.KindBuiltin,
		},
		ToolsDef: []plugin.PluginToolDefinition{
			{
				Name:        "custom-tool",
				Description: "A custom tool from a plugin",
			},
		},
	}

	registry := plugin.NewPluginRegistry([]plugin.RegisteredPlugin{
		{Plugin: bp, Enabled: true},
	})

	loop := &ConversationLoop{Config: &Config{}}
	initialToolCount := len(loop.Tools)
	loop.InitPlugins(registry)

	if loop.PluginRegistry == nil {
		t.Fatal("expected plugin registry to be set")
	}
	if len(loop.Tools) != initialToolCount+1 {
		t.Errorf("expected %d tools, got %d", initialToolCount+1, len(loop.Tools))
	}
}
