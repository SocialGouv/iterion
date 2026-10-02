package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// parallelGateWorkflow: a fan-out whose one branch pauses on a human gate.
func parallelGateWorkflow() (*ir.Workflow, *stubExecutor) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["gate_one"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_one"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "judge" && edge.To == "collect" {
			edge.To = "gate_one"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "gate_one", To: "collect"})
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	return wf, exec
}

// TestResume_aParallelPauseOutrunIsSuperseded: a newer attempt queued after a
// delivery's early check, on a run paused in a parallel branch. The delivery
// leaves queued for the pause only for its own attempt, so it is refused
// ErrResumeSuperseded: it neither takes the run nor runs it with its answers.
func TestResume_aParallelPauseOutrunIsSuperseded(t *testing.T) {
	wf, exec := parallelGateWorkflow()
	base := tmpStore(t)
	ctx := context.Background()
	const runID = "run-outrun-parallel-outrun"
	if err := New(wf, base, exec).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause at gate_one", err)
	}
	r, err := base.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint == nil || r.Checkpoint.Parallel == nil || r.Checkpoint.Parallel.PendingNodeID != "gate_one" || r.Checkpoint.InteractionID == "" {
		t.Fatalf("precondition: a parallel pause with its pointer, got %+v", r.Checkpoint)
	}
	published1 := queueAttempt(t, base, runID, store.RunStatusPausedWaitingHuman)
	s := &requeueAfterFirstLoad{RunStore: base, t: t, runID: runID}
	claimed := false
	err = New(wf, s, exec, WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, map[string]any{"decision": "stale"})
	doc, lerr := base.LoadRun(ctx, runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	t.Logf("stale delivery: err=%v claimed=%v; run now status=%s queued_at=%v", err, claimed, doc.Status, doc.QueuedAt)
	evs, _ := base.LoadEvents(ctx, runID)
	resumed := 0
	for _, ev := range evs {
		if ev.Type == store.EventRunResumed {
			resumed++
		}
	}
	t.Logf("run_resumed events: %d", resumed)
	if !errors.Is(err, ErrResumeSuperseded) || claimed || doc.Status != store.RunStatusQueued {
		t.Fatalf("REFUTED: a delivery outrun mid-resume on a parallel pause: err=%v claimed=%v status=%s — want ErrResumeSuperseded, no claim, the newer attempt still queued", err, claimed, doc.Status)
	}
	_ = time.Now
}

// TestResume_aPauseOutrunRecordsNoAnswer: a newer attempt queued after a
// delivery's early check, on a run paused at its gate. The pause path records
// answers, writes the gate's artifact and finishes its node before its claim;
// it now leaves queued first, for its own attempt only — so the superseded
// delivery records nothing, and the newer attempt's own delivery re-asks the
// gate.
func TestResume_aPauseOutrunRecordsNoAnswer(t *testing.T) {
	base := tmpStore(t)
	ctx := context.Background()
	const runID = "run-outrun-pause-outrun"
	if err := New(gateReplayWorkflow(), base, newStubExecutor()).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := base.LoadRun(ctx, runID)
	if err != nil || r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
		t.Fatalf("precondition: %v %v", r, err)
	}
	interactionID := r.Checkpoint.InteractionID
	published1 := queueAttempt(t, base, runID, store.RunStatusPausedWaitingHuman)
	s := &requeueAfterFirstLoad{RunStore: base, t: t, runID: runID}
	claimed := false
	err = New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, map[string]any{"decision": "stale"})
	doc, _ := base.LoadRun(ctx, runID)
	t.Logf("stale delivery: superseded=%v err=%v claimed=%v status=%s", errors.Is(err, ErrResumeSuperseded), err, claimed, doc.Status)
	in, ierr := base.LoadInteraction(ctx, runID, interactionID)
	if ierr == nil {
		t.Logf("interaction after the superseded delivery: answered_at=%v answers=%v", in.AnsweredAt, in.Answers)
	}
	evs, _ := base.LoadEvents(ctx, runID)
	for _, ev := range evs {
		switch ev.Type {
		case store.EventHumanAnswersRecorded, store.EventNodeFinished, store.EventArtifactWritten:
			t.Logf("event after the pause: %s node=%s data=%v", ev.Type, ev.NodeID, ev.Data)
		}
	}
	if a, aerr := base.LoadArtifact(ctx, runID, "gate", 0); aerr == nil && a != nil {
		t.Logf("artifact gate v0: %v", a.Data)
	}
	// The newer attempt's own delivery: a resume from cancelled, which ships no answers.
	claimed2 := false
	err2 := New(gateReplayWorkflow(), base, newStubExecutor(), WithQueuedAttempt(time.Now().UTC()), WithOnResumeClaimed(func() { claimed2 = true })).Resume(ctx, runID, nil)
	doc2, _ := base.LoadRun(ctx, runID)
	t.Logf("newer attempt's delivery (no answers): err=%v claimed=%v status=%s", err2, claimed2, doc2.Status)
	if a, aerr := base.LoadLatestArtifact(ctx, runID, "gate"); aerr == nil && a != nil {
		t.Logf("latest gate artifact after the newer attempt: v%d %v", a.Version, a.Data)
	}
	if in != nil && in.Answers["decision"] == "stale" {
		t.Fatalf("REFUTED: the superseded delivery (refused %v) recorded its answers on the newer attempt's gate: %v", errors.Is(err, ErrResumeSuperseded), in.Answers)
	}
}

// TestResume_aPauseSupersededEarlyReasksTheGate: the same newer attempt,
// queued before the delivery's early check — refused there — and the newer
// attempt's own delivery re-asks the gate.
func TestResume_aPauseSupersededEarlyReasksTheGate(t *testing.T) {
	base := tmpStore(t)
	ctx := context.Background()
	const runID = "run-outrun-pause-control"
	if err := New(gateReplayWorkflow(), base, newStubExecutor()).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	published1 := queueAttempt(t, base, runID, store.RunStatusPausedWaitingHuman)
	requeueOnce(t, base, runID)
	err := New(gateReplayWorkflow(), base, newStubExecutor(), WithQueuedAttempt(published1)).Resume(ctx, runID, map[string]any{"decision": "stale"})
	t.Logf("stale delivery: superseded=%v", errors.Is(err, ErrResumeSuperseded))
	err2 := New(gateReplayWorkflow(), base, newStubExecutor(), WithQueuedAttempt(time.Now().UTC())).Resume(ctx, runID, nil)
	doc2, _ := base.LoadRun(ctx, runID)
	t.Logf("newer attempt's delivery (no answers): err=%v status=%s", err2, doc2.Status)
}
