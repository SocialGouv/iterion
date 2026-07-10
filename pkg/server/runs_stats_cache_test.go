package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// countingStore wraps a real RunStore and counts ScanEvents calls so the
// tests can prove the runStatsCache actually elides the per-run event
// walk. Embedding the interface promotes every other method unchanged;
// only ScanEvents is intercepted. The counter is a pointer so copies of
// the value share it.
type countingStore struct {
	store.RunStore
	scans *int32
}

func (c countingStore) ScanEvents(ctx context.Context, runID string, visit func(*store.Event) bool) error {
	atomic.AddInt32(c.scans, 1)
	return c.RunStore.ScanEvents(ctx, runID, visit)
}

// erroringScanStore makes ScanEvents fail every time, to prove a failed
// scan of a terminal run is not memoized (and is therefore retried).
type erroringScanStore struct {
	store.RunStore
	scans *int32
}

func (e erroringScanStore) ScanEvents(_ context.Context, _ string, _ func(*store.Event) bool) error {
	atomic.AddInt32(e.scans, 1)
	return errors.New("scan boom")
}

func TestRunStatsCacheGetPutVersionClear(t *testing.T) {
	c := newRunStatsCache()

	if _, ok := c.get("r1", "v1"); ok {
		t.Fatal("empty cache should miss")
	}

	byDay := map[string]float64{"2026-05-01": 1.5}
	c.put("r1", "v1", byDay)

	if got, ok := c.get("r1", "v1"); !ok || got["2026-05-01"] != 1.5 {
		t.Fatalf("hit on matching version: got=%v ok=%v", got, ok)
	}
	if _, ok := c.get("r1", "v2"); ok {
		t.Fatal("stale version must miss")
	}
	if _, ok := c.get("other", "v1"); ok {
		t.Fatal("unknown run must miss")
	}

	c.clear()
	if _, ok := c.get("r1", "v1"); ok {
		t.Fatal("clear must drop all entries")
	}
}

// seedCostRun creates a run with the given status and a single node_finished
// event carrying _cost_usd, writing directly through the real store (so
// the decorator's scan counter is untouched by seeding).
func seedCostRun(t *testing.T, rs store.RunStore, id, wf string, status store.RunStatus, created time.Time, cost float64) *store.Run {
	t.Helper()
	ctx := context.Background()
	run, err := rs.CreateRun(ctx, id, wf, nil)
	if err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	run.CreatedAt = created
	run.UpdatedAt = created
	run.Status = status
	if status.IsTerminal() {
		fin := created.Add(time.Hour)
		run.UpdatedAt = fin
		run.FinishedAt = &fin
	}
	if err := rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if _, err := rs.AppendEvent(ctx, run.ID, store.Event{
		Type:      store.EventNodeFinished,
		Timestamp: created,
		Data:      map[string]any{"_cost_usd": cost},
	}); err != nil {
		t.Fatalf("AppendEvent %s: %v", id, err)
	}
	return run
}

func summaryOf(run *store.Run) runview.RunSummary {
	return runview.RunSummary{
		ID:           run.ID,
		WorkflowName: run.WorkflowName,
		Status:       run.Status,
		CreatedAt:    run.CreatedAt,
		UpdatedAt:    run.UpdatedAt,
		FinishedAt:   run.FinishedAt,
	}
}

// TestAggregateRunStatsCachesTerminalRunScan is the core behavioural
// test: a terminal run is scanned once and served from cache thereafter,
// while a non-terminal (running) run is re-scanned on every call.
func TestAggregateRunStatsCachesTerminalRunScan(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	term := seedCostRun(t, rs, "term", "wf-term", store.RunStatusFinished, created, 1.0)
	live := seedCostRun(t, rs, "live", "wf-live", store.RunStatusRunning, created, 2.0)

	var scans int32
	cs := countingStore{RunStore: rs, scans: &scans}
	svc, err := runview.NewService("", runview.WithStore(cs))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Discard any scans NewService/reconcileOrphans performed at startup.
	atomic.StoreInt32(&scans, 0)

	summaries := []runview.RunSummary{summaryOf(term), summaryOf(live)}
	cache := newRunStatsCache()

	out1 := aggregateRunStats(ctx, svc, summaries, 30, cache)
	if got := atomic.LoadInt32(&scans); got != 2 {
		t.Fatalf("first pass should scan both runs once: got %d scans", got)
	}
	if out1.TotalCostUSD != 3.0 {
		t.Fatalf("first pass total cost = %v, want 3.0", out1.TotalCostUSD)
	}

	out2 := aggregateRunStats(ctx, svc, summaries, 30, cache)
	// Terminal run served from cache (no scan); live run re-scanned.
	if got := atomic.LoadInt32(&scans); got != 3 {
		t.Fatalf("second pass should scan only the live run: got %d total scans, want 3", got)
	}
	if out2.TotalCostUSD != 3.0 {
		t.Fatalf("second pass total cost = %v, want 3.0 (must match first)", out2.TotalCostUSD)
	}
}

// TestAggregateRunStatsCacheVersionBust proves the cache serves the
// memoized value while the version is stable, and recomputes once the
// run's version (UpdatedAt) advances — the resume --force re-finish case.
func TestAggregateRunStatsCacheVersionBust(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	term := seedCostRun(t, rs, "term", "wf", store.RunStatusFinished, created, 1.0)

	var scans int32
	cs := countingStore{RunStore: rs, scans: &scans}
	svc, err := runview.NewService("", runview.WithStore(cs))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	atomic.StoreInt32(&scans, 0)

	cache := newRunStatsCache()
	sum := summaryOf(term)

	out1 := aggregateRunStats(ctx, svc, []runview.RunSummary{sum}, 30, cache)
	if out1.TotalCostUSD != 1.0 || atomic.LoadInt32(&scans) != 1 {
		t.Fatalf("first pass: cost=%v scans=%d, want 1.0/1", out1.TotalCostUSD, scans)
	}

	// Sneak a second cost event onto the (already terminal) run's log and
	// re-aggregate WITHOUT changing the version: the cache must still hit
	// and return the stale-by-design value, proving it elided the scan.
	if _, err := rs.AppendEvent(ctx, term.ID, store.Event{
		Type:      store.EventNodeFinished,
		Timestamp: created,
		Data:      map[string]any{"_cost_usd": 0.5},
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	out2 := aggregateRunStats(ctx, svc, []runview.RunSummary{sum}, 30, cache)
	if out2.TotalCostUSD != 1.0 || atomic.LoadInt32(&scans) != 1 {
		t.Fatalf("same version must hit cache: cost=%v scans=%d, want 1.0/1", out2.TotalCostUSD, scans)
	}

	// Now bump UpdatedAt (resume --force re-finish): the version changes,
	// the cache misses, and the recomputed total includes the new event.
	sum.UpdatedAt = sum.UpdatedAt.Add(time.Minute)
	out3 := aggregateRunStats(ctx, svc, []runview.RunSummary{sum}, 30, cache)
	if out3.TotalCostUSD != 1.5 || atomic.LoadInt32(&scans) != 2 {
		t.Fatalf("version bump must recompute: cost=%v scans=%d, want 1.5/2", out3.TotalCostUSD, scans)
	}
}

// TestAggregateRunStatsDoesNotCacheFailedScan proves a terminal run whose
// event scan errors is NOT memoized: the next request re-scans rather than
// pinning the partial/empty result for the run's whole terminal lifetime.
func TestAggregateRunStatsDoesNotCacheFailedScan(t *testing.T) {
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	created := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	term := seedCostRun(t, rs, "term", "wf", store.RunStatusFinished, created, 1.0)

	var scans int32
	svc, err := runview.NewService("", runview.WithStore(erroringScanStore{RunStore: rs, scans: &scans}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	atomic.StoreInt32(&scans, 0)

	cache := newRunStatsCache()
	sum := summaryOf(term)
	_ = aggregateRunStats(ctx, svc, []runview.RunSummary{sum}, 30, cache)
	_ = aggregateRunStats(ctx, svc, []runview.RunSummary{sum}, 30, cache)
	if got := atomic.LoadInt32(&scans); got != 2 {
		t.Fatalf("failed scan must not be cached: got %d scans, want 2", got)
	}
}
