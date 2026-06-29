package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeHookyPlugin installs an enabled plugin "hooky" under ITERION_HOME that
// contributes one PreToolUse command hook, and returns nothing (state via env).
func writeHookyPlugin(t *testing.T, home string, enabled bool) {
	t.Helper()
	dir := filepath.Join(home, "plugins", "hooky")
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: hooky\ncontributes:\n  hooks:\n    - hooks/h.json\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	frag := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo hi"}]}]}}`
	if err := os.WriteFile(filepath.Join(dir, "hooks", "h.json"), []byte(frag), 0o644); err != nil {
		t.Fatal(err)
	}
	state := "enabled:\n  hooky: false\n"
	if enabled {
		state = "enabled:\n  hooky: true\n"
	}
	if err := os.WriteFile(filepath.Join(home, "plugins.yaml"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
}

func preToolUseLen(t *testing.T, settingsPath string) int {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return 0
	}
	var s struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return len(s.Hooks["PreToolUse"])
}

func TestMergePluginHooks_InjectIdempotentRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")

	// A pre-existing USER hook must be preserved across merges.
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	userSettings := `{"hooks":{"PreToolUse":[{"matcher":"Read","hooks":[{"type":"command","command":"user-hook"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(userSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	writeHookyPlugin(t, home, true)

	// First merge: user hook + plugin hook = 2.
	if err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 1: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 2 {
		t.Fatalf("after inject: PreToolUse = %d, want 2 (user + plugin)", n)
	}

	// Second merge (resume/re-run): must NOT duplicate — still 2.
	if err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 2: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 2 {
		t.Fatalf("after re-inject: PreToolUse = %d, want 2 (idempotent)", n)
	}

	// Disable the plugin, merge again: plugin hook removed, user hook kept = 1.
	writeHookyPlugin(t, home, false)
	if err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge 3: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 1 {
		t.Fatalf("after disable: PreToolUse = %d, want 1 (user hook only)", n)
	}
}
