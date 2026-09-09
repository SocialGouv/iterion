package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func schemaContractWorkflow(name string, fields ...*ir.SchemaField) *ir.Workflow {
	return &ir.Workflow{
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "writer"},
				SchemaFields: ir.SchemaFields{OutputSchema: name},
				Publish:      "report",
			},
		},
		Schemas: map[string]*ir.Schema{name: {Name: name, Fields: fields}},
	}
}

func seedSchemaContract(t *testing.T, runID string, wf *ir.Workflow) (store.RunStore, *store.Run) {
	t.Helper()
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, runID, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	schema := ir.NodeOutputSchema(wf.Nodes["writer"])
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", Version: 0,
			Schema: schema, SchemaHash: schemaFingerprint(wf, schema),
		},
	}); err != nil {
		t.Fatal(err)
	}
	run, err = s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return s, run
}

func TestValidateArtifactContractsComparesSchemaShape(t *testing.T) {
	summary := &ir.SchemaField{Name: "summary", Type: ir.FieldTypeString}
	detail := &ir.SchemaField{Name: "detail", Type: ir.FieldTypeString}
	stamped := schemaContractWorkflow("report_out", summary, detail)

	t.Run("stable name with changed fields", func(t *testing.T) {
		s, run := seedSchemaContract(t, "artifact-schema-shape", stamped)
		err := ValidateArtifactContracts(context.Background(), s, run, schemaContractWorkflow("report_out", summary), "", false)
		if err == nil || !strings.Contains(err.Error(), "different definition of schema") {
			t.Fatalf("schema shape change was accepted: %v", err)
		}
	})
	t.Run("renamed schema with identical fields", func(t *testing.T) {
		s, run := seedSchemaContract(t, "artifact-schema-rename", stamped)
		if err := ValidateArtifactContracts(context.Background(), s, run, schemaContractWorkflow("renamed", summary, detail), "", false); err != nil {
			t.Fatalf("equivalent schema rename was refused: %v", err)
		}
	})
}

func TestValidateArtifactContractsReportsStableViolationOrder(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-order", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]ir.Node{}
	for _, id := range []string{"echo", "alpha", "delta", "bravo", "charlie"} {
		nodes[id] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: id}, Publish: id + "-now"}
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: run.ID, NodeID: id, Version: 0, Data: map[string]any{"ok": true},
			Contract: &store.ArtifactContract{LogicalRef: id + "-was", ProducerNode: id, Version: 0},
		}); err != nil {
			t.Fatal(err)
		}
	}
	run, err = s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: nodes}
	first := ValidateArtifactContracts(ctx, s, run, wf, "", false)
	if first == nil {
		t.Fatal("publish changes were accepted")
	}
	for i := 0; i < 8; i++ {
		if again := ValidateArtifactContracts(ctx, s, run, wf, "", false); again == nil || again.Error() != first.Error() {
			t.Fatalf("violation order changed: first=%v again=%v", first, again)
		}
	}
	if strings.Index(first.Error(), "alpha-was") > strings.Index(first.Error(), "echo-was") {
		t.Fatalf("violations are not sorted: %v", first)
	}
}

type fixedArtifactStore struct {
	store.RunStore
	artifact *store.Artifact
}

func (s fixedArtifactStore) LoadArtifact(context.Context, string, string, int) (*store.Artifact, error) {
	return s.artifact, nil
}

func TestValidateArtifactContractsBindsPersistedIdentity(t *testing.T) {
	ctx := context.Background()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	t.Run("contract producer", func(t *testing.T) {
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, "artifact-producer", "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
			Contract: &store.ArtifactContract{LogicalRef: "report", ProducerNode: "other", Version: 0},
		}); err != nil {
			t.Fatal(err)
		}
		run, _ = s.LoadRun(ctx, run.ID)
		if err := ValidateArtifactContracts(ctx, s, run, wf, "", true); err == nil || !strings.Contains(err.Error(), "names producer") {
			t.Fatalf("producer mismatch was waived: %v", err)
		}
	})
	t.Run("loaded body", func(t *testing.T) {
		base := tmpStore(t)
		run, err := base.CreateRun(ctx, "artifact-body", "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.ArtifactIndex = map[string]int{"writer": 0}
		run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
		s := fixedArtifactStore{RunStore: base, artifact: &store.Artifact{
			RunID: "foreign", NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
			Contract: &store.ArtifactContract{LogicalRef: "report", ProducerNode: "writer", Version: 0},
		}}
		if err := ValidateArtifactContracts(ctx, s, run, wf, "", false); err == nil || !strings.Contains(err.Error(), "loaded as foreign") {
			t.Fatalf("foreign artifact body was admitted: %v", err)
		}
	})
}

func TestValidateArtifactContractsResolvesDependencyByPublishedRef(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-dep-ref", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []*store.Artifact{
		{RunID: run.ID, NodeID: "planner", Version: 0, Data: map[string]any{"ok": true}, Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "planner", Version: 0}},
		{RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true}, Contract: &store.ArtifactContract{LogicalRef: "report", ProducerNode: "writer", Version: 0, Dependencies: []store.ArtifactDependency{{LogicalRef: "plan", Version: 0, Required: true}}}},
	} {
		if err := s.WriteArtifact(ctx, artifact); err != nil {
			t.Fatal(err)
		}
	}
	run, _ = s.LoadRun(ctx, run.ID)
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"writer":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "", false); err != nil {
		t.Fatalf("published-ref dependency was not resolved: %v", err)
	}
	wf.Nodes["planner"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "renamed"}
	err = ValidateArtifactContracts(ctx, s, run, wf, "", true)
	if err == nil || errors.Is(err, ErrArtifactContractUnavailable) || !strings.Contains(err.Error(), "no node of this workflow publishes") {
		t.Fatalf("orphaned dependency was misclassified: %v", err)
	}
}
