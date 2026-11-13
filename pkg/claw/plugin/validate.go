package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// isLiteralCommand returns true if the entry is a shell command (not a path).
// Rust skips path validation for commands that don't start with "./" or "../"
// and aren't absolute paths — these are shell commands resolved via PATH.
func isLiteralCommand(entry string) bool {
	return !strings.HasPrefix(entry, "./") && !strings.HasPrefix(entry, "../") && !filepath.IsAbs(entry)
}

// ValidateManifest checks a manifest for errors.
// Returns all validation errors found (does not stop at first).
func ValidateManifest(m *PluginManifest, root string, kind PluginKind) []ValidationError {
	var errs []ValidationError

	// Required fields
	if strings.TrimSpace(m.Name) == "" {
		errs = append(errs, ValidationError{Code: "empty_field", Message: "name is required"})
	}
	if strings.TrimSpace(m.Version) == "" {
		errs = append(errs, ValidationError{Code: "empty_field", Message: "version is required"})
	}
	if strings.TrimSpace(m.Description) == "" {
		errs = append(errs, ValidationError{Code: "empty_field", Message: "description is required"})
	}

	// Duplicate permissions
	validPerms := ValidPluginPermissions()
	seenPerms := make(map[PluginPermission]bool)
	for _, p := range m.Permissions {
		if !validPerms[p] {
			errs = append(errs, ValidationError{
				Code:    "invalid_permission",
				Message: fmt.Sprintf("invalid permission: %q", string(p)),
			})
		}
		if seenPerms[p] {
			errs = append(errs, ValidationError{
				Code:    "duplicate_permission",
				Message: fmt.Sprintf("duplicate permission: %q", string(p)),
			})
		}
		seenPerms[p] = true
	}

	// Tool validation
	validToolPerms := ValidToolPermissions()
	seenTools := make(map[string]bool)
	for _, t := range m.Tools {
		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, ValidationError{
				Code:    "empty_entry_field",
				Message: "tool name is required",
			})
		}
		if strings.TrimSpace(t.Description) == "" {
			errs = append(errs, ValidationError{
				Code:    "empty_entry_field",
				Message: fmt.Sprintf("tool %q description is required", t.Name),
			})
		}
		if strings.TrimSpace(t.Command) == "" {
			errs = append(errs, ValidationError{
				Code:    "empty_entry_field",
				Message: fmt.Sprintf("tool %q command is required", t.Name),
			})
		}
		if t.Name != "" {
			if seenTools[t.Name] {
				errs = append(errs, ValidationError{
					Code:    "duplicate_entry",
					Message: fmt.Sprintf("duplicate tool name: %q", t.Name),
				})
			}
			seenTools[t.Name] = true
		}

		// inputSchema must be a JSON object
		if len(t.InputSchema) > 0 {
			trimmed := strings.TrimSpace(string(t.InputSchema))
			if !strings.HasPrefix(trimmed, "{") {
				errs = append(errs, ValidationError{
					Code:    "invalid_tool_input_schema",
					Message: fmt.Sprintf("tool %q inputSchema must be a JSON object", t.Name),
				})
			}
		}

		// requiredPermission validation
		if t.RequiredPermission != "" && !validToolPerms[t.RequiredPermission] {
			errs = append(errs, ValidationError{
				Code:    "invalid_tool_required_permission",
				Message: fmt.Sprintf("tool %q has invalid requiredPermission: %q", t.Name, string(t.RequiredPermission)),
			})
		}

		// Path existence check for bundled/external.
		// Skip for literal commands (shell commands resolved via PATH).
		if (kind == KindBundled || kind == KindExternal) && root != "" && strings.TrimSpace(t.Command) != "" && !isLiteralCommand(t.Command) {
			cmdPath := t.Command
			if !filepath.IsAbs(cmdPath) {
				cmdPath = filepath.Join(root, cmdPath)
			}
			if _, err := os.Stat(cmdPath); err != nil {
				errs = append(errs, ValidationError{
					Code:    "missing_path",
					Message: fmt.Sprintf("tool %q command path not found: %s", t.Name, cmdPath),
				})
			}
		}
	}

	// Hook and lifecycle command path validation for Bundled/External plugins.
	if (kind == KindBundled || kind == KindExternal) && root != "" {
		// Validate hook command paths.
		hookLists := map[string][]string{
			"hook PreToolUse":         m.Hooks.PreToolUse,
			"hook PostToolUse":        m.Hooks.PostToolUse,
			"hook PostToolUseFailure": m.Hooks.PostToolUseFailure,
		}
		for label, commands := range hookLists {
			for _, cmd := range commands {
				if strings.TrimSpace(cmd) == "" || isLiteralCommand(cmd) {
					continue
				}
				cmdPath := cmd
				if !filepath.IsAbs(cmdPath) {
					cmdPath = filepath.Join(root, cmdPath)
				}
				if _, err := os.Stat(cmdPath); err != nil {
					errs = append(errs, ValidationError{
						Code:    "missing_path",
						Message: fmt.Sprintf("%s command path not found: %s", label, cmdPath),
					})
				}
			}
		}

		// Validate lifecycle command paths.
		lifecycleLists := map[string][]string{
			"lifecycle Init":     m.Lifecycle.Init,
			"lifecycle Shutdown": m.Lifecycle.Shutdown,
		}
		for label, commands := range lifecycleLists {
			for _, cmd := range commands {
				if strings.TrimSpace(cmd) == "" || isLiteralCommand(cmd) {
					continue
				}
				cmdPath := cmd
				if !filepath.IsAbs(cmdPath) {
					cmdPath = filepath.Join(root, cmdPath)
				}
				if _, err := os.Stat(cmdPath); err != nil {
					errs = append(errs, ValidationError{
						Code:    "missing_path",
						Message: fmt.Sprintf("%s command path not found: %s", label, cmdPath),
					})
				}
			}
		}
	}

	// Unsupported contracts
	if len(m.RawJSON) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(m.RawJSON, &raw); err == nil {
			for _, key := range []string{"skills", "mcpServers", "agents"} {
				if _, ok := raw[key]; ok {
					errs = append(errs, ValidationError{
						Code:    "unsupported_contract",
						Message: fmt.Sprintf("unsupported manifest key: %q", key),
					})
				}
			}

			// Reject unsupported hook names (matching Rust lib.rs:1651-1660).
			if hooksRaw, ok := raw["hooks"]; ok {
				var hooksMap map[string]json.RawMessage
				if err := json.Unmarshal(hooksRaw, &hooksMap); err == nil {
					for hookName := range hooksMap {
						switch hookName {
						case "PreToolUse", "PostToolUse", "PostToolUseFailure":
							// valid
						default:
							errs = append(errs, ValidationError{
								Code:    "unsupported_contract",
								Message: fmt.Sprintf("unsupported hook name: %q; only PreToolUse, PostToolUse, PostToolUseFailure are supported", hookName),
							})
						}
					}
				}
			}

			// Reject string-array commands (Claude Code convention, not supported;
			// matching Rust lib.rs:1643-1649).
			if cmdsRaw, ok := raw["commands"]; ok {
				var cmdsArr []json.RawMessage
				if err := json.Unmarshal(cmdsRaw, &cmdsArr); err == nil {
					for _, item := range cmdsArr {
						var s string
						if json.Unmarshal(item, &s) == nil {
							errs = append(errs, ValidationError{
								Code:    "unsupported_contract",
								Message: "commands array contains string entries; only object commands are supported",
							})
							break
						}
					}
				}
			}
		}
	}

	return errs
}
