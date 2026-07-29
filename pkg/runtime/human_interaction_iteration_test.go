package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func multiLoopHumanWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "multi_loop_human",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "gate", To: "gate", LoopName: "kit"},
			{From: "gate", To: "done"},
		},
		Loops: map[string]*ir.Loop{
			"plan": {
				Name:          "plan",
				MaxIterations: 3,
				Body:          map[string]bool{"gate": true},
				Entries:       map[string]bool{"gate": true},
			},
			"kit": {
				Name:          "kit",
				MaxIterations: 4,
				Body:          map[string]bool{"gate": true},
				Entries:       map[string]bool{"gate": true},
			},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
	}
}

func TestInteractionIDForPauseUsesCompleteIterationPath(t *testing.T) {
	eng := New(multiLoopHumanWorkflow(), tmpStore(t), newStubExecutor())

	firstCounters := map[string]int{"plan": 2, "kit": 1}
	secondCounters := map[string]int{"plan": 2, "kit": 2}
	if got := eng.currentLoopIteration("gate", firstCounters); got != 2 {
		t.Fatalf("fixture no longer reproduces scalar collision: first max = %d", got)
	}
	if got := eng.currentLoopIteration("gate", secondCounters); got != 2 {
		t.Fatalf("fixture no longer reproduces scalar collision: second max = %d", got)
	}

	first := eng.interactionIDForPause("run-path", "gate", firstCounters)
	second := eng.interactionIDForPause("run-path", "gate", secondCounters)
	if first == second {
		t.Fatalf("distinct iteration paths collapsed to %q", first)
	}

	for id, wantPath := range map[string]string{
		first:  "kit=1;plan=2",
		second: "kit=2;plan=2",
	} {
		const marker = "_loops_"
		_, encoded, ok := strings.Cut(id, marker)
		if !ok {
			t.Fatalf("multi-loop interaction ID %q has no %q suffix", id, marker)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode interaction ID %q: %v", id, err)
		}
		if got := string(decoded); got != wantPath {
			t.Errorf("decoded iteration path = %q, want %q", got, wantPath)
		}
	}
}

func TestInteractionIDForPauseKeepsLegacyFormsWhenUnambiguous(t *testing.T) {
	eng := New(multiLoopHumanWorkflow(), tmpStore(t), newStubExecutor())

	if got := eng.interactionIDForPause(
		"run-legacy", "gate", map[string]int{"plan": 0, "kit": 0},
	); got != "run-legacy_gate" {
		t.Errorf("first all-zero pause ID = %q, want legacy base ID", got)
	}

	singleLoop := &Engine{workflow: &ir.Workflow{Loops: map[string]*ir.Loop{
		"kit": {Name: "kit", Body: map[string]bool{"gate": true}},
	}}}
	if got := singleLoop.interactionIDForPause(
		"run-legacy", "gate", map[string]int{"kit": 3},
	); got != "run-legacy_gate_3" {
		t.Errorf("single-loop pause ID = %q, want legacy scalar ID", got)
	}
}

func TestResumeCreatesDistinctInteractionForCollidingScalarIterations(t *testing.T) {
	ctx := context.Background()
	const runID = "run-multi-loop-resume"

	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, runID, "multi_loop_human", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eng := New(multiLoopHumanWorkflow(), s, newStubExecutor())
	rs := eng.newRunState(runID, nil)
	rs.ctx = ctx
	rs.loopCounters["plan"] = 2
	rs.loopCounters["kit"] = 1

	if err := eng.doPause(
		rs,
		"gate",
		map[string]any{"approved": "Approve?"},
		nil,
		pauseInfo{},
	); err != nil {
		t.Fatalf("initial doPause: %v", err)
	}
	firstRun, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after first pause: %v", err)
	}
	firstID := firstRun.Checkpoint.InteractionID
	wantFirst := eng.interactionIDForPause(
		runID, "gate", map[string]int{"plan": 2, "kit": 1},
	)
	if firstID != wantFirst {
		t.Fatalf("first interaction ID = %q, want %q", firstID, wantFirst)
	}

	err = eng.Resume(ctx, runID, map[string]any{"approved": true})
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Resume should loop to a second human pause, got %v", err)
	}

	secondRun, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if secondRun.Checkpoint == nil {
		t.Fatal("second pause checkpoint is nil")
	}
	if got := secondRun.Checkpoint.LoopCounters["plan"]; got != 2 {
		t.Errorf("plan counter after resume = %d, want 2", got)
	}
	if got := secondRun.Checkpoint.LoopCounters["kit"]; got != 2 {
		t.Errorf("kit counter after resume = %d, want 2", got)
	}
	secondID := secondRun.Checkpoint.InteractionID
	wantSecond := eng.interactionIDForPause(
		runID, "gate", map[string]int{"plan": 2, "kit": 2},
	)
	if secondID != wantSecond {
		t.Fatalf("second interaction ID = %q, want %q", secondID, wantSecond)
	}
	if secondID == firstID {
		t.Fatalf("resume overwrote interaction %q instead of creating a new one", firstID)
	}

	ids, err := s.ListInteractions(ctx, runID)
	if err != nil {
		t.Fatalf("ListInteractions: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("interaction count = %d, want 2: %v", len(ids), ids)
	}
	firstInteraction, err := s.LoadInteraction(ctx, runID, firstID)
	if err != nil {
		t.Fatalf("LoadInteraction(first): %v", err)
	}
	if firstInteraction.AnsweredAt == nil || firstInteraction.Answers["approved"] != true {
		t.Errorf("first interaction was not durably answered: %+v", firstInteraction)
	}
	secondInteraction, err := s.LoadInteraction(ctx, runID, secondID)
	if err != nil {
		t.Fatalf("LoadInteraction(second): %v", err)
	}
	if secondInteraction.AnsweredAt != nil {
		t.Errorf("second interaction should still be pending: %+v", secondInteraction)
	}
	if secondRun.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("run status = %q, want %q", secondRun.Status, store.RunStatusPausedWaitingHuman)
	}
}
