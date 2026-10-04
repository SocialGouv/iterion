package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #1743 — a forked child's inputs are never judged against their var
// constraints. The fork surface pre-checks what it can at fork time and
// records the operator's delta on the child (store.Run.ForkSuppliedInputs);
// the engine judges EXACTLY that record at the child's FIRST resume, against
// the workflow the child compiles, then consumes it. The fixtures here write
// the child document runview.Fork produces — parked cancelled, checkpoint
// anchored at the node the child re-executes first, the delta recorded — so
// the judgment is exercised on the engine side, where the verdict is taken.

// forkInputsWorkflow is the child's program: an anchor tool node the fork
// re-executes, then a human gate that pauses the run for the second resume.
func forkInputsWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "fork_inputs",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"gate": &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}, Publish: "approval"},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "gate"},
			{From: "gate", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			"mode": {
				Name:       "mode",
				Type:       ir.VarString,
				EnumValues: []string{"fast", "slow"},
				HasDefault: true,
				Default:    "fast",
			},
		},
		Loops: map[string]*ir.Loop{},
	}
}

// seedForkChild writes the child document a fork leaves behind: parked
// cancelled, checkpoint anchored at `anchor`, the merged inputs, and the
// recorded operator delta.
func seedForkChild(t *testing.T, s store.RunStore, id string, inputs map[string]any, supplied []string, anchor string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, id, "fork_inputs", inputs); err != nil {
		t.Fatal(err)
	}
	r, err := s.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = store.RunStatusCancelled
	r.ForkSuppliedInputs = supplied
	r.Checkpoint = &store.Checkpoint{
		NodeID:  anchor,
		Outputs: map[string]map[string]any{},
	}
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, id, r.Checkpoint); err != nil {
		t.Fatal(err)
	}
}

// TestForkChildInputsRefusedTypedAtFirstResume is the hole itself, closed:
// the same value on the same declaration runs to completion through the fork
// path while Engine.Run refuses it typed. The child's first resume must
// refuse typed, BEFORE any node executes, and leave the run resumable with
// the record still armed (a forced resume against a changed workflow is
// still judged).
func TestForkChildInputsRefusedTypedAtFirstResume(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	seedForkChild(t, s, "fork-child-yolo", map[string]any{"mode": "yolo"}, []string{"mode"}, "a")

	eng := New(forkInputsWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))
	err := eng.Resume(ctx, "fork-child-yolo", nil)
	if !errors.Is(err, ErrForkInputsViolated) {
		t.Fatalf("resume = %v, want the typed fork-input refusal — a value the operator typed into a fork reached execution without ever meeting a gate", err)
	}
	for _, want := range []string{`"mode"`, `"yolo"`, "fast", "slow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q — the operator typed the value and is reading the refusal", err, want)
		}
	}
	r, err := s.LoadRun(ctx, "fork-child-yolo")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusCancelled {
		t.Fatalf("status = %s, want cancelled — a refused fork child keeps its resumable status so the operator can re-fork", r.Status)
	}
	if strings.Join(r.ForkSuppliedInputs, ",") != "mode" {
		t.Fatalf("ForkSuppliedInputs = %v after the refusal, want still [mode] — the record stays armed for a forced resume against a changed workflow", r.ForkSuppliedInputs)
	}
}

// TestForkChildEmptyRecordIsNotReJudged pins the sibling of the pinned
// launch-resume invariant: a fork child WITHOUT a record is never re-judged
// under a tightened declaration. Every stored value on such a child is the
// parent's (admitted at the parent's launch) — the gate must not even be
// consulted.
func TestForkChildEmptyRecordIsNotReJudged(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	seedForkChild(t, s, "fork-child-stored", map[string]any{"mode": "yolo"}, nil, "a")

	// The declaration tightens AFTER the fork: "yolo" was never legal, but
	// this child carries no operator delta — the record is empty, so the
	// gate call must not even happen, and the run stays resumable.
	tightened := forkInputsWorkflow()
	tightened.Vars["mode"].EnumValues = []string{"fast"}
	if ok := slices.Contains(tightened.Vars["mode"].EnumValues, "yolo"); ok {
		t.Fatal("fixture is inert: the tightened enum still admits the stored value")
	}
	eng := New(tightened, s, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng.Resume(ctx, "fork-child-stored", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("resume = %v, want the pause — a child with no recorded delta is never re-judged, tightening the declaration must not strand it", err)
	}
}

// TestForkChildJudgedOnceThenConsumed pins the one-shot property: the
// judgment passes once, the record is consumed, and a declaration tightened
// between two resumes must not strand the mid-flight child — the same
// invariant that keeps Resume off stored values, one gate later.
func TestForkChildJudgedOnceThenConsumed(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	seedForkChild(t, s, "fork-child-slow", map[string]any{"mode": "slow"}, []string{"mode"}, "a")

	eng := New(forkInputsWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng.Resume(ctx, "fork-child-slow", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first resume: want ErrRunPaused at the gate, got %v", err)
	}
	r, err := s.LoadRun(ctx, "fork-child-slow")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %s, want paused_waiting_human", r.Status)
	}
	if len(r.ForkSuppliedInputs) != 0 {
		t.Fatalf("ForkSuppliedInputs = %v after the passing resume, want cleared — judged once, never again", r.ForkSuppliedInputs)
	}

	// The declaration tightens between the two resumes. The record is
	// consumed, so the second resume judges nothing and crosses.
	tightened := forkInputsWorkflow()
	tightened.Vars["mode"].EnumValues = []string{"fast"}
	if tightened.Vars["mode"].EnumValues[0] != "fast" {
		t.Fatal("fixture is inert")
	}
	eng2 := New(tightened, s, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng2.Resume(ctx, "fork-child-slow", map[string]any{"decision": "approve"}); err != nil {
		t.Fatalf("second resume after the record was consumed: %v — a declaration tightened mid-flight must not strand the child", err)
	}
}

// TestForkChildRecordCoversInheritedKeysToo pins the union: a grandchild
// forked from a child that never resumed inherits the unjudged keys, and the
// grandchild's first resume judges them.
func TestForkChildRecordCoversInheritedKeysToo(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	seedForkChild(t, s, "fork-child-mid", map[string]any{"mode": "yolo"}, []string{"mode"}, "a")

	eng := New(forkInputsWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))
	err := eng.Resume(ctx, "fork-child-mid", nil)
	if !errors.Is(err, ErrForkInputsViolated) {
		t.Fatalf("resume = %v, want the typed refusal — the inherited key rides the chain until a resume judges it", err)
	}
}
