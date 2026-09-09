package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type correctingExecutor struct {
	*stubExecutor
	correct func(map[string]any, error) (map[string]any, error)
	calls   int
}

func (e *correctingExecutor) CorrectOutput(_ context.Context, _ ir.Node, output map[string]any, validationErr error) (map[string]any, error) {
	e.calls++
	if e.correct == nil {
		return output, validationErr
	}
	return e.correct(output, validationErr)
}

// validationWorkflow builds a simple workflow: agent -> done
// where the agent declares an output schema.
func validationWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "validation_test",
		Entry: "my_agent",
		Nodes: map[string]ir.Node{
			"my_agent": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "my_agent"},
				SchemaFields: ir.SchemaFields{OutputSchema: "MySchema"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "my_agent", To: "done"},
		},
		Schemas: map[string]*ir.Schema{
			"MySchema": {
				Name: "MySchema",
				Fields: []*ir.SchemaField{
					{Name: "summary", Type: ir.FieldTypeString},
					{Name: "score", Type: ir.FieldTypeInt},
				},
			},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func TestSchemaValidation_CatchesBadOutput(t *testing.T) {
	wf := validationWorkflow()

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		// Return wrong types: score should be int (float64 in JSON), not string.
		return map[string]any{
			"summary": "looks good",
			"score":   "not_a_number",
		}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-val-bad", nil)
	if err == nil {
		t.Fatal("expected schema validation error, got nil")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeSchemaValidation {
		t.Errorf("expected error code %s, got %s", ErrCodeSchemaValidation, rtErr.Code)
	}
	if rtErr.NodeID != "my_agent" {
		t.Errorf("expected nodeID %q, got %q", "my_agent", rtErr.NodeID)
	}
}

func TestSchemaValidation_BoundedCorrectionPersistsSuccess(t *testing.T) {
	exec := &correctingExecutor{stubExecutor: newStubExecutor()}
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "initial", "score": "not-a-number"}, nil
	})
	exec.correct = func(_ map[string]any, _ error) (map[string]any, error) {
		return map[string]any{"summary": "repaired", "score": 7}, nil
	}

	st := tmpStore(t)
	err := New(validationWorkflow(), st, exec, WithOutputValidation(true), WithOutputCorrectionBudget(2)).Run(context.Background(), "run-val-correct", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("correction calls = %d, want 1", exec.calls)
	}
	run, err := st.LoadRun(context.Background(), "run-val-correct")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	ep, ok := run.OutputCorrections["my_agent"]
	if !ok || ep.Status != correctionStatusSucceeded || ep.Attempts != 1 {
		t.Fatalf("correction episode = %#v, want succeeded/1", ep)
	}
}

func TestSchemaValidation_UnchangedCorrectionStopsImmediately(t *testing.T) {
	exec := &correctingExecutor{stubExecutor: newStubExecutor()}
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "initial", "score": "not-a-number"}, nil
	})
	exec.correct = func(output map[string]any, _ error) (map[string]any, error) {
		return output, nil
	}

	st := tmpStore(t)
	err := New(validationWorkflow(), st, exec, WithOutputValidation(true), WithOutputCorrectionBudget(5)).Run(context.Background(), "run-val-unchanged", nil)
	if err == nil {
		t.Fatal("expected schema validation failure")
	}
	if exec.calls != 1 {
		t.Fatalf("correction calls = %d, want 1", exec.calls)
	}
	run, loadErr := st.LoadRun(context.Background(), "run-val-unchanged")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	ep := run.OutputCorrections["my_agent"]
	if ep.Status != correctionStatusUnchanged || ep.Attempts != 1 {
		t.Fatalf("correction episode = %#v, want unchanged/1", ep)
	}
}

func TestSchemaValidation_PreseededTerminalEpisodeIsNotReinvoked(t *testing.T) {
	exec := &correctingExecutor{stubExecutor: newStubExecutor()}
	exec.correct = func(output map[string]any, _ error) (map[string]any, error) {
		return map[string]any{"summary": "would be repaired", "score": 1}, nil
	}
	st := tmpStore(t)
	if _, err := st.CreateRun(context.Background(), "run-val-preseed", "validation_test", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run, err := st.LoadRun(context.Background(), "run-val-preseed")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.OutputCorrections = map[string]store.OutputCorrectionEpisode{
		"my_agent": {
			EpisodeID:        "episode-1",
			NodeID:           "my_agent",
			Budget:           5,
			Attempts:         1,
			Status:           correctionStatusUnchanged,
			InputFingerprint: correctionFingerprint(map[string]any{"summary": "bad", "score": "x"}),
		},
	}
	if err := st.SaveRun(context.Background(), run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	eng := New(validationWorkflow(), st, exec, WithOutputValidation(true), WithOutputCorrectionBudget(5))
	rs := &runState{ctx: context.Background(), runID: "run-val-preseed"}
	_, validationErr := eng.correctAndValidateNodeOutput(context.Background(), rs, "my_agent", validationWorkflow().Nodes["my_agent"], map[string]any{"summary": "bad", "score": "x"})
	if validationErr == nil || exec.calls != 0 {
		t.Fatalf("preseeded terminal episode invoked corrector: calls=%d err=%v", exec.calls, validationErr)
	}
}

func TestSchemaValidation_CorrectionCarriesUsageMetadata(t *testing.T) {
	exec := &correctingExecutor{stubExecutor: newStubExecutor()}
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "initial", "score": "bad", "_tokens": 9, "_cost_usd": 1.25, "_backend": "test"}, nil
	})
	exec.correct = func(_ map[string]any, _ error) (map[string]any, error) {
		return map[string]any{"summary": "repaired", "score": 7}, nil
	}
	st := tmpStore(t)
	if err := New(validationWorkflow(), st, exec, WithOutputValidation(true), WithOutputCorrectionBudget(2)).Run(context.Background(), "run-val-meta", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := st.LoadEvents(context.Background(), "run-val-meta")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, evt := range events {
		if evt.Type != store.EventNodeFinished || evt.NodeID != "my_agent" {
			continue
		}
		if got, _ := extractUsage(evt.Data); got != 9 {
			t.Fatalf("node_finished _tokens = %#v, want 9", evt.Data["_tokens"])
		}
		payload, _ := evt.Data["output"].(map[string]any)
		if got, _ := payload["_backend"].(string); got != "test" {
			t.Fatalf("node_finished output._backend = %#v, want test", payload["_backend"])
		}
		return
	}
	t.Fatal("my_agent node_finished event not found")
}

func TestSchemaValidation_DisabledByDefault(t *testing.T) {
	wf := validationWorkflow()

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		// Return wrong types — but validation is disabled.
		return map[string]any{
			"summary": "looks good",
			"score":   "not_a_number",
		}, nil
	})

	eng := New(wf, tmpStore(t), exec) // no WithOutputValidation
	err := eng.Run(context.Background(), "run-val-disabled", nil)
	if err != nil {
		t.Fatalf("expected success with validation disabled, got: %v", err)
	}
}

func TestSchemaValidation_PassesValidOutput(t *testing.T) {
	wf := validationWorkflow()

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"summary":  "all good",
			"score":    float64(42), // JSON numbers are float64
			"_tokens":  float64(100),
			"_backend": "test",
		}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-val-good", nil)
	if err != nil {
		t.Fatalf("expected success with valid output, got: %v", err)
	}
}

func TestSchemaValidation_MissingField(t *testing.T) {
	wf := validationWorkflow()

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		// Missing "score" field.
		return map[string]any{
			"summary": "partial",
		}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-val-missing", nil)
	if err == nil {
		t.Fatal("expected schema validation error for missing field, got nil")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeSchemaValidation {
		t.Errorf("expected error code %s, got %s", ErrCodeSchemaValidation, rtErr.Code)
	}
}

func TestSchemaValidation_InBranch(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "branch_validation_test",
		Entry: "router",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{
				BaseNode:   ir.BaseNode{ID: "router"},
				RouterMode: ir.RouterFanOutAll,
			},
			"branch_a": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "branch_a"},
				SchemaFields: ir.SchemaFields{OutputSchema: "BranchSchema"},
				LLMFields:    ir.LLMFields{Readonly: true},
			},
			"branch_b": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "branch_b"},
				SchemaFields: ir.SchemaFields{OutputSchema: "BranchSchema"},
				LLMFields:    ir.LLMFields{Readonly: true},
			},
			"merge": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "merge"},
				AwaitMode: ir.AwaitBestEffort,
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "branch_a"},
			{From: "router", To: "branch_b"},
			{From: "branch_a", To: "merge"},
			{From: "branch_b", To: "merge"},
			{From: "merge", To: "done"},
		},
		Schemas: map[string]*ir.Schema{
			"BranchSchema": {
				Name: "BranchSchema",
				Fields: []*ir.SchemaField{
					{Name: "result", Type: ir.FieldTypeString},
				},
			},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("branch_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "ok"}, nil
	})
	exec.on("branch_b", func(_ map[string]any) (map[string]any, error) {
		// Return wrong type — should cause branch to fail.
		return map[string]any{"result": 42}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-val-branch", nil)
	// With best_effort, the run should succeed but branch_b should have failed.
	if err != nil {
		t.Fatalf("expected success with best_effort, got: %v", err)
	}
}

func TestSchemaValidation_NoSchemaSkipsValidation(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "no_schema_test",
		Entry: "my_agent",
		Nodes: map[string]ir.Node{
			"my_agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "my_agent"}}, // no OutputSchema
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "my_agent", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"anything": "goes"}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-no-schema", nil)
	if err != nil {
		t.Fatalf("expected success for node without schema, got: %v", err)
	}
}

// computeGaugeWorkflow: one compute node whose `used_pct` expression feeds a
// declared field of the given type, then done. The shape of a phase-budget
// guard, minus the guard.
func computeGaugeWorkflow(t *testing.T, fieldType ir.FieldType, exprSrc string) *ir.Workflow {
	t.Helper()
	ast, err := expr.Parse(exprSrc)
	if err != nil {
		t.Fatalf("parse %q: %v", exprSrc, err)
	}
	return &ir.Workflow{
		Name:  "compute_gauge",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.ComputeNode{
				BaseNode:     ir.BaseNode{ID: "gate"},
				SchemaFields: ir.SchemaFields{OutputSchema: "gauge"},
				Exprs:        []*ir.ComputeExpr{{Key: "used_pct", AST: ast, Raw: exprSrc}},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
		Schemas: map[string]*ir.Schema{
			"gauge": {Name: "gauge", Fields: []*ir.SchemaField{{Name: "used_pct", Type: fieldType}}},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// TestComputeOutputConformsToDeclaredSchema covers #792: a compute output
// declared `int` kept whatever the expression produced — `0.0952…` under an
// `int` label, no coercion, no error. The output is now conformed to its
// schema where it is produced (computeOutput, the body the trunk and the
// fan-out branch share), NOT behind WithOutputValidation, which no product
// entry point enables: an integral float becomes the int it reads as, a
// fractional one fails the node naming the field, the value and the
// builtin that makes the rounding explicit.
func TestComputeOutputConformsToDeclaredSchema(t *testing.T) {
	t.Run("an integral float under int becomes an int64", func(t *testing.T) {
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "6.0 / 2")
		eng := New(wf, tmpStore(t), newStubExecutor())
		rs := eng.newRunState("run-int-ok", nil)
		out, err := eng.computeOutput(rs, "gate", wf.Nodes["gate"].(*ir.ComputeNode), rs.scope())
		if err != nil {
			t.Fatalf("computeOutput: %v", err)
		}
		if got := out["used_pct"]; got != int64(3) {
			t.Errorf("used_pct = %#v (%T), want int64(3)", got, got)
		}
	})

	t.Run("a fractional float under int fails the node, naming field, value and builtin", func(t *testing.T) {
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "10.58")
		// No WithOutputValidation: the rule must hold on the path a real
		// run takes.
		err := New(wf, tmpStore(t), newStubExecutor()).Run(context.Background(), "run-int-frac", nil)
		if err == nil {
			t.Fatal("run succeeded with 10.58 under an int label")
		}
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) {
			t.Fatalf("expected RuntimeError, got %T: %v", err, err)
		}
		if rtErr.Code != ErrCodeSchemaValidation {
			t.Errorf("code = %s, want %s", rtErr.Code, ErrCodeSchemaValidation)
		}
		if rtErr.NodeID != "gate" {
			t.Errorf("node = %q, want gate", rtErr.NodeID)
		}
		for _, want := range []string{`"used_pct"`, "10.58", "floor(", "round("} {
			if !strings.Contains(rtErr.Message, want) {
				t.Errorf("error %q does not name %q", rtErr.Message, want)
			}
		}
	})

	t.Run("a string under int fails the same way", func(t *testing.T) {
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "'7'")
		err := New(wf, tmpStore(t), newStubExecutor()).Run(context.Background(), "run-int-str", nil)
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeSchemaValidation {
			t.Fatalf("expected a %s error, got %v", ErrCodeSchemaValidation, err)
		}
		if !strings.Contains(rtErr.Message, "expected integer") {
			t.Errorf("error %q does not say what was expected", rtErr.Message)
		}
	})

	t.Run("an integer under float is promoted", func(t *testing.T) {
		wf := computeGaugeWorkflow(t, ir.FieldTypeFloat, "7")
		eng := New(wf, tmpStore(t), newStubExecutor())
		rs := eng.newRunState("run-float-ok", nil)
		out, err := eng.computeOutput(rs, "gate", wf.Nodes["gate"].(*ir.ComputeNode), rs.scope())
		if err != nil {
			t.Fatalf("computeOutput: %v", err)
		}
		if got := out["used_pct"]; got != float64(7) {
			t.Errorf("used_pct = %#v (%T), want float64(7)", got, got)
		}
	})

	t.Run("floor() is the explicit form the error points at", func(t *testing.T) {
		wf := computeGaugeWorkflow(t, ir.FieldTypeInt, "floor(1058 / 100.0)")
		eng := New(wf, tmpStore(t), newStubExecutor())
		rs := eng.newRunState("run-int-floor", nil)
		out, err := eng.computeOutput(rs, "gate", wf.Nodes["gate"].(*ir.ComputeNode), rs.scope())
		if err != nil {
			t.Fatalf("computeOutput: %v", err)
		}
		if got := out["used_pct"]; got != int64(10) {
			t.Errorf("used_pct = %#v (%T), want int64(10)", got, got)
		}
	})
}
