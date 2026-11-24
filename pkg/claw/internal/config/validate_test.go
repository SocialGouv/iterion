package config

import (
	"os"
	"strings"
	"testing"
)

func TestValidateSettingsJSONValid(t *testing.T) {
	data := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"maxTokens": 200000,
		"theme": "dark"
	}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if !result.IsClean() {
		t.Errorf("expected clean result, got errors=%d warnings=%d", len(result.Errors), len(result.Warnings))
		t.Log(FormatDiagnostics(&result))
	}
}

func TestValidateSettingsJSONEmpty(t *testing.T) {
	data := []byte(`{}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if !result.IsClean() {
		t.Errorf("expected clean result for empty config, got errors=%d warnings=%d", len(result.Errors), len(result.Warnings))
	}
}

func TestValidateSettingsJSONUnknownKeyWithSuggestion(t *testing.T) {
	data := []byte(`{"modle": "claude-sonnet-4-20250514"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for unknown key 'modle'")
	}
	found := false
	for _, e := range result.Errors {
		ukd, ok := e.Kind.(UnknownKeyDiag)
		if ok && ukd.Suggestion == "model" {
			found = true
		}
	}
	if !found {
		t.Error("expected suggestion 'model' for typo 'modle'")
	}
}

func TestValidateSettingsJSONUnknownKeyShortNoSuggestion(t *testing.T) {
	// Short keys (< 4 chars) should NOT get suggestions to avoid false positives.
	data := []byte(`{"foo": "bar"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for unknown key 'foo'")
	}
	for _, e := range result.Errors {
		ukd, ok := e.Kind.(UnknownKeyDiag)
		if ok && ukd.Suggestion != "" {
			t.Errorf("short key should not get suggestion, got %q", ukd.Suggestion)
		}
	}
}

func TestValidateSettingsJSONWrongType(t *testing.T) {
	data := []byte(`{"maxTokens": "abc"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for wrong type maxTokens")
	}
	found := false
	for _, e := range result.Errors {
		if _, ok := e.Kind.(WrongTypeDiag); ok && e.Field == "maxTokens" {
			found = true
		}
	}
	if !found {
		t.Error("expected WrongTypeDiag for maxTokens")
	}
}

func TestValidateSettingsJSONDeprecated(t *testing.T) {
	data := []byte(`{"permissionMode": "default"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	found := false
	for _, w := range result.Warnings {
		if dd, ok := w.Kind.(DeprecatedDiag); ok && dd.Replacement == "permissions.defaultMode" {
			found = true
		}
	}
	if !found {
		t.Error("expected deprecated warning for permissionMode")
	}
}

func TestValidateSettingsJSONInvalidJSON(t *testing.T) {
	data := []byte(`not json`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateSettingsJSONNestedHooksUnknownKey(t *testing.T) {
	data := []byte(`{"hooks": {"PreToolUse": ["cmd1"], "BadHook": ["cmd2"]}}`)
	result := ValidateSettingsJSON(data, "settings.json")
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e.Field, "BadHook") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for unknown hook key 'BadHook'")
	}
}

func TestValidateSettingsJSONHooksWrongType(t *testing.T) {
	data := []byte(`{"hooks": "not an object"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for hooks wrong type")
	}
}

func TestLevenshteinDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"model", "modle", 2},
		{"theme", "theem", 2},
		{"cat", "car", 1},
	}
	for _, tt := range tests {
		got := levenshteinDistance(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("levenshteinDistance(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestFormatDiagnostics(t *testing.T) {
	result := &ValidationResult{
		Errors: []ConfigDiagnostic{
			{Path: "s.json", Field: "x", Kind: WrongTypeDiag{Expected: "string", Got: "number"}},
		},
		Warnings: []ConfigDiagnostic{
			{Path: "s.json", Field: "y", Kind: UnknownKeyDiag{Suggestion: "z"}},
		},
	}
	out := FormatDiagnostics(result)
	if !strings.Contains(out, "error:") || !strings.Contains(out, "warning:") {
		t.Errorf("unexpected format: %s", out)
	}
}

func TestValidateSettingsJSONNestedPluginsUnknownKey(t *testing.T) {
	data := []byte(`{"plugins": {"enabled": {}, "badKey": "x"}}`)
	result := ValidateSettingsJSON(data, "settings.json")
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e.Field, "plugins.badKey") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for unknown plugins key 'badKey'")
	}
}

func TestValidateSettingsJSONNestedSandboxUnknownKey(t *testing.T) {
	data := []byte(`{"sandbox": {"enabled": true, "badKey": "x"}}`)
	result := ValidateSettingsJSON(data, "settings.json")
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e.Field, "sandbox.badKey") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for unknown sandbox key 'badKey'")
	}
}

func TestValidateSettingsJSONNestedOAuthUnknownKey(t *testing.T) {
	data := []byte(`{"oauth": {"clientId": "abc", "badKey": "x"}}`)
	result := ValidateSettingsJSON(data, "settings.json")
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e.Field, "oauth.badKey") {
			found = true
		}
	}
	if !found {
		t.Error("expected error for unknown oauth key 'badKey'")
	}
}

func TestValidateSettingsJSONCaseInsensitiveSuggestion(t *testing.T) {
	data := []byte(`{"Model": "claude-sonnet-4-20250514"}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Errors) == 0 {
		t.Fatal("expected error for unknown key 'Model'")
	}
	found := false
	for _, e := range result.Errors {
		ukd, ok := e.Kind.(UnknownKeyDiag)
		if ok && ukd.Suggestion == "model" {
			found = true
		}
	}
	if !found {
		t.Error("expected case-insensitive suggestion 'model' for 'Model'")
	}
}

func TestValidateSettingsJSONAllSections(t *testing.T) {
	data := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"hooks": {"PreToolUse": ["cmd"]},
		"permissions": {"defaultMode": "default", "allow": [], "deny": [], "ask": []},
		"plugins": {"enabled": {}, "externalDirectories": [], "installRoot": "/p", "registryPath": "/r", "bundledRoot": "/b", "maxOutputTokens": 1000},
		"sandbox": {"enabled": true, "namespaceRestrictions": false, "networkIsolation": true, "filesystemMode": "full", "allowedMounts": []},
		"oauth": {"clientId": "abc", "authorizeUrl": "https://x", "tokenUrl": "https://y", "callbackPort": 8080, "scopes": []}
	}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if !result.IsClean() {
		t.Errorf("expected clean result for valid config with all sections, got:\n%s", FormatDiagnostics(&result))
	}
}

func TestValidateSettingsJSONNonJSONFormat(t *testing.T) {
	data := []byte(`not json`)
	result := ValidateSettingsJSON(data, "settings.json")
	if !result.HasErrors() {
		t.Fatal("expected error for non-JSON input")
	}
}

func TestCheckUnsupportedFormatYAML(t *testing.T) {
	result := checkUnsupportedFormat([]byte("---\nmodel: claude"))
	if result == "" {
		t.Error("expected YAML detection")
	}
}

func TestCheckUnsupportedFormatJSON(t *testing.T) {
	result := checkUnsupportedFormat([]byte(`{"model": "x"}`))
	if result != "" {
		t.Errorf("expected no detection for JSON, got %q", result)
	}
}

func TestValidateConfigFileNonExistent(t *testing.T) {
	result := ValidateConfigFile("/nonexistent/path/settings.json")
	if !result.HasErrors() {
		t.Fatal("expected error for non-existent file")
	}
}

func TestValidateConfigFileTmpValid(t *testing.T) {
	tmpDir := t.TempDir()
	path := tmpDir + "/settings.json"
	if err := os.WriteFile(path, []byte(`{"model": "test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	result := ValidateConfigFile(path)
	if !result.IsClean() {
		t.Errorf("expected clean result, got:\n%s", FormatDiagnostics(&result))
	}
}

func TestValidateSettingsJSONMultipleDeprecated(t *testing.T) {
	data := []byte(`{"permissionMode": "default", "enabledPlugins": {}}`)
	result := ValidateSettingsJSON(data, "settings.json")
	if len(result.Warnings) < 2 {
		t.Errorf("expected at least 2 deprecation warnings, got %d", len(result.Warnings))
	}
}

func TestFormatDiagnosticsMultiLine(t *testing.T) {
	result := &ValidationResult{
		Errors: []ConfigDiagnostic{
			{Path: "a.json", Field: "x", Kind: WrongTypeDiag{Expected: "string", Got: "number"}},
			{Path: "a.json", Field: "y", Kind: UnknownKeyDiag{}},
		},
		Warnings: []ConfigDiagnostic{
			{Path: "a.json", Field: "z", Kind: DeprecatedDiag{Replacement: "new_z"}},
		},
	}
	out := FormatDiagnostics(result)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Errorf("expected 3 diagnostic lines, got %d: %s", len(lines), out)
	}
}

func TestValidateMCPServerEntries(t *testing.T) {
	data := []byte(`{
		"mcpServers": {
			"myserver": {
				"transport": "stdio",
				"command": "node",
				"args": ["server.js"],
				"unknownField": true
			}
		}
	}`)
	result := ValidateSettingsJSON(data, "test.json")
	if len(result.Errors) == 0 {
		t.Error("expected error for unknown MCP server entry field")
	}
	foundUnknown := false
	for _, d := range result.Errors {
		if strings.Contains(d.Field, "unknownField") {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Error("expected unknown key diagnostic for 'unknownField' in MCP server entry")
	}
}

func TestValidateMCPServerValidEntries(t *testing.T) {
	data := []byte(`{
		"mcpServers": {
			"myserver": {
				"transport": "stdio",
				"command": "node",
				"args": ["server.js"]
			}
		}
	}`)
	result := ValidateSettingsJSON(data, "test.json")
	for _, d := range result.Errors {
		if strings.Contains(d.Field, "mcpServers.myserver") {
			t.Errorf("unexpected error for valid MCP server entry: %s", d)
		}
	}
}
