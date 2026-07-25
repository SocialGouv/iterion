package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestNestedLoopsUseMappingFromSelectedIncomingEdge reproduces a planner
// architecture where two independent correction loops target the same agent:
// an AI review first sends review feedback, then deterministic validation sends
// a newer contract error. Both source nodes retain outputs in run state, but
// only the edge selected for the current visit may provide the loop feedback.
func TestNestedLoopsUseMappingFromSelectedIncomingEdge(t *testing.T) {
	wf := nestedCorrectionWorkflow(t)

	var architectFeedback []string
	validateCalls := 0
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("architect", func(input map[string]any) (map[string]any, error) {
		feedback, _ := input["feedback"].(string)
		architectFeedback = append(architectFeedback, feedback)
		return map[string]any{"stop": len(architectFeedback) >= 3}, nil
	})
	exec.on("validate", func(map[string]any) (map[string]any, error) {
		validateCalls++
		if validateCalls == 1 {
			return map[string]any{"valid": true, "detail": "valid before review"}, nil
		}
		return map[string]any{"valid": false, "detail": "dependency_ids are forbidden"}, nil
	})
	exec.on("review", func(map[string]any) (map[string]any, error) {
		return map[string]any{"retry": true, "feedback": "old catalog review finding"}, nil
	})

	eng := New(wf, tmpStore(t), exec)
	if err := eng.Run(context.Background(), "run-nested-correction-loops", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{"", "old catalog review finding", "dependency_ids are forbidden"}
	if len(architectFeedback) != len(want) {
		t.Fatalf("architect calls=%d feedback=%v, want %d calls", len(architectFeedback), architectFeedback, len(want))
	}
	for i := range want {
		if architectFeedback[i] != want[i] {
			t.Errorf("architect call %d feedback=%q, want %q", i+1, architectFeedback[i], want[i])
		}
	}
}

// TestNestedLoopSelectedMappingSurvivesFailedResume pins the checkpoint half
// of the fix. A failed agent is re-executed from its checkpoint; the runtime
// must restore which correction edge entered it instead of reviving an older
// loop's still-present output.
func TestNestedLoopSelectedMappingSurvivesFailedResume(t *testing.T) {
	wf := nestedCorrectionWorkflow(t)
	st := tmpStore(t)

	var architectFeedback []string
	validateCalls := 0
	failContractAttempt := true
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("architect", func(input map[string]any) (map[string]any, error) {
		feedback, _ := input["feedback"].(string)
		architectFeedback = append(architectFeedback, feedback)
		if feedback == "dependency_ids are forbidden" && failContractAttempt {
			failContractAttempt = false
			return nil, errors.New("transient architect failure")
		}
		return map[string]any{"stop": len(architectFeedback) >= 4}, nil
	})
	exec.on("validate", func(map[string]any) (map[string]any, error) {
		validateCalls++
		if validateCalls == 1 {
			return map[string]any{"valid": true, "detail": "valid before review"}, nil
		}
		return map[string]any{"valid": false, "detail": "dependency_ids are forbidden"}, nil
	})
	exec.on("review", func(map[string]any) (map[string]any, error) {
		return map[string]any{"retry": true, "feedback": "old catalog review finding"}, nil
	})

	const runID = "run-nested-correction-resume"
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("first run succeeded, want transient architect failure")
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load failed run: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.IncomingEdgeIndex == 0 {
		t.Fatalf("checkpoint did not persist selected incoming edge: %+v", r.Checkpoint)
	}

	if err := eng.Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	want := []string{
		"",
		"old catalog review finding",
		"dependency_ids are forbidden",
		"dependency_ids are forbidden",
	}
	if len(architectFeedback) != len(want) {
		t.Fatalf("architect feedback=%v, want %v", architectFeedback, want)
	}
	for i := range want {
		if architectFeedback[i] != want[i] {
			t.Errorf("architect call %d feedback=%q, want %q", i+1, architectFeedback[i], want[i])
		}
	}
}

// TestLegacyCheckpointOnLoopSourceSelectsFreshEdgeAfterResume covers the exact
// upgrade path of an in-flight Town run. The legacy checkpoint is on the
// validator (the loop source), not on the shared architect target. Resuming
// re-executes the validator, so the new runtime selects and records the
// contract_fix edge before it builds the architect input; no persisted incoming
// edge is needed from the old checkpoint in this shape.
func TestLegacyCheckpointOnLoopSourceSelectsFreshEdgeAfterResume(t *testing.T) {
	wf := nestedCorrectionWorkflow(t)
	st := tmpStore(t)

	var architectFeedback []string
	validateCalls := 0
	exec := newStubExecutor()
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("architect", func(input map[string]any) (map[string]any, error) {
		feedback, _ := input["feedback"].(string)
		architectFeedback = append(architectFeedback, feedback)
		return map[string]any{"stop": len(architectFeedback) >= 3}, nil
	})
	exec.on("validate", func(map[string]any) (map[string]any, error) {
		validateCalls++
		switch validateCalls {
		case 1:
			return map[string]any{"valid": true, "detail": "valid before review"}, nil
		case 2:
			return nil, errors.New("transient validator failure")
		default:
			return map[string]any{"valid": false, "detail": "dependency_ids are forbidden"}, nil
		}
	})
	exec.on("review", func(map[string]any) (map[string]any, error) {
		return map[string]any{"retry": true, "feedback": "old catalog review finding"}, nil
	})

	const runID = "run-legacy-checkpoint-on-validator"
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("first run succeeded, want transient validator failure")
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load failed run: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "validate" {
		t.Fatalf("checkpoint=%+v, want validator checkpoint", r.Checkpoint)
	}
	// Simulate a checkpoint written by the pre-fix runtime.
	r.Checkpoint.IncomingEdgeIndex = 0
	if err := st.SaveCheckpoint(context.Background(), runID, r.Checkpoint); err != nil {
		t.Fatalf("save legacy checkpoint: %v", err)
	}

	if err := eng.Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume legacy validator checkpoint: %v", err)
	}
	want := []string{"", "old catalog review finding", "dependency_ids are forbidden"}
	if len(architectFeedback) != len(want) {
		t.Fatalf("architect feedback=%v, want %v", architectFeedback, want)
	}
	for i := range want {
		if architectFeedback[i] != want[i] {
			t.Errorf("architect call %d feedback=%q, want %q", i+1, architectFeedback[i], want[i])
		}
	}
}

func nestedCorrectionWorkflow(t *testing.T) *ir.Workflow {
	t.Helper()
	mapping := func(key, raw string) *ir.DataMapping {
		t.Helper()
		refs, err := ir.ParseRefs(raw)
		if err != nil {
			t.Fatalf("parse mapping %q: %v", raw, err)
		}
		return &ir.DataMapping{Key: key, Raw: raw, Refs: refs}
	}

	return &ir.Workflow{
		Name:  "nested_correction_loops",
		Entry: "seed",
		Nodes: map[string]ir.Node{
			"seed":      &ir.ToolNode{BaseNode: ir.BaseNode{ID: "seed"}},
			"architect": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "architect"}},
			"validate":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "validate"}},
			"review":    &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "review"}},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":      &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "seed", To: "architect", With: []*ir.DataMapping{
				mapping("feedback", ""),
			}},
			{From: "architect", To: "done", Condition: "stop"},
			{From: "architect", To: "validate"},
			{From: "validate", To: "review", Condition: "valid"},
			// Keep the contract edge before the review edge, as in the Town
			// planner. The old all-incoming-edge merge consequently let the
			// later, stale review mapping overwrite this selected edge.
			{
				From:      "validate",
				To:        "architect",
				Condition: "valid",
				Negated:   true,
				LoopName:  "contract_fix",
				With: []*ir.DataMapping{
					mapping("feedback", "{{outputs.validate.detail}}"),
				},
			},
			{
				From:      "review",
				To:        "architect",
				Condition: "retry",
				LoopName:  "review_fix",
				With: []*ir.DataMapping{
					mapping("feedback", "{{outputs.review.feedback}}"),
				},
			},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"contract_fix": {Name: "contract_fix", MaxIterations: 3},
			"review_fix":   {Name: "review_fix", MaxIterations: 3},
		},
	}
}
