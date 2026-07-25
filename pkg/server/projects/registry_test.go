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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
