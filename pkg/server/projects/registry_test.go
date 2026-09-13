package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFromWrongTypedFieldErrorsWithFieldName(t *testing.T) {
	cases := []struct {
		field   string
		content string
	}{
		{"recent_projects", `{"version": 1, "recent_projects": "not-an-array"}`},
		{"version", `{"version": "one"}`},
		{"current_project_id", `{"version": 1, "current_project_id": 42}`},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			path := writeConfigFile(t, tc.content)
			cfg, err := loadFrom(path)
			if err == nil {
				t.Fatalf("expected error for wrong-typed %q, got cfg %+v", tc.field, cfg)
			}
			want := `parse config field "` + tc.field + `"`
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name the field: want substring %q, got %q", want, err)
			}
		})
	}
}

func TestLoadFromValidConfig(t *testing.T) {
	dir := t.TempDir() // must exist so pruneDeadProjects keeps it
	content := `{
		"version": 1,
		"recent_projects": [
			{"id": "p1", "name": "one", "dir": ` + string(mustJSON(t, dir)) + `, "last_opened": "2026-01-02T03:04:05Z"}
		],
		"current_project_id": "p1"
	}`
	path := writeConfigFile(t, content)
	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version: want 1, got %d", cfg.Version)
	}
	if len(cfg.RecentProjects) != 1 || cfg.RecentProjects[0].ID != "p1" {
		t.Errorf("recent_projects: want [p1], got %+v", cfg.RecentProjects)
	}
	if cfg.CurrentProjectID != "p1" {
		t.Errorf("current_project_id: want p1, got %q", cfg.CurrentProjectID)
	}
}

func TestLoadFromKeepsPinnedProjectWhenRootIsUnavailable(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "unmounted")
	storeDir := t.TempDir()
	content := `{
		"version": 1,
		"recent_projects": [
			{"id": "stable", "name": "offline", "dir": ` + string(mustJSON(t, missingRoot)) + `, "store_dir": ` + string(mustJSON(t, storeDir)) + `, "last_opened": "2026-01-02T03:04:05Z"}
		],
		"current_project_id": "stable"
	}`
	cfg, err := loadFrom(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.RecentProjects) != 1 || cfg.RecentProjects[0].ID != "stable" {
		t.Fatalf("pinned unavailable project was pruned: %+v", cfg.RecentProjects)
	}
}

func TestRegisterWithStoreCanonicalizesAliasesAndPinsStore(t *testing.T) {
	root := t.TempDir()
	storeDir := t.TempDir()
	rootAlias := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatalf("symlink root: %v", err)
	}
	cfg := &Config{Version: schemaVersion}
	first, created, err := cfg.RegisterWithStore(rootAlias, storeDir, []string{filepath.Join(root, "bots")}, filepath.Join(root, ".env"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !created || first.StoreDir != storeDir || first.Dir != root {
		t.Fatalf("unexpected first registration: created=%v project=%+v", created, first)
	}
	second, created, err := cfg.RegisterWithStore(root, storeDir, nil, "")
	if err != nil {
		t.Fatalf("register alias: %v", err)
	}
	if created || second.ID != first.ID || len(cfg.RecentProjects) != 1 {
		t.Fatalf("alias minted another project: created=%v first=%+v second=%+v all=%+v", created, first, second, cfg.RecentProjects)
	}
}

func TestRegisterWithStoreRejectsSharedStoreAndMissingStore(t *testing.T) {
	cfg := &Config{Version: schemaVersion}
	storeDir := t.TempDir()
	if _, _, err := cfg.RegisterWithStore(t.TempDir(), storeDir, nil, ""); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, _, err := cfg.RegisterWithStore(t.TempDir(), storeDir, nil, ""); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("shared store error = %v", err)
	}
	if _, _, err := cfg.RegisterWithStore(t.TempDir(), filepath.Join(t.TempDir(), "missing"), nil, ""); err == nil || !strings.Contains(err.Error(), "project store") {
		t.Fatalf("missing store error = %v", err)
	}
}

func TestLoadFromUnknownKeysPassThroughExtras(t *testing.T) {
	path := writeConfigFile(t, `{
		"version": 1,
		"recent_projects": [],
		"current_project_id": "",
		"Window": {"width": 1280, "height": 800},
		"FirstRunDone": true
	}`)
	cfg, err := loadFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, key := range []string{"Window", "FirstRunDone"} {
		if _, ok := cfg.Extras[key]; !ok {
			t.Errorf("extras: missing key %q, got %v", key, cfg.Extras)
		}
	}
	// Extras survive a Save round-trip verbatim.
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := loadFrom(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var w struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.Unmarshal(reloaded.Extras["Window"], &w); err != nil {
		t.Fatalf("unmarshal round-tripped Window: %v", err)
	}
	if w.Width != 1280 || w.Height != 800 {
		t.Errorf("Window round-trip: want 1280x800, got %+v", w)
	}
}

func TestRegisterWithStoreCanClearPinnedEnvFile(t *testing.T) {
	root, storeDir := t.TempDir(), t.TempDir()
	envFile := filepath.Join(root, ".env")
	if err := os.WriteFile(envFile, []byte("PROJECT=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Version: schemaVersion}
	registered, _, err := cfg.RegisterWithStore(root, storeDir, nil, envFile)
	if err != nil {
		t.Fatal(err)
	}
	if registered.EnvFile != envFile {
		t.Fatalf("env file = %q, want %q", registered.EnvFile, envFile)
	}
	registered, _, err = cfg.RegisterWithStore(root, storeDir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if registered.EnvFile != "" {
		t.Fatalf("env file = %q, want cleared", registered.EnvFile)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
