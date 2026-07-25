package server

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestEditorRunRows_ScopedProjection proves the config_editor recent-runs view
// (1) keeps only the share's category, (2) exposes ONLY status + timestamps
// (never run id / inputs / error — the role has no run-console access), and
// (3) honours the limit.
func TestEditorRunRows_ScopedProjection(t *testing.T) {
	fin := time.Date(2026, 7, 20, 8, 5, 0, 0, time.UTC)
	recs := []*store.Run{
		{ID: "r1", BundleName: "feed-watch", Status: store.RunStatusFinished, CreatedAt: fin.Add(-time.Hour), FinishedAt: &fin, Error: "should-not-leak", Inputs: map[string]any{"category": "a11y", "secret": "x"}},
		{ID: "r2", BundleName: "feed-watch", Status: store.RunStatusFailed, CreatedAt: fin.Add(-2 * time.Hour), Inputs: map[string]any{"category": "design-systems"}},
		{ID: "r3", BundleName: "feed-watch", Status: store.RunStatusFinished, CreatedAt: fin.Add(-3 * time.Hour), Inputs: map[string]any{"category": "a11y"}},
		nil, // a corrupt/nil record must be skipped, not panic
		{ID: "r4", BundleName: "feed-watch", Status: store.RunStatusFinished, CreatedAt: fin.Add(-4 * time.Hour), Inputs: map[string]any{"category": "a11y"}},
	}

	// category scoping: only the a11y runs, capped at limit=2.
	rows := editorRunRows(recs, "a11y", 2)
	if len(rows) != 2 {
		t.Fatalf("want 2 a11y rows (limit), got %d", len(rows))
	}
	// no-leak: exactly the allow-listed keys, nothing else.
	allowed := map[string]bool{"status": true, "created_at": true, "finished_at": true}
	for i, row := range rows {
		for k := range row {
			if !allowed[k] {
				t.Errorf("row %d leaks forbidden key %q", i, k)
			}
		}
		if _, ok := row["id"]; ok {
			t.Errorf("row %d must not carry a run id", i)
		}
	}
	// first row is the newest a11y run and carries its finished_at.
	if rows[0]["status"] != store.RunStatusFinished {
		t.Errorf("row0 status = %v, want finished", rows[0]["status"])
	}
	if rows[0]["finished_at"] == nil {
		t.Error("row0 (has FinishedAt) must project finished_at")
	}
	if rows[1]["finished_at"] != nil {
		t.Error("row1 (no FinishedAt) must omit finished_at")
	}

	// category-less share sees all of its bot's runs (nil skipped) — 4 here.
	if all := editorRunRows(recs, "", 100); len(all) != 4 {
		t.Fatalf("category-less: want 4 rows, got %d", len(all))
	}
}
