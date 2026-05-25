package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDesiredState_MissingFile(t *testing.T) {
	got, err := loadDesiredState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("missing file should yield zero value, got %q", got)
	}
}

func TestLoadDesiredState_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDesiredState(path); err == nil {
		t.Fatal("expected parse error on malformed JSON")
	}
}

func TestLoadDesiredState_UnknownValue(t *testing.T) {
	// A future iterion version might introduce desired:"foo"; older code
	// must tolerate the value (return "") instead of locking the
	// dispatcher idle behind an opaque error.
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(`{"desired":"foo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadDesiredState(path)
	if err != nil {
		t.Fatalf("unknown value should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("unknown value should yield zero, got %q", got)
	}
}

func TestSaveAndLoadDesiredState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatcher", "runtime.json")
	for _, want := range []DesiredState{DesiredRunning, DesiredPaused, DesiredStopped} {
		if err := saveDesiredState(path, want); err != nil {
			t.Fatalf("save(%s): %v", want, err)
		}
		got, err := loadDesiredState(path)
		if err != nil {
			t.Fatalf("load after save(%s): %v", want, err)
		}
		if got != want {
			t.Errorf("round-trip %s: got %q", want, got)
		}
	}
}

func TestSaveDesiredState_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	// Two missing parents — confirms mkdirAll, not just one level.
	path := filepath.Join(dir, "deep", "nested", "runtime.json")
	if err := saveDesiredState(path, DesiredPaused); err != nil {
		t.Fatalf("save with missing parents: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestSaveDesiredState_AtomicViaTmpRename(t *testing.T) {
	// We never want a half-flushed JSON file on disk — a crash mid-write
	// would otherwise leave loadDesiredState parsing garbage. The save
	// path writes to .tmp and renames, which is atomic on every POSIX
	// filesystem. The behavioural check here: after save, only the
	// final path exists (no leftover .tmp).
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.json")
	if err := saveDesiredState(path, DesiredRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file lingered after rename: err=%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f runtimeStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("on-disk file is not valid JSON: %v\n%s", err, raw)
	}
	if f.Desired != DesiredRunning {
		t.Errorf("on-disk Desired=%q, want running", f.Desired)
	}
}

func TestAutoStartEnabled(t *testing.T) {
	for _, c := range []struct {
		env  string
		want bool
	}{
		{"", true},
		{"1", true},
		{"true", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"No", false},
		{"off", false},
		{" off ", false}, // whitespace tolerant — a YAML/env source may pad.
	} {
		t.Setenv("ITERION_DISPATCHER_AUTOSTART", c.env)
		if got := autoStartEnabled(); got != c.want {
			t.Errorf("autoStartEnabled(env=%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestResolveBootIntent(t *testing.T) {
	// Persisted intent always wins, regardless of config or env. The
	// env var only modulates the no-persistence default.
	t.Run("persisted overrides everything", func(t *testing.T) {
		t.Setenv("ITERION_DISPATCHER_AUTOSTART", "0")
		for _, persisted := range []DesiredState{DesiredRunning, DesiredPaused, DesiredStopped} {
			if got := resolveBootIntent(persisted, true); got != persisted {
				t.Errorf("hasConfig=true persisted=%s → %s, want %s", persisted, got, persisted)
			}
			if got := resolveBootIntent(persisted, false); got != persisted {
				t.Errorf("hasConfig=false persisted=%s → %s, want %s", persisted, got, persisted)
			}
		}
	})
	t.Run("no persistence + no config → stopped", func(t *testing.T) {
		t.Setenv("ITERION_DISPATCHER_AUTOSTART", "")
		if got := resolveBootIntent("", false); got != DesiredStopped {
			t.Errorf("got %s, want stopped (no config to dispatch against)", got)
		}
	})
	t.Run("no persistence + config + autostart default → running", func(t *testing.T) {
		t.Setenv("ITERION_DISPATCHER_AUTOSTART", "")
		if got := resolveBootIntent("", true); got != DesiredRunning {
			t.Errorf("got %s, want running (first-boot auto-start)", got)
		}
	})
	t.Run("no persistence + config + autostart=0 → stopped", func(t *testing.T) {
		t.Setenv("ITERION_DISPATCHER_AUTOSTART", "0")
		if got := resolveBootIntent("", true); got != DesiredStopped {
			t.Errorf("got %s, want stopped (operator opted out via env)", got)
		}
	})
}
