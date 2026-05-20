package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// skipIfCatalogueEmpty skips a test when the embedded catalogue is
// empty — happens for `go test ./...` invocations that didn't go
// through `task` (which depends on `templates:dispatch-bots`). CI
// always populates the catalogue, so this only fires in ad-hoc local
// runs.
func skipIfCatalogueEmpty(t *testing.T) {
	t.Helper()
	entries, err := fs.ReadDir(defaultBotsFS, defaultBotsFSRoot)
	if err != nil {
		t.Fatalf("inspect embed: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			return
		}
	}
	t.Skip("dispatcher bot catalogue not populated — run `devbox run -- task templates:dispatch-bots` first")
}

func TestBuildDefaultConfig_ValidatesAndExtractsCatalogue(t *testing.T) {
	skipIfCatalogueEmpty(t)
	storeDir := t.TempDir()
	cfg, err := BuildDefaultConfig(storeDir)
	if err != nil {
		t.Fatalf("BuildDefaultConfig: %v", err)
	}
	if cfg.Tracker.Kind != "native" {
		t.Fatalf("tracker kind: %q", cfg.Tracker.Kind)
	}
	if cfg.Server.Port == 0 {
		t.Fatalf("server port should be non-zero in default mode, got %d", cfg.Server.Port)
	}
	// Workflow must point at the extracted default bundle dir.
	if !strings.HasSuffix(cfg.Workflow, filepath.Join("dispatcher", "bots", "default")) {
		t.Fatalf("workflow: %s", cfg.Workflow)
	}
	if _, err := os.Stat(filepath.Join(cfg.Workflow, "main.bot")); err != nil {
		t.Fatalf("default bot main.bot missing on disk: %v", err)
	}
	// Every assignee_workflow entry must point at an existing directory
	// with a main.bot (bundle dir) — required by bundle.Detect.
	for name, path := range cfg.AssigneeWorkflows {
		if _, err := os.Stat(filepath.Join(path, "main.bot")); err != nil {
			t.Fatalf("assignee_workflows[%s] (%s) missing main.bot: %v", name, path, err)
		}
		if _, ok := cfg.AssigneeDispatch[name]; !ok {
			t.Fatalf("assignee_workflows[%s] has no matching AssigneeDispatch entry", name)
		}
	}
}

func TestBuildDefaultConfig_PreservesUserEdits(t *testing.T) {
	skipIfCatalogueEmpty(t)
	storeDir := t.TempDir()
	// First extraction populates the catalogue.
	if _, err := BuildDefaultConfig(storeDir); err != nil {
		t.Fatalf("first BuildDefaultConfig: %v", err)
	}
	// Hand-edit one bot.
	target := filepath.Join(storeDir, "dispatcher", "bots", "default", "main.bot")
	const userMarker = "## USER-EDITED — must survive re-extraction\n"
	if err := os.WriteFile(target, []byte(userMarker), 0o644); err != nil {
		t.Fatalf("write user edit: %v", err)
	}
	// Re-extract: write-if-absent must keep the user edit.
	if _, err := BuildDefaultConfig(storeDir); err != nil {
		t.Fatalf("second BuildDefaultConfig: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read edited bot: %v", err)
	}
	if string(got) != userMarker {
		t.Fatalf("user edit overwritten — got %q want %q", got, userMarker)
	}
}

func TestDefaultAssigneeNames_Sorted(t *testing.T) {
	got := DefaultAssigneeNames()
	if len(got) == 0 {
		t.Fatal("expected at least one default assignee")
	}
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	for i := range got {
		if got[i] != sorted[i] {
			t.Fatalf("DefaultAssigneeNames not sorted at index %d: got %v", i, got)
		}
	}
}

func TestBuildDefaultConfig_RejectsEmptyStoreDir(t *testing.T) {
	if _, err := BuildDefaultConfig(""); err == nil {
		t.Fatal("expected error on empty storeDir")
	}
}
