package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// TestFinishRun_OperatorCancelSchedulesNoRetry pins the tracker-agnostic
// choke point: on github/forgejo there is no last_run seam, so the guards
// that hold a cancelled ticket do not exist — the ONLY thing standing
// between an operator's cancel and a fresh replay from the workflow entry
// is finishRun refusing to schedule a retry for ErrRunCancelled.
func TestFinishRun_OperatorCancelSchedulesNoRetry(t *testing.T) {
	ft := newFakeTracker()
	c := newTestDispatcher(t, &StubRunner{}, ft, time.Hour)
	c.state.running["fake:1"] = &runningEntry{IssueID: "fake:1", Identifier: "fake#1", RunID: "r1", WorkflowState: "ready"}
	ft.claims["fake:1"] = c.hostMarker

	c.finishRun(context.Background(), "fake:1", fmt.Errorf("%w: run cancelled by user", runtime.ErrRunCancelled))

	if _, ok := c.state.retries["fake:1"]; ok {
		t.Fatal("operator cancel scheduled a retry — on a non-native tracker its empty PrevRunID mints a FRESH run from the workflow entry")
	}
	if _, held := ft.claims["fake:1"]; !held {
		t.Fatal("operator cancel released the claim — the ticket must stay held until an explicit resume")
	}

	// Contrast: an INTERNAL interruption keeps retrying (stall recovery).
	c.state.running["fake:2"] = &runningEntry{IssueID: "fake:2", Identifier: "fake#2", RunID: "r2", WorkflowState: "ready"}
	c.finishRun(context.Background(), "fake:2", fmt.Errorf("%w: at node x", runtime.ErrRunInterrupted))
	if _, ok := c.state.retries["fake:2"]; !ok {
		t.Fatal("an internal interruption no longer schedules a retry — stall recovery regressed")
	}
}

// TestRunStatusOnDisk_UnreadableRecordIsKnownFalse pins the fail-closed
// primitive: "no information" is not "no run" — a truncated run.json must
// read as unknown (the mint path holds), while a genuinely missing record
// stays the legitimate fresh start.
func TestRunStatusOnDisk_UnreadableRecordIsKnownFalse(t *testing.T) {
	c, s := newStoreBackedDispatcher(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, "trunc-1", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusCancelled
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// Truncate run.json mid-write (the crash shape).
	path := filepath.Join(c.storeDir, "runs", "trunc-1", "run.json")
	if err := os.Truncate(path, 10); err != nil {
		t.Fatal(err)
	}
	if _, known := c.runStatusOnDisk("trunc-1"); known {
		t.Fatal("a truncated run.json read as known — the mint guard would fail open over a held cancel")
	}
	if status, known := c.runStatusOnDisk("never-existed"); !known || status != "" {
		t.Fatalf("a missing record must stay the legitimate fresh start: status=%q known=%v", status, known)
	}
	// A DELETED run leaves a tombstone (ErrRunDeleted, not ErrRunNotFound):
	// nothing is alive behind the id either way, so it is the same fresh
	// start — not "no information", which would refuse the mint forever.
	del, err := s.CreateRun(ctx, "deleted-1", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	del.Status = store.RunStatusFinished
	if err := s.SaveRun(ctx, del); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRun(ctx, "deleted-1"); err != nil {
		t.Fatal(err)
	}
	if status, known := c.runStatusOnDisk("deleted-1"); !known || status != "" {
		t.Fatalf("a deleted (tombstoned) record must read as provably absent, like a missing one: status=%q known=%v", status, known)
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
