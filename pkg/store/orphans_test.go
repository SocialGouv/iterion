package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// seedOrphanRun writes a runs/<id>/run.json with status=running and an
// events.jsonl with the requested mtime. Returns the runID.
func seedOrphanRun(t *testing.T, root, id string, eventsAge time.Duration) string {
	t.Helper()
	runDir := filepath.Join(root, "runs", id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Run{
		ID:           id,
		WorkflowName: "test",
		Status:       RunStatusRunning,
		CreatedAt:    time.Now().UTC().Add(-time.Hour),
		UpdatedAt:    time.Now().UTC().Add(-time.Hour),
	}
	body, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	evPath := filepath.Join(runDir, "events.jsonl")
	if err := os.WriteFile(evPath, []byte(`{"seq":1,"type":"node_started","timestamp":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set events.jsonl mtime to "now - eventsAge" so the staleness
	// check sees the run as live (<OrphanStaleAfter) or dead.
	target := time.Now().Add(-eventsAge)
	if err := os.Chtimes(evPath, target, target); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPromoteStaleOrphans_PromotesStaleRunning(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := seedOrphanRun(t, dir, "stale-run", OrphanStaleAfter+30*time.Second)

	promoted, err := s.PromoteStaleOrphans(context.Background(), iterlog.New(iterlog.LevelError, os.Stderr))
	if err != nil {
		t.Fatalf("PromoteStaleOrphans: %v", err)
	}
	if len(promoted) != 1 || promoted[0].RunID != id {
		t.Fatalf("expected exactly %q promoted, got %+v", id, promoted)
	}
	if promoted[0].FromStatus != RunStatusRunning {
		t.Errorf("FromStatus = %q, want running", promoted[0].FromStatus)
	}
	// On-disk state matches the promotion.
	r, _ := s.LoadRun(context.Background(), id)
	if r.Status != RunStatusFailedResumable {
		t.Errorf("post-promote status = %q, want failed_resumable", r.Status)
	}
	if r.Error == "" {
		t.Error("expected non-empty error message explaining the promotion")
	}
}

func TestPromoteStaleOrphans_SkipsFreshRunning(t *testing.T) {
	// A run whose events.jsonl was touched within OrphanStaleAfter
	// represents an engine that's likely still alive (or just briefly
	// quiet between events). Must NOT be promoted — a false positive
	// here would step on a healthy run.
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := seedOrphanRun(t, dir, "fresh-run", OrphanStaleAfter/4)

	promoted, err := s.PromoteStaleOrphans(context.Background(), iterlog.New(iterlog.LevelError, os.Stderr))
	if err != nil {
		t.Fatalf("PromoteStaleOrphans: %v", err)
	}
	if len(promoted) != 0 {
		t.Fatalf("expected zero promotions, got %+v", promoted)
	}
	r, _ := s.LoadRun(context.Background(), id)
	if r.Status != RunStatusRunning {
		t.Errorf("status should stay running, got %q", r.Status)
	}
}

func TestPromoteStaleOrphans_SkipsTerminalStatuses(t *testing.T) {
	// Already-promoted, finished, failed, cancelled runs must be left
	// alone — calling the sweep repeatedly is idempotent.
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{
		RunStatusFinished,
		RunStatusFailed,
		RunStatusFailedResumable,
		RunStatusCancelled,
		RunStatusPausedWaitingHuman,
	} {
		id := "run-" + string(status)
		runDir := filepath.Join(dir, "runs", id)
		_ = os.MkdirAll(runDir, 0o755)
		r := &Run{ID: id, Status: status, WorkflowName: "test"}
		body, _ := json.MarshalIndent(r, "", "  ")
		_ = os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o644)
		// Stale events to ensure the mtime check would trigger if status
		// were "running" — we want to confirm the status filter, not
		// the mtime filter, is what protects these.
		evPath := filepath.Join(runDir, "events.jsonl")
		_ = os.WriteFile(evPath, []byte{}, 0o644)
		stale := time.Now().Add(-OrphanStaleAfter * 2)
		_ = os.Chtimes(evPath, stale, stale)
	}

	promoted, err := s.PromoteStaleOrphans(context.Background(), iterlog.New(iterlog.LevelError, os.Stderr))
	if err != nil {
		t.Fatalf("PromoteStaleOrphans: %v", err)
	}
	if len(promoted) != 0 {
		t.Errorf("expected zero promotions (only running is targeted), got %+v", promoted)
	}
}

func TestPromoteStaleOrphans_MissingEventsFileIsOrphan(t *testing.T) {
	// A run with status=running but NO events.jsonl at all is an even
	// stronger orphan signal — the engine never got past creating the
	// run dir before dying. Should promote.
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "no-events"
	runDir := filepath.Join(dir, "runs", id)
	_ = os.MkdirAll(runDir, 0o755)
	r := &Run{ID: id, Status: RunStatusRunning, WorkflowName: "test"}
	body, _ := json.MarshalIndent(r, "", "  ")
	_ = os.WriteFile(filepath.Join(runDir, "run.json"), body, 0o644)

	promoted, err := s.PromoteStaleOrphans(context.Background(), iterlog.New(iterlog.LevelError, os.Stderr))
	if err != nil {
		t.Fatalf("PromoteStaleOrphans: %v", err)
	}
	if len(promoted) != 1 || promoted[0].RunID != id {
		t.Fatalf("expected %q promoted, got %+v", id, promoted)
	}
}

func TestPromoteStaleOrphans_NoRunsDirIsNoop(t *testing.T) {
	// A fresh store with no runs/ subdir yet must not error — the
	// studio is allowed to boot against an empty store.
	dir := t.TempDir()
	// Delete the runs/ dir New() creates so we test the missing path.
	_ = os.RemoveAll(filepath.Join(dir, "runs"))
	s := &FilesystemRunStore{root: dir}
	promoted, err := s.PromoteStaleOrphans(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error on missing runs/, got %v", err)
	}
	if promoted != nil && len(promoted) != 0 {
		t.Errorf("expected zero promotions, got %+v", promoted)
	}
}

func TestPromoteStaleOrphans_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedOrphanRun(t, dir, "double-sweep", OrphanStaleAfter*2)

	first, err := s.PromoteStaleOrphans(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("first sweep: want 1 promotion, got %d", len(first))
	}
	second, err := s.PromoteStaleOrphans(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("second sweep: want 0 promotions (idempotent), got %+v", second)
	}
}
