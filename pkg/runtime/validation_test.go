package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

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
	exec.on("my_agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Return wrong types: score should be int (float64 in JSON), not string.
		return map[string]interface{}{
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

func TestSchemaValidation_DisabledByDefault(t *testing.T) {
	wf := validationWorkflow()

	exec := newStubExecutor()
	exec.on("my_agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Return wrong types — but validation is disabled.
		return map[string]interface{}{
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
	exec.on("my_agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
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
	exec.on("my_agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Missing "score" field.
		return map[string]interface{}{
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
	exec.on("branch_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "ok"}, nil
	})
	exec.on("branch_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Return wrong type — should cause branch to fail.
		return map[string]interface{}{"result": 42}, nil
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
	exec.on("my_agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"anything": "goes"}, nil
	})

	eng := New(wf, tmpStore(t), exec, WithOutputValidation(true))
	err := eng.Run(context.Background(), "run-no-schema", nil)
	if err != nil {
		t.Fatalf("expected success for node without schema, got: %v", err)
	}
}
