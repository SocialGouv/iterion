package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings holds the merged configuration from all settings sources.
// Fields correspond to JSON keys in .claude/settings.json.
type Settings struct {
	Model          string   `json:"model,omitempty"`
	PermissionMode string   `json:"permissionMode,omitempty"`
	AllowedTools   []string `json:"allowedTools,omitempty"`
	BlockedTools   []string `json:"blockedTools,omitempty"`
	MaxTokens      int      `json:"maxTokens,omitempty"`
	Theme          string   `json:"theme,omitempty"`

	// EnabledPlugins maps plugin IDs to their enabled state.
	EnabledPlugins map[string]bool `json:"enabledPlugins,omitempty"`

	// Hooks holds hook command lists per event type.
	Hooks *SettingsHooks `json:"hooks,omitempty"`

	// FallbackModels configures the provider fallback chain.
	FallbackModels *SettingsFallbackModels `json:"providerFallbacks,omitempty"`

	// RawJSON preserves the original raw JSON bytes for feature extraction
	// and validation. Not serialized back — use ExtractFeatureConfig() for
	// typed access to all fields including those not in Settings struct.
	RawJSON json.RawMessage `json:"-"`
}

// SettingsHooks mirrors the hooks block in settings.json.
type SettingsHooks struct {
	PreToolUse         []string `json:"PreToolUse,omitempty"`
	PostToolUse        []string `json:"PostToolUse,omitempty"`
	PostToolUseFailure []string `json:"PostToolUseFailure,omitempty"`
}

// SettingsFallbackModels mirrors the providerFallbacks block in settings.json.
type SettingsFallbackModels struct {
	Primary   string   `json:"primary,omitempty"`
	Fallbacks []string `json:"fallbacks,omitempty"`
}

// Load returns merged settings from (in order of increasing precedence):
//  1. ~/.claude/settings.json      (user global)
//  2. .claude/settings.json        (project)
//  3. .claude/settings.local.json  (local overrides, typically gitignored)
//
// CLI flag overrides are applied by the caller after Load returns.
func Load() *Settings {
	return LoadForDir("")
}

// LoadForDir returns merged settings using cwd as the base for project-level
// config files. If cwd is empty, the current working directory is used.
// This is the Go equivalent of Rust's ConfigLoader::default_for(cwd).
func LoadForDir(cwd string) *Settings {
	s := &Settings{}

	homeDir, _ := os.UserHomeDir()

	var sources []string
	if homeDir != "" {
		sources = append(sources, filepath.Join(homeDir, ".claude", "settings.json"))
	}

	if cwd != "" {
		sources = append(sources,
			filepath.Join(cwd, ".claude", "settings.json"),
			filepath.Join(cwd, ".claude", "settings.local.json"),
		)
	} else {
		sources = append(sources,
			filepath.Join(".claude", "settings.json"),
			filepath.Join(".claude", "settings.local.json"),
		)
	}

	for _, path := range sources {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var patch Settings
		if err := json.Unmarshal(data, &patch); err != nil {
			continue
		}
		patch.RawJSON = data
		merge(s, &patch)

		// Validate and emit warnings to stderr (non-fatal).
		vr := ValidateSettingsJSON(data, path)
		if vr.HasWarnings() {
			for _, w := range vr.Warnings {
				fmt.Fprintf(os.Stderr, "config warning: %s\n", w)
			}
		}
		if vr.HasErrors() {
			for _, e := range vr.Errors {
				fmt.Fprintf(os.Stderr, "config error: %s\n", e)
			}
		}
	}

	return s
}

// merge applies non-zero fields from src into dst.
func merge(dst, src *Settings) {
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.PermissionMode != "" {
		dst.PermissionMode = src.PermissionMode
	}
	if len(src.AllowedTools) > 0 {
		dst.AllowedTools = src.AllowedTools
	}
	if len(src.BlockedTools) > 0 {
		dst.BlockedTools = src.BlockedTools
	}
	if src.MaxTokens != 0 {
		dst.MaxTokens = src.MaxTokens
	}
	if src.Theme != "" {
		dst.Theme = src.Theme
	}
	if src.EnabledPlugins != nil {
		dst.EnabledPlugins = src.EnabledPlugins
	}
	if src.Hooks != nil {
		dst.Hooks = src.Hooks
	}
	if src.FallbackModels != nil {
		dst.FallbackModels = src.FallbackModels
	}
	if len(src.RawJSON) > 0 {
		dst.RawJSON = src.RawJSON
	}
}

// WriteProject writes settings to .claude/settings.json, merging with any
// existing content to preserve unmanaged fields (e.g. mcpServers, rules).
func WriteProject(s *Settings) error {
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		return NewConfigIOError("create_dir", ".claude", err)
	}

	// Read existing settings first to preserve unmanaged fields.
	existing := map[string]any{}
	if data, err := os.ReadFile(filepath.Join(".claude", "settings.json")); err == nil {
		_ = json.Unmarshal(data, &existing)
	}

	// Overlay our managed fields.
	if s.Model != "" {
		existing["model"] = s.Model
	}
	if s.PermissionMode != "" {
		existing["permissionMode"] = s.PermissionMode
	}
	if s.AllowedTools != nil {
		existing["allowedTools"] = s.AllowedTools
	}
	if s.BlockedTools != nil {
		existing["blockedTools"] = s.BlockedTools
	}
	if s.MaxTokens != 0 {
		existing["maxTokens"] = s.MaxTokens
	}
	if s.Theme != "" {
		existing["theme"] = s.Theme
	}

	path := filepath.Join(".claude", "settings.json")
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return NewConfigJSONError("marshal", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return NewConfigIOError("write", path, err)
	}
	return nil
}

// InitProject creates .claude/settings.json with default values.
// Returns os.ErrExist if the file already exists.
func InitProject(model string) error {
	path := filepath.Join(".claude", "settings.json")
	if _, err := os.Stat(path); err == nil {
		return NewConfigError(ConfigErrInvalidConfig, "settings file already exists: "+path)
	}
	if model == "" {
		model = "claude-sonnet-4-20250514"
	}
	defaults := &Settings{
		Model:          model,
		PermissionMode: "default",
		AllowedTools:   []string{},
		BlockedTools:   []string{},
	}
	return WriteProject(defaults)
}
