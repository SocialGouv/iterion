package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPauseSignalProducesPausedOperatorStatus exercises the operator-
// pause vertical slice end-to-end: close the pause signal, observe
// the engine transition to paused_operator with a preserved checkpoint
// and a run_paused event tagged with reason=operator.
func TestPauseSignalProducesPausedOperatorStatus(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "pause_status_test",
		Entry: "step1",
		Nodes: map[string]ir.Node{
			"step1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step1"}},
			"step2": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step2"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2"},
			{From: "step2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	pauseCh := make(chan struct{})
	exec := newStubExecutor()
	exec.on("step1", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Pause AFTER step1 succeeds and writes its checkpoint, so we
		// see the engine pause at step2's pre-execute boundary with
		// the checkpoint NodeID set to step2.
		close(pauseCh)
		return map[string]interface{}{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithPauseSignal(pauseCh))

	err := eng.Run(context.Background(), "run-pause-1", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("expected ErrRunPausedOperator, got: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "run-pause-1")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Errorf("expected status paused_operator, got %s", r.Status)
	}
	if r.Checkpoint == nil {
		t.Fatal("expected non-nil checkpoint after operator pause")
	}
	if r.Checkpoint.NodeID != "step2" {
		t.Errorf("expected checkpoint NodeID=step2 (the node about to run), got %s", r.Checkpoint.NodeID)
	}

	events, err := s.LoadEvents(context.Background(), "run-pause-1")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	sawPaused := false
	for _, evt := range events {
		if evt.Type != store.EventRunPaused {
			continue
		}
		if reason, _ := evt.Data["reason"].(string); reason == "operator" {
			sawPaused = true
			break
		}
	}
	if !sawPaused {
		t.Error("expected run_paused event with reason=operator")
	}
}

// TestPauseSignalNilIsNoOp confirms that omitting WithPauseSignal does
// not gate ctx cancellation — a run with no pause channel still
// cancels via ctx.Done as before.
func TestPauseSignalNilIsNoOp(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "pause_nil_test",
		Entry: "step1",
		Nodes: map[string]ir.Node{
			"step1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step1"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	s := tmpStore(t)
	eng := New(wf, s, exec) // no WithPauseSignal

	err := eng.Run(context.Background(), "run-pause-nil", nil)
	if err != nil {
		t.Fatalf("expected clean finish without pause signal, got: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-pause-nil")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

// TestPauseResumeRoundTrip verifies that a paused_operator run can be
// resumed via Engine.Resume and continues from the checkpoint, just
// like a cancelled run. This is the contract Phase 1 must hold so the
// studio's Resume button works against operator-paused runs.
func TestPauseResumeRoundTrip(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "pause_resume_test",
		Entry: "step1",
		Nodes: map[string]ir.Node{
			"step1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step1"}},
			"step2": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step2"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step1", To: "step2"},
			{From: "step2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	pauseCh := make(chan struct{})
	exec := newStubExecutor()
	step1Count, step2Count := 0, 0
	exec.on("step1", func(_ map[string]interface{}) (map[string]interface{}, error) {
		step1Count++
		close(pauseCh) // fires once; subsequent runs use a different exec/eng
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("step2", func(_ map[string]interface{}) (map[string]interface{}, error) {
		step2Count++
		return map[string]interface{}{"ok": true}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec, WithPauseSignal(pauseCh))
	if err := eng.Run(context.Background(), "run-pr", nil); !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("expected ErrRunPausedOperator on initial run, got: %v", err)
	}
	if step1Count != 1 || step2Count != 0 {
		t.Fatalf("expected step1=1 step2=0 at pause, got step1=%d step2=%d", step1Count, step2Count)
	}

	// Resume without a pause signal — the run should complete.
	eng2 := New(wf, s, exec)
	resumeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := eng2.Resume(resumeCtx, "run-pr", nil); err != nil {
		t.Fatalf("Resume after operator pause failed: %v", err)
	}
	if step1Count != 1 {
		t.Errorf("expected step1 NOT re-executed on resume, got step1=%d", step1Count)
	}
	if step2Count != 1 {
		t.Errorf("expected step2=1 after resume, got step2=%d", step2Count)
	}
	r, _ := s.LoadRun(context.Background(), "run-pr")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished after resume, got %s", r.Status)
	}
}
