package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newStoreBackedDispatcher builds a Dispatcher whose storeDir hosts a real
// filesystem run store, so the resume-decision helpers read genuine run
// records instead of an empty dir.
func newStoreBackedDispatcher(t *testing.T) (*Dispatcher, *store.FilesystemRunStore) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	cfg := &Config{
		Name:      "test",
		Workflow:  t.TempDir() + "/fake.bot",
		Tracker:   TrackerConfig{Kind: "fake"},
		Agent:     AgentConfig{MaxConcurrent: 4, MaxRetryBackoffMS: 1000},
		Workspace: WorkspaceConfig{Root: t.TempDir()},
	}
	cfg.applyDefaults()
	ws, err := NewWorkspaces(cfg.Workspace.Root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	c, err := New(Options{
		Config:     cfg,
		Tracker:    newFakeTracker(),
		Runner:     &StubRunner{},
		Workspaces: ws,
		Logger:     iterlog.New(iterlog.LevelError, &bytes.Buffer{}),
		HostMarker: "test",
		StoreDir:   dir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, s
}

// TestResumableRunID_CancelledIsNotAutoResumed pins the cancel contract:
// `cancelled` can only be an operator's decision (internal stops interrupt
// with runtime.ErrRunInterrupted and persist failed_resumable), so the
// dispatcher must never auto-resume it. failed_resumable and paused_operator
// stay auto-resumable.
func TestResumableRunID_CancelledIsNotAutoResumed(t *testing.T) {
	c, s := newStoreBackedDispatcher(t)
	ctx := context.Background()

	mk := func(status store.RunStatus) string {
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatalf("GenerateRunID: %v", err)
		}
		run, err := s.CreateRun(ctx, id, "wf", nil)
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		run.Status = status
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		return id
	}

	if got := c.resumableRunID(mk(store.RunStatusCancelled)); got != "" {
		t.Fatalf("resumableRunID(cancelled) = %q, want \"\" — an operator cancel must not be auto-resumed", got)
	}
	if got := c.resumableRunID(mk(store.RunStatusFailedResumable)); got == "" {
		t.Fatal("resumableRunID(failed_resumable) = \"\", want the run id — interruptions must keep auto-resuming")
	}
	if got := c.resumableRunID(mk(store.RunStatusPausedOperator)); got == "" {
		t.Fatal("resumableRunID(paused_operator) = \"\", want the run id")
	}
	if got := c.resumableRunID(mk(store.RunStatusFinished)); got != "" {
		t.Fatalf("resumableRunID(finished) = %q, want \"\"", got)
	}
}

// TestReconcileStalled_InterruptsWithCause pins the other half: the
// dispatcher's own stall reaper must cancel WITH runtime.ErrRunInterrupted as
// the cause, so the engine persists failed_resumable (auto-resumed) instead
// of `cancelled` (operator-owned, held).
func TestReconcileStalled_InterruptsWithCause(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Hour)
	cfg := c.cfg.Load()
	cfg.Stall.TimeoutMS = 1 // anything stalls

	runCtx, cancel := context.WithCancelCause(context.Background())
	entry := &runningEntry{
		IssueID:    "fake:1",
		Identifier: "fake#1",
		RunID:      "r1",
		Cancel:     cancel,
		// Ancient watermark → immediately past the stall timeout.
		StartedAt:   time.Now().Add(-time.Hour),
		LastEventAt: time.Now().Add(-time.Hour),
	}
	entry.touchEvent(time.Now().Add(-time.Hour))
	c.state.running["fake:1"] = entry

	c.reconcileStalled(context.Background(), cfg)

	if runCtx.Err() == nil {
		t.Fatal("stalled entry was not cancelled")
	}
	if cause := context.Cause(runCtx); !errors.Is(cause, runtime.ErrRunInterrupted) {
		t.Fatalf("stall cancel cause = %v, want runtime.ErrRunInterrupted — a stall reap is an internal stop, not an operator cancel", cause)
	}
}

// TestCmdCancel_OperatorCauseIsPlainCanceled pins that the HTTP cancel
// surface does NOT masquerade as an infrastructure interruption: the cause
// must stay context.Canceled so the engine persists terminal `cancelled`.
func TestCmdCancel_OperatorCauseIsPlainCanceled(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Hour)

	runCtx, cancel := context.WithCancelCause(context.Background())
	c.state.running["fake:1"] = &runningEntry{
		IssueID:    "fake:1",
		Identifier: "fake#1",
		RunID:      "r1",
		Cancel:     cancel,
	}

	cmdCancel{issueID: "fake:1"}.apply(c, context.Background())

	if runCtx.Err() == nil {
		t.Fatal("cmdCancel did not cancel the run context")
	}
	if cause := context.Cause(runCtx); errors.Is(cause, runtime.ErrRunInterrupted) {
		t.Fatal("operator cancel must not carry ErrRunInterrupted — it would be auto-resumed against the operator's decision")
	}
}
