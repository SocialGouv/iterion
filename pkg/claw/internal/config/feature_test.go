package config

import (
	"reflect"
	"testing"
)

func TestExtractFeatureConfigModel(t *testing.T) {
	data := []byte(`{"model": "claude-sonnet-4-20250514"}`)
	cfg := ExtractFeatureConfig(data)
	if cfg.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-sonnet-4-20250514")
	}
}

func TestExtractFeatureConfigHooks(t *testing.T) {
	data := []byte(`{"hooks": {"PreToolUse": ["cmd1", "cmd2"], "PostToolUse": ["cmd3"]}}`)
	cfg := ExtractFeatureConfig(data)
	if !reflect.DeepEqual(cfg.Hooks.PreToolUse, []string{"cmd1", "cmd2"}) {
		t.Errorf("PreToolUse = %v", cfg.Hooks.PreToolUse)
	}
	if !reflect.DeepEqual(cfg.Hooks.PostToolUse, []string{"cmd3"}) {
		t.Errorf("PostToolUse = %v", cfg.Hooks.PostToolUse)
	}
}

func TestExtractFeatureConfigPermissions(t *testing.T) {
	data := []byte(`{"permissions": {"defaultMode": "plan", "allow": ["read_file"], "deny": ["bash"]}}`)
	cfg := ExtractFeatureConfig(data)
	if cfg.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want %q", cfg.PermissionMode, "plan")
	}
	if !reflect.DeepEqual(cfg.PermissionRules.Allow, []string{"read_file"}) {
		t.Errorf("Allow = %v", cfg.PermissionRules.Allow)
	}
	if !reflect.DeepEqual(cfg.PermissionRules.Deny, []string{"bash"}) {
		t.Errorf("Deny = %v", cfg.PermissionRules.Deny)
	}
}

func TestExtractFeatureConfigLegacyPermissionMode(t *testing.T) {
	data := []byte(`{"permissionMode": "bypass"}`)
	cfg := ExtractFeatureConfig(data)
	if cfg.PermissionMode != "bypass" {
		t.Errorf("PermissionMode = %q, want %q", cfg.PermissionMode, "bypass")
	}
}

func TestExtractFeatureConfigFallbacks(t *testing.T) {
	data := []byte(`{"model": "claude-opus", "providerFallbacks": {"primary": "anthropic", "fallbacks": ["bedrock", "vertex"]}}`)
	cfg := ExtractFeatureConfig(data)
	if cfg.ProviderFallbacks.Primary != "anthropic" {
		t.Errorf("Primary = %q", cfg.ProviderFallbacks.Primary)
	}
	if !reflect.DeepEqual(cfg.ProviderFallbacks.Fallbacks, []string{"bedrock", "vertex"}) {
		t.Errorf("Fallbacks = %v", cfg.ProviderFallbacks.Fallbacks)
	}
	if cfg.ProviderFallbacks.IsEmpty() {
		t.Error("should not be empty")
	}
}

func TestExtractFeatureConfigEmpty(t *testing.T) {
	cfg := ExtractFeatureConfig([]byte(`{}`))
	if cfg.Model != "" {
		t.Errorf("Model should be empty, got %q", cfg.Model)
	}
	if !cfg.ProviderFallbacks.IsEmpty() {
		t.Error("should be empty")
	}
}

func TestExtractFeatureConfigInvalidJSON(t *testing.T) {
	cfg := ExtractFeatureConfig([]byte(`not json`))
	if cfg.Model != "" {
		t.Error("should return zero-value for invalid JSON")
	}
}

func TestExtractFeatureConfigPlugins(t *testing.T) {
	data := []byte(`{
		"plugins": {
			"enabled": {"my-plugin": true, "other": false},
			"externalDirectories": ["/path/to/plugins"],
			"installRoot": "/root",
			"maxOutputTokens": 8192
		}
	}`)
	cfg := ExtractFeatureConfig(data)
	if !cfg.Plugins.EnabledPlugins["my-plugin"] {
		t.Error("my-plugin should be enabled")
	}
	if cfg.Plugins.EnabledPlugins["other"] {
		t.Error("other should be disabled")
	}
	if cfg.Plugins.InstallRoot != "/root" {
		t.Errorf("InstallRoot = %q", cfg.Plugins.InstallRoot)
	}
	if cfg.Plugins.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d", cfg.Plugins.MaxOutputTokens)
	}
}

func TestExtractFeatureConfigAliases(t *testing.T) {
	data := []byte(`{"aliases": {"s4": "claude-sonnet-4-20250514"}}`)
	cfg := ExtractFeatureConfig(data)
	if cfg.Aliases["s4"] != "claude-sonnet-4-20250514" {
		t.Errorf("Aliases = %v", cfg.Aliases)
	}
}
