package supervise

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
)

// hookDrainSubcommand is the hidden iterion subcommand a raw Claude Code
// session invokes from its Stop / PostToolUse hooks to drain the
// supervisor inbox. The install marker is this literal substring.
const hookDrainSubcommand = "__claude-hook-drain"

// HookSettingsScope selects which settings file the hook is written to.
type HookSettingsScope string

const (
	// HookScopeLocal targets .claude/settings.local.json (machine-local,
	// gitignored) — the default.
	HookScopeLocal HookSettingsScope = "local"
	// HookScopeProject targets .claude/settings.json (shared/committed).
	HookScopeProject HookSettingsScope = "project"
)

// settingsPath resolves the settings file for a repo + scope.
func settingsPath(repoDir string, scope HookSettingsScope) string {
	name := "settings.local.json"
	if scope == HookScopeProject {
		name = "settings.json"
	}
	return filepath.Join(repoDir, ".claude", name)
}

// InstallHook adds the iterion drain command to the Stop and PostToolUse
// hooks in the target repo's settings file, non-destructively (unknown
// keys + pre-existing hooks are preserved). Idempotent: a second call is
// a no-op. Returns the settings path written and whether a change was
// made.
func InstallHook(repoDir string, scope HookSettingsScope) (path string, changed bool, err error) {
	path = settingsPath(repoDir, scope)
	root, err := readSettings(path)
	if err != nil {
		return path, false, err
	}
	// The command is a shell line in the target repo's settings: the
	// binary path is quoted, so a path carrying a space or a shell
	// metacharacter survives it.
	cmd := shellquote.Quote(proc.LocateIterionBinary()) + " " + hookDrainSubcommand

	hooks := asMap(root["hooks"])
	stopChanged := ensureCommandHook(hooks, "Stop", "", cmd)
	postChanged := ensureCommandHook(hooks, "PostToolUse", "*", cmd)
	if !stopChanged && !postChanged {
		return path, false, nil
	}
	root["hooks"] = hooks
	if err := writeSettings(path, root); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// UninstallHook removes every iterion drain hook from the settings file,
// pruning emptied matcher blocks and arrays, and leaves all other hooks
// untouched. Returns whether a change was made.
func UninstallHook(repoDir string, scope HookSettingsScope) (path string, changed bool, err error) {
	path = settingsPath(repoDir, scope)
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return path, false, nil
	}
	root, err := readSettings(path)
	if err != nil {
		return path, false, err
	}
	hooks := asMap(root["hooks"])
	c1 := removeCommandHook(hooks, "Stop")
	c2 := removeCommandHook(hooks, "PostToolUse")
	if !c1 && !c2 {
		return path, false, nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	if err := writeSettings(path, root); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// HookInstalled reports whether the drain hook is present under the given
// event in the settings file.
func HookInstalled(repoDir string, scope HookSettingsScope) bool {
	root, err := readSettings(settingsPath(repoDir, scope))
	if err != nil {
		return false
	}
	hooks := asMap(root["hooks"])
	return hasCommandHook(hooks, "Stop") || hasCommandHook(hooks, "PostToolUse")
}

// ---- JSON surgery helpers (preserve unknown keys) ----

func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("supervise: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("supervise: parse %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func writeSettings(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("supervise: mkdir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("supervise: marshal settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("supervise: write %s: %w", path, err)
	}
	return nil
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// ensureCommandHook appends a {matcher?, hooks:[{type:command, command}]}
// block under hooks[event] when our command isn't already present.
// Returns whether it added one.
func ensureCommandHook(hooks map[string]any, event, matcher, cmd string) bool {
	list, _ := hooks[event].([]any)
	for _, entry := range list {
		if blockHasOurCommand(entry) {
			return false // already installed
		}
	}
	block := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": cmd}},
	}
	if matcher != "" {
		block["matcher"] = matcher
	}
	hooks[event] = append(list, block)
	return true
}

// removeCommandHook drops every block under hooks[event] that contains
// our drain command, pruning the event key when it empties. Returns
// whether anything was removed.
func removeCommandHook(hooks map[string]any, event string) bool {
	list, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(list))
	removed := false
	for _, entry := range list {
		if blockHasOurCommand(entry) {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	return true
}

func hasCommandHook(hooks map[string]any, event string) bool {
	list, _ := hooks[event].([]any)
	for _, entry := range list {
		if blockHasOurCommand(entry) {
			return true
		}
	}
	return false
}

// blockHasOurCommand reports whether a hook matcher block contains a
// command hook invoking our drain subcommand.
func blockHasOurCommand(entry any) bool {
	block, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := block["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, hookDrainSubcommand) {
			return true
		}
	}
	return false
}
