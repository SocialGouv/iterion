package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// capacityGuardWorkflow is two agents in a row: the second blocks until its
// context is done, the shape an interrupted run parks in.
func capacityGuardWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "capacity_guard",
		Entry: "first",
		Nodes: map[string]ir.Node{
			"first":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "first"}},
			"second": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "second"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "first", To: "second"}, {From: "second", To: "done"}},
	}
}

// claimGuardProbe records every run document a claim guard was handed.
type claimGuardProbe struct {
	mu   sync.Mutex
	seen []store.RunStatus
	err  error
}

func (p *claimGuardProbe) guard(_ context.Context, run *store.Run) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, run.Status)
	return p.err
}

func (p *claimGuardProbe) statuses() []store.RunStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]store.RunStatus(nil), p.seen...)
}

// interruptedRun parks a run failed_resumable INTERRUPTED at its second node.
func interruptedRun(t *testing.T, s store.RunStore, runID string, opts ...EngineOption) {
	t.Helper()
	x := newStubExecutor()
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	x.on("second", func(map[string]any) (map[string]any, error) {
		stop(ErrRunInterrupted)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err := New(capacityGuardWorkflow(), s, x, opts...).Run(ctx, runID, nil); !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("Run: %v, want an interruption", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureInterrupted {
		t.Fatalf("interrupted run reads %s/%s, want failed_resumable/INTERRUPTED", r.Status, r.FailureCode)
	}
}

// A claim guard's refusal stops the resume before its claim, with nothing
// written: the run stays failed_resumable at the version it was read, and the
// refusal travels in the error chain for the caller to park on.
func TestResumeClaimGuardRefusesBeforeAnyWrite(t *testing.T) {
	s := tmpStore(t)
	const runID = "claim-guard-refusal"
	interruptedRun(t, s, runID)
	before, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	probe := &claimGuardProbe{err: store.ErrCapacityPlaceLost}
	err = New(capacityGuardWorkflow(), s, newStubExecutor(), WithResumeClaimGuard(probe.guard)).Resume(context.Background(), runID, nil)
	if !errors.Is(err, store.ErrCapacityPlaceLost) {
		t.Fatalf("Resume: %v, want the guard's refusal in the chain", err)
	}
	if got := probe.statuses(); len(got) != 1 || got[0] != store.RunStatusFailedResumable {
		t.Fatalf("guard saw %v, want one call on the failed_resumable document", got)
	}
	after, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// The engine's own admission stamp is written before any claim; the
	// guard's refusal adds nothing to it: no claim, no resumed event.
	if after.Status != store.RunStatusFailedResumable || after.FailureCode != store.FailureInterrupted || after.Error != before.Error {
		t.Fatalf("after the refusal: %s/%s %q, want failed_resumable/INTERRUPTED %q", after.Status, after.FailureCode, after.Error, before.Error)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, ev := range events {
		if ev.Type == store.EventRunResumed {
			t.Fatalf("a run_resumed event was written by a resume its guard refused: %+v", ev)
		}
	}
	// Allowed, the same guard lets the resume claim and finish.
	probe.err = nil
	if err := New(capacityGuardWorkflow(), s, newStubExecutor(), WithResumeClaimGuard(probe.guard)).Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("Resume with the guard satisfied: %v", err)
	}
	if r, _ := s.LoadRun(context.Background(), runID); r.Status != store.RunStatusFinished {
		t.Fatalf("resumed run reads %s, want finished", r.Status)
	}
}

// A guard makes the answered-gate replay claim first: the run is never
// flipped to paused_waiting_human — a status that holds no capacity — ahead of
// the guard, which judges the failed_resumable document it was read in.
func TestResumeClaimGuardClaimsFirstOnTheGateReplay(t *testing.T) {
	s := tmpStore(t)
	wf := &ir.Workflow{
		Name:  "capacity_gate_replay",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	ctx := context.Background()
	const runID = "claim-guard-gate-replay"
	if err := New(wf, s, newStubExecutor()).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = map[string]any{"decision": "approve"}
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.FailRunResumable(ctx, runID, &cp, "drained", store.FailureInterrupted); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	probe := &claimGuardProbe{err: store.ErrCapacityPlaceLost}
	if err := New(wf, s, newStubExecutor(), WithResumeClaimGuard(probe.guard)).Resume(ctx, runID, nil); !errors.Is(err, store.ErrCapacityPlaceLost) {
		t.Fatalf("Resume: %v, want the guard's refusal", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusFailedResumable {
		t.Fatalf("run reads %s after the refused replay, want failed_resumable — the replay flipped it before the guard judged it", got.Status)
	}
	if seen := probe.statuses(); len(seen) != 1 || seen[0] != store.RunStatusFailedResumable {
		t.Fatalf("guard saw %v, want the failed_resumable document", seen)
	}
}

// The park hook fires at the engine's own failed_resumable write, with the
// code written — and not on a run that finishes.
func TestOnResumableParkFiresAtTheEnginePark(t *testing.T) {
	s := tmpStore(t)
	var mu sync.Mutex
	var codes []store.FailureCode
	hook := WithOnResumablePark(func(_ context.Context, code store.FailureCode) {
		mu.Lock()
		defer mu.Unlock()
		codes = append(codes, code)
	})
	interruptedRun(t, s, "park-hook-interrupted", hook)
	mu.Lock()
	got := append([]store.FailureCode(nil), codes...)
	mu.Unlock()
	if len(got) != 1 || got[0] != store.FailureInterrupted {
		t.Fatalf("park hook saw %v, want one INTERRUPTED", got)
	}
	codes = nil
	if err := New(capacityGuardWorkflow(), s, newStubExecutor(), hook).Run(context.Background(), "park-hook-finished", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(codes) != 0 {
		t.Fatalf("park hook fired %v on a finished run", codes)
	}
}
