package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestRewindThenResume_SkipsUpstreamNodes is the acceptance test for the
// in-place rewind loop end to end: run → fail → rewind to a middle node →
// resume, and assert the engine re-executes ONLY from the pivot.
//
// This is the property the whole feature exists for. Everything else
// (checkpoint shape, dropped outputs) is a means to it: if `survey` and
// `plan` execute a second time, the operator paid twice and the rewind
// bought nothing over a plain re-run.
func TestRewindThenResume_SkipsUpstreamNodes(t *testing.T) {
	wf := compileFixtureStubSafe(t, "rewind_mini.bot")

	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	// Pass 1: verify fails execution (a transient-style error, NOT the
	// `fail` terminal node — reaching that is intentional termination and
	// produces a non-resumable `failed` with the checkpoint cleared), so
	// the run lands failed_resumable anchored on `verify`.
	exec := newScenarioExecutor()
	exec.on("verify", func(_ map[string]any) (map[string]any, error) {
		return nil, errors.New("verify blew up")
	})

	const runID = "e2e-rewind-mini"
	eng := runtime.New(wf, st, exec)
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("expected the run to fail (verify -> fail), got success")
	}

	run, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Checkpoint == nil {
		t.Fatal("expected a checkpoint after the failing pass")
	}
	for _, id := range []string{"survey", "plan", "implement"} {
		if exec.callCount(id) != 1 {
			t.Fatalf("pass 1: %s called %d times, want 1", id, exec.callCount(id))
		}
	}

	// The operator now edits main.bot and rewinds to `implement`.
	svc, err := runview.NewService(storeDir, runview.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Rewind(context.Background(), runview.RewindSpec{
		RunID:  runID,
		NodeID: "implement",
		// The e2e engine runs a compiled fixture with no persisted
		// FilePath, so point the graph resolution at the source.
		SourcePath: filepath.Join("testdata", "rewind_mini.bot"),
	})
	if err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("rewind minted run %q; it must mutate %q in place", result.RunID, runID)
	}

	// Pass 2: resume. `verify` now approves so the run can finish.
	exec.on("verify", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"value": "approved", "ok": true}, nil
	})
	eng2 := runtime.New(wf, st, exec)
	if err := eng2.Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume after rewind: %v", err)
	}

	run, err = st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run after resume: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Errorf("status after rewind+resume = %s, want finished", run.Status)
	}

	// The point of the whole feature: upstream stays paid-for.
	for _, id := range []string{"survey", "plan"} {
		if n := exec.callCount(id); n != 1 {
			t.Errorf("%s executed %d times across rewind+resume, want 1 — upstream must not replay", id, n)
		}
	}
	// The pivot and its downstream do replay.
	if n := exec.callCount("implement"); n != 2 {
		t.Errorf("implement executed %d times, want 2 (once per pass) — the pivot must re-execute", n)
	}
	if n := exec.callCount("verify"); n != 2 {
		t.Errorf("verify executed %d times, want 2 — downstream of the pivot must re-execute", n)
	}

	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if !hasEvent(events, store.EventRunRewound) {
		t.Error("missing run_rewound event — the rewind must leave an audit marker")
	}
	// events.jsonl is append-only: the first pass's node_started records
	// survive alongside the replayed ones.
	if n := countEventType(events, store.EventNodeStarted); n < 6 {
		t.Errorf("node_started count = %d, want >= 6 (4 first pass + 2 replayed) — history must not be truncated", n)
	}
	if !hasEvent(events, store.EventRunResumed) {
		t.Error("missing run_resumed event")
	}
}

// TestRewind_RefusesRunningRun_E2E guards the concurrency precondition
// against the real store rather than a hand-built run doc.
func TestRewind_RefusesRunningRun_E2E(t *testing.T) {
	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	const runID = "e2e-rewind-running"
	if _, err := st.CreateRun(context.Background(), runID, "rewind_mini", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// CreateRun parks a run in `running` by contract.
	svc, err := runview.NewService(storeDir, runview.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Rewind(context.Background(), runview.RewindSpec{
		RunID:      runID,
		NodeID:     "implement",
		SourcePath: filepath.Join("testdata", "rewind_mini.bot"),
	})
	if !errors.Is(err, runview.ErrRewindNotRewindable) {
		t.Fatalf("Rewind on a running run: err = %v, want ErrRewindNotRewindable", err)
	}
}
