package dispatcher

import (
	"context"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPromoteIfOrphanedPersistsProcessOrphaned pins the ADR-095 follow-up
// this chantier owns: the dispatcher's orphan promotion writes the typed
// PROCESS_ORPHANED classification onto the run document — not just a
// status flip whose "why" lives in log archaeology. The assertion reads
// the PERSISTED doc (the #597 lesson: a fake that drops the meta
// certifies nothing).
func TestPromoteIfOrphanedPersistsProcessOrphaned(t *testing.T) {
	s, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if !s.Capabilities().CrossProcessLock {
		t.Skip("no cross-process lock on this platform — the promotion never fires")
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "orphan-1", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.LoadRun(ctx, "orphan-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.Status = store.RunStatusRunning
	r.CreatedAt = time.Now().Add(-2 * orphanRunGraceWindow)
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	r, _ = s.LoadRun(ctx, "orphan-1")

	c := &Dispatcher{logger: iterlog.Nop()}
	got := c.promoteIfOrphaned(ctx, s, r)
	if got != store.RunStatusFailed {
		t.Fatalf("promotion = %s, want failed (no checkpoint)", got)
	}
	after, err := s.LoadRun(ctx, "orphan-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.FailureCode != store.FailureProcessOrphaned {
		t.Fatalf("persisted failure code = %q, want PROCESS_ORPHANED — the promotion still writes an untyped status", after.FailureCode)
	}
	// And on the TIMELINE: a terminal status with nothing to read is what a
	// consumer triaging by the tree cannot classify.
	events, err := s.LoadEvents(ctx, "orphan-1")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, e := range events {
		if e.Type != store.EventRunFailed {
			continue
		}
		if got, _ := e.Data["code"].(string); got != string(store.FailureProcessOrphaned) {
			t.Fatalf("run_failed.code = %q, want PROCESS_ORPHANED", got)
		}
		return
	}
	t.Fatal("no run_failed event after the orphan promotion")
}

func TestPromoteIfOrphanedKeepsNativeCheckpointResumable(t *testing.T) {
	s, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Capabilities().CrossProcessLock {
		t.Skip("no cross-process lock on this platform")
	}
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	const id = "pc1_dispatcher_orphan"
	if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatal(err)
	}
	identity := store.PortExecutionIdentity{Source: "fixture", Graph: "fixture", Contract: "fixture", Policy: "fixture", Inputs: "fixture"}
	if err := store.SavePortExecution(ctx, s, id, 0, &store.PortExecution{
		Version: store.PortExecutionVersion, Revision: 1, Generation: 1, RootRunID: id, Identity: identity,
	}); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.CreatedAt = time.Now().Add(-2 * orphanRunGraceWindow)
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	c := &Dispatcher{logger: iterlog.Nop()}
	if status := c.promoteIfOrphaned(ctx, s, r); status != store.RunStatusFailedResumable {
		t.Fatalf("native orphan = %s, want failed_resumable", status)
	}
	after, err := s.LoadRun(ctx, id)
	if err != nil || after.Status != store.RunStatusFailedResumable || after.PortExecution == nil || after.FailureCode != store.FailureProcessOrphaned {
		t.Fatalf("native checkpoint lost or classified terminal: run=%+v err=%v", after, err)
	}
}
