package runtime

import (
	"bytes"
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

// A malformed (but existing) user settings.json must never be destroyed by the
// merge: mergePluginHooks returns an error and leaves the file bytes intact.
func TestMergePluginHooks_MalformedSettingsRefusesRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := []byte(`{"permissions":`)
	if err := os.WriteFile(settingsPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	writeHookyPlugin(t, home, true)

	if err := mergePluginHooks(ws, nil); err == nil {
		t.Fatal("want error for malformed settings.json, got nil")
	}
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read back settings.json: %v", err)
	}
	if !bytes.Equal(got, malformed) {
		t.Fatalf("settings.json was rewritten despite parse error:\n got: %s\nwant: %s", got, malformed)
	}
}

// A settings.json containing literal `null` unmarshals into a nil map without
// error; the merge must treat it as empty (no panic, no error) and proceed.
func TestMergePluginHooks_NullSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	ws := t.TempDir()
	settingsPath := filepath.Join(ws, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte("null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeHookyPlugin(t, home, true)

	if err := mergePluginHooks(ws, nil); err != nil {
		t.Fatalf("merge over null settings: %v", err)
	}
	if n := preToolUseLen(t, settingsPath); n != 1 {
		t.Fatalf("after merge over null: PreToolUse = %d, want 1 (plugin hook)", n)
	}
}
