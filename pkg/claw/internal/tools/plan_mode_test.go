package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPlanMode(t *testing.T) {
	t.Run("enter plan mode sets active true", func(t *testing.T) {
		active := false
		out, err := ExecuteEnterPlanMode(&active, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !active {
			t.Fatal("expected active to be true")
		}
		if !strings.Contains(out, `"changed": true`) {
			t.Fatalf("expected changed=true in output, got %s", out)
		}
	})

	t.Run("enter plan mode when already active", func(t *testing.T) {
		active := true
		out, err := ExecuteEnterPlanMode(&active, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, `"changed": false`) {
			t.Fatalf("expected changed=false in output, got %s", out)
		}
	})

	t.Run("exit plan mode sets active false", func(t *testing.T) {
		active := true
		out, err := ExecuteExitPlanMode(&active, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if active {
			t.Fatal("expected active to be false")
		}
		// In-memory exit with active=true returns changed=true.
		if !strings.Contains(out, `"changed": true`) {
			t.Fatalf("expected changed=true in output, got %s", out)
		}
	})

	t.Run("exit plan mode when not active", func(t *testing.T) {
		active := false
		out, err := ExecuteExitPlanMode(&active, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, `"changed": false`) {
			t.Fatalf("expected changed=false in output, got %s", out)
		}
	})

	t.Run("enter nil pointer returns error", func(t *testing.T) {
		_, err := ExecuteEnterPlanMode(nil, "")
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("expected nil pointer error, got %v", err)
		}
	})

	t.Run("exit nil pointer returns error", func(t *testing.T) {
		_, err := ExecuteExitPlanMode(nil, "")
		if err == nil || !strings.Contains(err.Error(), "not available") {
			t.Fatalf("expected nil pointer error, got %v", err)
		}
	})
}

func TestPlanModePersistence(t *testing.T) {
	t.Run("persist and load round-trip", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		// Enter plan mode with persistence.
		_, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}
		if !active {
			t.Fatal("expected active to be true")
		}

		// Verify state file exists.
		statePath := filepath.Join(dir, "tool-state", "plan-mode.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("failed to read state file: %v", err)
		}
		var state map[string]any
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("failed to parse state: %v", err)
		}
		// State file now uses planModeState format with hadLocalOverride.
		if _, ok := state["hadLocalOverride"]; !ok {
			t.Error("expected hadLocalOverride in state file")
		}
	})

	t.Run("exit clears persisted state", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		// Enter then exit.
		ExecuteEnterPlanMode(&active, dir)
		ExecuteExitPlanMode(&active, dir)

		// State file should be removed.
		statePath := filepath.Join(dir, "tool-state", "plan-mode.json")
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Error("expected state file to be removed after exit")
		}
	})

	t.Run("load from empty dir returns false", func(t *testing.T) {
		dir := t.TempDir()
		loaded, err := LoadPersistedPlanMode(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if loaded {
			t.Error("expected false from empty dir")
		}
	})

	t.Run("load from empty string returns false", func(t *testing.T) {
		loaded, err := LoadPersistedPlanMode("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if loaded {
			t.Error("expected false from empty string")
		}
	})
}

func TestExitPlanMode_ManagedFlag_DerivedFromRemove(t *testing.T) {
	t.Run("persisted then exit yields managed=false (exit always sets managed=false)", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		// Enter plan mode with persistence.
		_, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		// Exit — matches Rust behavior: exit always sets managed=false.
		out, err := ExecuteExitPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("exit: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}
		// In Rust, exit always returns managed=false.
		if parsed["managed"] != false {
			t.Errorf("expected managed=false (Rust parity), got %v", parsed["managed"])
		}
	})

	t.Run("exit without persisting yields managed=false", func(t *testing.T) {
		dir := t.TempDir()
		active := true // Set active without persisting.

		out, err := ExecuteExitPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("exit: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if parsed["managed"] != false {
			t.Errorf("expected managed=false, got %v", parsed["managed"])
		}
	})

	t.Run("double exit yields managed=false on second call", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		// Enter with persistence, then exit twice.
		ExecuteEnterPlanMode(&active, dir)

		// First exit.
		ExecuteExitPlanMode(&active, dir)

		// Second exit — no state file.
		active = true
		out, err := ExecuteExitPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("second exit: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if parsed["managed"] != false {
			t.Errorf("expected managed=false on second exit, got %v", parsed["managed"])
		}
	})
}

// TestPlanModeEnrichedFields verifies that the 4 new fields are present
// in plan mode responses, matching Rust's PlanModeOutput structure.
func TestPlanModeEnrichedFields(t *testing.T) {
	t.Run("enter includes settingsPath and statePath", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("parse: %v", err)
		}

		sp, ok := parsed["settingsPath"].(string)
		if !ok || sp == "" {
			t.Error("expected settingsPath to be a non-empty string")
		}
		stp, ok := parsed["statePath"].(string)
		if !ok || stp == "" {
			t.Error("expected statePath to be a non-empty string")
		}

		// settingsPath should point to settings.local.json.
		if !strings.HasSuffix(sp, "settings.local.json") {
			t.Errorf("settingsPath = %s, want suffix settings.local.json", sp)
		}
		// statePath should point to plan-mode.json.
		if !strings.HasSuffix(stp, "plan-mode.json") {
			t.Errorf("statePath = %s, want suffix plan-mode.json", stp)
		}
	})

	t.Run("enter from empty state has null previousLocalMode", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		// previousLocalMode should be present as null (typed struct always emits all keys).
		if parsed["previousLocalMode"] != nil {
			t.Errorf("expected previousLocalMode to be null, got %v", parsed["previousLocalMode"])
		}

		// currentLocalMode should be "plan" after entering.
		if parsed["currentLocalMode"] != "plan" {
			t.Errorf("currentLocalMode = %v, want 'plan'", parsed["currentLocalMode"])
		}
	})

	t.Run("enter with existing local override preserves previousLocalMode", func(t *testing.T) {
		dir := t.TempDir()

		// Pre-set a local override in settings.local.json.
		settingsPath := filepath.Join(dir, "settings.local.json")
		settingsData := `{"permissions": {"defaultMode": "acceptEdits"}}`
		os.WriteFile(settingsPath, []byte(settingsData), 0o644)

		active := false
		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		if parsed["previousLocalMode"] != "acceptEdits" {
			t.Errorf("previousLocalMode = %v, want 'acceptEdits'", parsed["previousLocalMode"])
		}
		if parsed["currentLocalMode"] != "plan" {
			t.Errorf("currentLocalMode = %v, want 'plan'", parsed["currentLocalMode"])
		}
	})

	t.Run("exit restores previousLocalMode", func(t *testing.T) {
		dir := t.TempDir()

		// Pre-set a local override.
		settingsPath := filepath.Join(dir, "settings.local.json")
		settingsData := `{"permissions": {"defaultMode": "acceptEdits"}}`
		os.WriteFile(settingsPath, []byte(settingsData), 0o644)

		// Enter (saves "acceptEdits" as previous), then exit (restores).
		active := false
		ExecuteEnterPlanMode(&active, dir)

		out, err := ExecuteExitPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("exit: %v", err)
		}

		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		if parsed["previousLocalMode"] != "acceptEdits" {
			t.Errorf("previousLocalMode = %v, want 'acceptEdits'", parsed["previousLocalMode"])
		}
		if parsed["currentLocalMode"] != "acceptEdits" {
			t.Errorf("currentLocalMode = %v, want 'acceptEdits' (restored)", parsed["currentLocalMode"])
		}
		if parsed["changed"] != true {
			t.Errorf("changed = %v, want true", parsed["changed"])
		}
	})

	t.Run("exit from empty state has null previousLocalMode", func(t *testing.T) {
		dir := t.TempDir()

		// Enter from empty state (no settings.local.json).
		active := false
		ExecuteEnterPlanMode(&active, dir)

		out, err := ExecuteExitPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("exit: %v", err)
		}

		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		// previousLocalMode should be null (was nil before entering).
		if parsed["previousLocalMode"] != nil {
			t.Errorf("expected previousLocalMode to be null, got %v", parsed["previousLocalMode"])
		}

		// currentLocalMode should be null (defaultMode was removed).
		if parsed["currentLocalMode"] != nil {
			t.Errorf("expected currentLocalMode to be null after removing default, got %v", parsed["currentLocalMode"])
		}
	})

	t.Run("gracefully handles missing settings.local.json", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		// Should work without error even though settings.local.json didn't exist.
		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		if parsed["success"] != true {
			t.Error("expected success=true")
		}
	})
}

// TestPlanModeOutputShape verifies that the typed planModeOutput struct always
// serializes all 10 keys, matching Rust's #[derive(Serialize)] behavior.
func TestPlanModeOutputShape(t *testing.T) {
	t.Run("output always contains all 10 keys", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("parse: %v", err)
		}

		expectedKeys := []string{
			"success", "operation", "changed", "active", "managed", "message",
			"settingsPath", "statePath", "previousLocalMode", "currentLocalMode",
		}
		for _, key := range expectedKeys {
			if _, ok := raw[key]; !ok {
				t.Errorf("missing key %q in output JSON", key)
			}
		}

		// Verify exact key count — no extra keys.
		if len(raw) != len(expectedKeys) {
			t.Errorf("expected %d keys, got %d", len(expectedKeys), len(raw))
		}
	})

	t.Run("in-memory path (no stateDir) also contains all 10 keys", func(t *testing.T) {
		active := false

		out, err := ExecuteEnterPlanMode(&active, "")
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatalf("parse: %v", err)
		}

		expectedKeys := []string{
			"success", "operation", "changed", "active", "managed", "message",
			"settingsPath", "statePath", "previousLocalMode", "currentLocalMode",
		}
		for _, key := range expectedKeys {
			if _, ok := raw[key]; !ok {
				t.Errorf("missing key %q in output JSON", key)
			}
		}
	})

	t.Run("settingsPath/statePath are empty string when no stateDir", func(t *testing.T) {
		active := false

		out, err := ExecuteEnterPlanMode(&active, "")
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var parsed map[string]any
		json.Unmarshal([]byte(out), &parsed)

		if parsed["settingsPath"] != "" {
			t.Errorf("settingsPath = %v, want empty string", parsed["settingsPath"])
		}
		if parsed["statePath"] != "" {
			t.Errorf("statePath = %v, want empty string", parsed["statePath"])
		}
	})

	t.Run("previousLocalMode/currentLocalMode are null when unset", func(t *testing.T) {
		active := false

		out, err := ExecuteEnterPlanMode(&active, "")
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		var raw map[string]json.RawMessage
		json.Unmarshal([]byte(out), &raw)

		// null in JSON is the literal string "null".
		if string(raw["previousLocalMode"]) != "null" {
			t.Errorf("previousLocalMode = %s, want null", string(raw["previousLocalMode"]))
		}
	})

	t.Run("populated previousLocalMode round-trips correctly", func(t *testing.T) {
		dir := t.TempDir()

		// Pre-set a local override.
		settingsPath := filepath.Join(dir, "settings.local.json")
		settingsData := `{"permissions": {"defaultMode": "acceptEdits"}}`
		os.WriteFile(settingsPath, []byte(settingsData), 0o644)

		active := false
		out, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		// Verify the output round-trips through planModeOutput struct.
		var roundTripped planModeOutput
		if err := json.Unmarshal([]byte(out), &roundTripped); err != nil {
			t.Fatalf("failed to unmarshal into planModeOutput: %v", err)
		}
		if roundTripped.PreviousLocalMode == nil {
			t.Fatal("expected previousLocalMode to be non-nil")
		}
		var prevVal string
		json.Unmarshal(*roundTripped.PreviousLocalMode, &prevVal)
		if prevVal != "acceptEdits" {
			t.Errorf("round-tripped previousLocalMode = %q, want 'acceptEdits'", prevVal)
		}
	})
}

// TestReadPlanModeStateFile verifies the (nil, nil) / (nil, error) / (*state, nil)
// return semantics that match Rust's Result<Option<PlanModeState>>.
func TestReadPlanModeStateFile(t *testing.T) {
	t.Run("valid file returns state and nil error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		os.WriteFile(path, []byte(`{"hadLocalOverride":true,"previousLocalMode":"\"acceptEdits\""}`), 0o644)

		state, err := readPlanModeStateFile(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state == nil {
			t.Fatal("expected non-nil state")
		}
		if !state.HadLocalOverride {
			t.Error("expected HadLocalOverride to be true")
		}
	})

	t.Run("nonexistent file returns nil nil", func(t *testing.T) {
		state, err := readPlanModeStateFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
		if err != nil {
			t.Fatalf("expected nil error for missing file, got: %v", err)
		}
		if state != nil {
			t.Fatal("expected nil state for missing file")
		}
	})

	t.Run("empty path returns nil nil", func(t *testing.T) {
		state, err := readPlanModeStateFile("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state != nil {
			t.Fatal("expected nil state for empty path")
		}
	})

	t.Run("empty file returns nil nil (Rust parity)", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		os.WriteFile(path, []byte("   \n"), 0o644)

		state, err := readPlanModeStateFile(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state != nil {
			t.Fatal("expected nil state for empty file")
		}
	})

	t.Run("corrupt JSON returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		os.WriteFile(path, []byte(`{not valid json`), 0o644)

		state, err := readPlanModeStateFile(path)
		if err == nil {
			t.Fatal("expected error for corrupt JSON")
		}
		if state != nil {
			t.Fatal("expected nil state for corrupt JSON")
		}
		if !strings.Contains(err.Error(), "parse plan mode state") {
			t.Errorf("error should mention parse, got: %v", err)
		}
	})

	t.Run("unreadable file returns permission error (not nil)", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("permission-based tests are unreliable on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		os.WriteFile(path, []byte(`{"hadLocalOverride":false}`), 0o644)
		os.Chmod(path, 0o000)
		defer os.Chmod(path, 0o644)

		state, err := readPlanModeStateFile(path)
		if err == nil {
			t.Fatal("expected error for unreadable file, got nil")
		}
		if state != nil {
			t.Fatal("expected nil state for unreadable file")
		}
		if !strings.Contains(err.Error(), "read plan mode state") {
			t.Errorf("error should mention read plan mode state, got: %v", err)
		}
	})
}

// TestPlanModeClearBeforeWrite verifies that ExecuteEnterPlanMode clears stale
// state before writing new state, matching Rust's two-step clear+write sequence.
func TestPlanModeClearBeforeWrite(t *testing.T) {
	t.Run("enter with pre-existing state file clears before writing", func(t *testing.T) {
		dir := t.TempDir()
		active := false

		// First enter creates state.
		_, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("first enter: %v", err)
		}

		statePath := filepath.Join(dir, "tool-state", "plan-mode.json")
		data1, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state after first enter: %v", err)
		}

		// Tamper with settings to force a second enter to go through the write path.
		settingsPath := filepath.Join(dir, "settings.local.json")
		os.WriteFile(settingsPath, []byte(`{"permissions":{"defaultMode":"acceptEdits"}}`), 0o644)
		active = false

		// Second enter should clear then write.
		_, err = ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("second enter: %v", err)
		}

		data2, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state after second enter: %v", err)
		}

		// The state files should differ because previousLocalMode changed.
		if string(data1) == string(data2) {
			t.Error("expected state file to be rewritten (clear+write), but content is identical")
		}
	})
}

// TestLoadPersistedPlanModeErrorPropagation verifies that LoadPersistedPlanMode
// propagates filesystem errors instead of silently returning false.
func TestLoadPersistedPlanModeErrorPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based tests are unreliable on Windows")
	}

	t.Run("propagates permission errors", func(t *testing.T) {
		dir := t.TempDir()

		// Create a state file, then make it unreadable.
		stateDir := filepath.Join(dir, "tool-state")
		os.MkdirAll(stateDir, 0o755)
		statePath := filepath.Join(stateDir, "plan-mode.json")
		os.WriteFile(statePath, []byte(`{"hadLocalOverride":false}`), 0o644)
		os.Chmod(statePath, 0o000)
		defer os.Chmod(statePath, 0o644)

		loaded, err := LoadPersistedPlanMode(dir)
		if err == nil {
			t.Fatal("expected error for unreadable state file, got nil")
		}
		if loaded {
			t.Error("expected false when error occurs")
		}
		if !strings.Contains(err.Error(), "load persisted plan mode") {
			t.Errorf("error should mention load persisted plan mode, got: %v", err)
		}
	})

	t.Run("nonexistent state file returns false nil", func(t *testing.T) {
		dir := t.TempDir()
		loaded, err := LoadPersistedPlanMode(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if loaded {
			t.Error("expected false for nonexistent state")
		}
	})
}

// TestPlanModeErrorPropagation verifies that filesystem errors are properly
// propagated rather than silently swallowed.
func TestPlanModeErrorPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based tests are unreliable on Windows")
	}

	t.Run("writePlanModeStateFile to read-only dir returns error", func(t *testing.T) {
		dir := t.TempDir()
		readOnlyDir := filepath.Join(dir, "readonly")
		os.Mkdir(readOnlyDir, 0o755)
		// Create the target path inside read-only dir.
		targetPath := filepath.Join(readOnlyDir, "subdir", "state.json")
		// Make the dir read-only so MkdirAll fails.
		os.Chmod(readOnlyDir, 0o444)
		defer os.Chmod(readOnlyDir, 0o755)

		state := &planModeState{HadLocalOverride: false}
		err := writePlanModeStateFile(targetPath, state)
		if err == nil {
			t.Fatal("expected error writing to read-only dir")
		}
	})

	t.Run("setNestedValue to unwritable path returns error", func(t *testing.T) {
		dir := t.TempDir()
		readOnlyDir := filepath.Join(dir, "readonly")
		os.Mkdir(readOnlyDir, 0o755)
		targetPath := filepath.Join(readOnlyDir, "subdir", "settings.json")
		os.Chmod(readOnlyDir, 0o444)
		defer os.Chmod(readOnlyDir, 0o755)

		err := setNestedValue(targetPath, []string{"key"}, "value")
		if err == nil {
			t.Fatal("expected error writing to read-only dir")
		}
	})

	t.Run("clearPlanModeStateFile on nonexistent file returns nil", func(t *testing.T) {
		err := clearPlanModeStateFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
		if err != nil {
			t.Fatalf("expected nil for nonexistent file, got %v", err)
		}
	})

	t.Run("clearPlanModeStateFile on read-only dir returns error", func(t *testing.T) {
		dir := t.TempDir()
		// Create a file inside a directory, then make the directory read-only.
		targetPath := filepath.Join(dir, "state.json")
		os.WriteFile(targetPath, []byte("{}"), 0o644)
		os.Chmod(dir, 0o444)
		defer os.Chmod(dir, 0o755)

		err := clearPlanModeStateFile(targetPath)
		if err == nil {
			t.Fatal("expected error removing file from read-only dir")
		}
	})

	t.Run("ExecuteEnterPlanMode propagates write errors", func(t *testing.T) {
		dir := t.TempDir()
		// Make dir read-only so file creation fails.
		os.Chmod(dir, 0o444)
		defer os.Chmod(dir, 0o755)

		active := false
		_, err := ExecuteEnterPlanMode(&active, dir)
		if err == nil {
			t.Fatal("expected error from enter with read-only stateDir")
		}
		if !strings.Contains(err.Error(), "enter_plan_mode") {
			t.Errorf("error should mention enter_plan_mode, got: %v", err)
		}
	})

	t.Run("ExecuteExitPlanMode propagates write errors", func(t *testing.T) {
		dir := t.TempDir()

		// First enter successfully.
		active := false
		_, err := ExecuteEnterPlanMode(&active, dir)
		if err != nil {
			t.Fatalf("enter: %v", err)
		}

		// Make only the settings file read-only (not the dir itself, so the
		// state file can still be read — the dir needs execute permission).
		// The exit path will successfully read the state file, then fail
		// when trying to write the settings file.
		settingsPath := filepath.Join(dir, "settings.local.json")
		os.Chmod(settingsPath, 0o444)
		defer os.Chmod(settingsPath, 0o644)

		_, err = ExecuteExitPlanMode(&active, dir)
		if err == nil {
			t.Fatal("expected error from exit with read-only settings file")
		}
		if !strings.Contains(err.Error(), "exit_plan_mode") {
			t.Errorf("error should mention exit_plan_mode, got: %v", err)
		}
	})
}
