package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// pluginHooksSidecar records the exact hooks blob iterion last injected into
// <workDir>/.claude/settings.json, so a re-run or resume can remove the prior
// injection before adding the current one — keeping the merge idempotent and
// never duplicating a plugin's hooks. Lives under the same managed dir as the
// skill markers.
const pluginHooksSidecar = "plugin-hooks.json"

// mergePluginHooks idempotently merges the `hooks` settings fragments
// contributed by every enabled plugin into <workDir>/.claude/settings.json,
// where claude_code discovers them via --setting-sources project. User hooks
// already in settings.json are preserved (only iterion's own prior injection,
// tracked in a sidecar, is removed before re-injecting). Best-effort: a broken
// plugin or unreadable settings file is logged and skipped — never fails a run.
// No-op when workDir is empty or nothing is (or was) injected.
func mergePluginHooks(workDir string, logger *iterlog.Logger) error {
	if workDir == "" {
		return nil
	}
	reg, err := plugin.Load()
	if err != nil {
		if logger != nil {
			logger.Warn("runtime: load plugins for hooks merge: %v — skipping", err)
		}
		return nil
	}

	// newBlob: event -> []group, concatenated across every enabled plugin.
	newBlob := map[string][]any{}
	for _, p := range reg.Enabled() {
		frags, ferr := p.HookFragments()
		if ferr != nil {
			if logger != nil {
				logger.Warn("runtime: plugin %q hooks: %v — skipping", p.Name(), ferr)
			}
			continue
		}
		for _, frag := range frags {
			for event, v := range frag {
				if groups, ok := v.([]any); ok {
					newBlob[event] = append(newBlob[event], groups...)
				}
			}
		}
	}

	claudeDir := filepath.Join(workDir, ".claude")
	settingsPath := filepath.Join(claudeDir, "settings.json")
	sidecarPath := filepath.Join(claudeDir, bundleMirrorMarkerDir, pluginHooksSidecar)
	prevBlob := readHooksSidecar(sidecarPath)

	if len(newBlob) == 0 && len(prevBlob) == 0 {
		return nil // nothing to do, and nothing was ever injected
	}

	settings := readJSONObject(settingsPath)
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Remove our prior injection (deep-equal groups), preserving user hooks.
	for event, prevGroups := range prevBlob {
		existing, ok := hooks[event].([]any)
		if !ok {
			continue
		}
		kept := existing[:0]
		for _, g := range existing {
			if !containsGroup(prevGroups, g) {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	// Add the current injection, skipping any group already present.
	for event, groups := range newBlob {
		existing, _ := hooks[event].([]any)
		for _, g := range groups {
			if !containsGroup(existing, g) {
				existing = append(existing, g)
			}
		}
		hooks[event] = existing
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0o755); err != nil {
		return fmt.Errorf("runtime/plugin: mkdir managed dir: %w", err)
	}
	if err := writeJSON(settingsPath, settings); err != nil {
		return fmt.Errorf("runtime/plugin: write settings.json: %w", err)
	}
	if err := writeJSON(sidecarPath, newBlob); err != nil {
		return fmt.Errorf("runtime/plugin: write hooks sidecar: %w", err)
	}
	return nil
}

func containsGroup(groups []any, g any) bool {
	for _, x := range groups {
		if reflect.DeepEqual(x, g) {
			return true
		}
	}
	return false
}

func readHooksSidecar(path string) map[string][]any {
	out := map[string][]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// readJSONObject reads path as a JSON object, returning {} on any error
// (missing/invalid file) so the merge starts from a clean base.
func readJSONObject(path string) map[string]any {
	out := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
