package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestValidateArtifactContractsRefusesIncompatibleRevisionInEnforce(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-preflight", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "rev-new"
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: "artifact-preflight", NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-old", Version: 0,
		},
		Data: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{
			BaseNode:     ir.BaseNode{ID: "writer"},
			SchemaFields: ir.SchemaFields{OutputSchema: "new-schema"},
			Publish:      "renamed-report",
		},
	}}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "rev-new", false); err == nil {
		t.Fatal("source-derived artifact contract changes accepted")
	}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "rev-new", true); err != nil {
		t.Fatalf("forced source-change resume rejected publish/schema/revision edit: %v", err)
	}
	run.ExecutionContext.Policy = store.ContextPolicyReport
	if err := ValidateArtifactContracts(ctx, s, run, wf, "rev-new", false); err != nil {
		t.Fatalf("report-only context refused artifact: %v", err)
	}
	events, err := s.LoadEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundReport := false
	for _, event := range events {
		if event.Type == store.EventArtifactContractViolation {
			foundReport = true
		}
	}
	if !foundReport {
		t.Fatal("report-only artifact violation was not persisted")
	}
}

type artifactReadErrorStore struct{ store.RunStore }

func (artifactReadErrorStore) LoadArtifact(context.Context, string, string, int) (*store.Artifact, error) {
	return nil, errors.New("blob unavailable")
}

func TestValidateArtifactContractsFailsClosedOnUnreadableEnforcedArtifact(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-unreadable", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	err = ValidateArtifactContracts(ctx, artifactReadErrorStore{base}, run, &ir.Workflow{}, "rev", false)
	if err == nil || !strings.Contains(err.Error(), "blob unavailable") {
		t.Fatalf("unreadable enforced artifact error = %v", err)
	}
}

func TestArtifactContractRecordsConsumedArtifactVersion(t *testing.T) {
	producer := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"}
	consumer := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
		CommandRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner": producer,
		"writer":  consumer,
	}}, workflowHash: "rev"}
	rs := &runState{
		artifacts:         map[string]map[string]any{"plan": {"ok": true}},
		artifactVersions:  map[string]int{"planner": 2},
		artifactRevisions: map[string]store.ArtifactRevisionRef{"plan": {NodeID: "planner", Version: 1}},
	}
	contract := eng.artifactContractFor("writer", consumer, 0, rs)
	if contract == nil || len(contract.Dependencies) != 1 {
		t.Fatalf("contract dependencies = %+v", contract)
	}
	dep := contract.Dependencies[0]
	if dep.LogicalRef != "plan" || dep.NodeID != "planner" || dep.Version != 1 || !dep.Required {
		t.Fatalf("dependency = %+v", dep)
	}
}

func TestResumeReportsSourceChangeBeforeDerivativeArtifactMismatch(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "source-before-artifact", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.WorkflowHash = "rev-old"
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
		Workflow: store.WorkflowContext{WorkflowRevision: "rev-old"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{LogicalRef: "old", ProducerNode: "writer", ProducerRevision: "rev-old", Version: 0},
		Data:     map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Entry: "writer", Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new"},
	}}
	err = New(wf, s, newStubExecutor(), WithWorkflowHash("rev-new")).Resume(ctx, run.ID, nil)
	if !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatalf("resume error = %v, want ErrWorkflowSourceChanged", err)
	}
}

func TestValidateArtifactContractsIgnoresLegacyArtifact(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-legacy", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, s, run, &ir.Workflow{}, "rev", false); err != nil {
		t.Fatalf("legacy artifact refused: %v", err)
	}
}

func TestValidateArtifactContractsRejectsMissingVersionZeroDependency(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-missing-dependency", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "input", NodeID: "input", Version: 0, Required: true}},
		},
		Data: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	err = ValidateArtifactContracts(ctx, s, run, wf, "rev", false)
	if err == nil || !errors.Is(err, ErrArtifactContractUnavailable) {
		t.Fatalf("missing version-zero dependency error = %v", err)
	}
	if err = ValidateArtifactContracts(ctx, s, run, wf, "rev", true); err == nil || !errors.Is(err, ErrArtifactContractUnavailable) {
		t.Fatalf("force waived persisted dependency integrity: %v", err)
	}
}

func TestArtifactContractUsesExecutedPublisherProvenance(t *testing.T) {
	consumer := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
		CommandRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner_a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner_a"}, Publish: "plan"},
		"planner_b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner_b"}, Publish: "plan"},
		"writer":    consumer,
	}}}
	rs := &runState{
		artifacts:         map[string]map[string]any{"plan": {"selected": "b"}},
		artifactVersions:  map[string]int{"planner_a": 5, "planner_b": 1},
		artifactRevisions: map[string]store.ArtifactRevisionRef{"plan": {NodeID: "planner_b", Version: 0}},
	}
	contract := eng.artifactContractFor("writer", consumer, 0, rs)
	if len(contract.Dependencies) != 1 || contract.Dependencies[0].NodeID != "planner_b" || contract.Dependencies[0].Version != 0 {
		t.Fatalf("dependency did not follow the consumed value: %+v", contract.Dependencies)
	}
}

func TestArtifactContractIncludesSelectedIncomingMapping(t *testing.T) {
	producer := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"}
	consumer := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"}
	edge := &ir.Edge{From: "router", To: "writer", With: []*ir.DataMapping{{
		Key: "validated_plan", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner": producer,
		"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}},
		"writer":  consumer,
	}, Edges: []*ir.Edge{edge}}}
	rs := &runState{
		outputs:           map[string]map[string]any{"router": {"ok": true}},
		artifacts:         map[string]map[string]any{"plan": {"ok": true}},
		artifactVersions:  map[string]int{"planner": 1},
		artifactRevisions: map[string]store.ArtifactRevisionRef{"plan": {NodeID: "planner", Version: 0}},
		selectedIncoming:  map[string][]store.IncomingEdge{"writer": {incomingFromEdge(edge)}},
	}
	contract := eng.artifactContractFor("writer", consumer, 0, rs)
	if len(contract.Dependencies) != 1 || contract.Dependencies[0].LogicalRef != "plan" {
		t.Fatalf("incoming mapping dependency = %+v", contract.Dependencies)
	}
}

func TestValidateArtifactContractsLoadsExactDependencyWhenIndexLags(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-lagging-index", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: run.ID, NodeID: "planner", Version: 0, Data: map[string]any{"ok": false}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: run.ID, NodeID: "planner", Version: 1, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "plan", NodeID: "planner", Version: 1, Required: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate the Mongo best-effort cache lagging behind the durable blob.
	run.ArtifactIndex = map[string]int{"writer": 0, "planner": 0}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "rev", false); err != nil {
		t.Fatalf("durable dependency was rejected because its index lagged: %v", err)
	}
}

// seedSchemaContractRun persists one artifact whose contract was stamped
// against `stamped` and returns the run plus the store, so a test only has to
// supply the workflow the resume would run against.
func seedSchemaContractRun(t *testing.T, id string, stamped *ir.Workflow) (store.RunStore, *store.Run) {
	t.Helper()
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, id, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	schema := ir.NodeOutputSchema(stamped.Nodes["writer"])
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: id, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", Version: 0,
			Schema:     schema,
			SchemaHash: schemaFingerprint(stamped, schema),
		},
		Data: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	return s, run
}

// schemaWorkflow is one publishing node plus the schema definition it declares.
func schemaWorkflow(schemaName string, fields ...*ir.SchemaField) *ir.Workflow {
	return &ir.Workflow{
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "writer"},
				SchemaFields: ir.SchemaFields{OutputSchema: schemaName},
				Publish:      "report",
			},
		},
		Schemas: map[string]*ir.Schema{schemaName: {Name: schemaName, Fields: fields}},
	}
}

// TestValidateArtifactContractsComparesSchemaShapeNotName pins both halves of
// the reason the contract records a fingerprint. `ir.NodeOutputSchema` returns
// the schema's reference NAME, so comparing it admits exactly the edit that
// invalidates a persisted artifact — a downstream node reads
// `outputs.writer.<field>` and the field is gone — while refusing a rename
// that changes no shape at all.
func TestValidateArtifactContractsComparesSchemaShapeNotName(t *testing.T) {
	ctx := context.Background()
	summary := &ir.SchemaField{Name: "summary", Type: ir.FieldTypeString}
	detail := &ir.SchemaField{Name: "detail", Type: ir.FieldTypeString}

	t.Run("fields edited under a stable name are refused", func(t *testing.T) {
		stamped := schemaWorkflow("report_out", summary, detail)
		s, run := seedSchemaContractRun(t, "artifact-schema-edited", stamped)
		// Same name, one field removed: a name comparison sees no change.
		edited := schemaWorkflow("report_out", summary)
		err := ValidateArtifactContracts(ctx, s, run, edited, "", false)
		if err == nil {
			t.Fatal("an artifact produced against a different schema definition was admitted")
		}
		if !strings.Contains(err.Error(), "different definition of schema") {
			t.Fatalf("refusal does not name the shape change: %v", err)
		}
	})

	t.Run("a rename with an identical body is accepted", func(t *testing.T) {
		stamped := schemaWorkflow("report_out", summary, detail)
		s, run := seedSchemaContractRun(t, "artifact-schema-renamed", stamped)
		renamed := schemaWorkflow("report_output", summary, detail)
		if err := ValidateArtifactContracts(ctx, s, run, renamed, "", false); err != nil {
			t.Fatalf("renaming a schema whose body is unchanged was refused: %v", err)
		}
	})

	t.Run("a contract with no fingerprint still compares names", func(t *testing.T) {
		stamped := schemaWorkflow("report_out", summary)
		s, run := seedSchemaContractRun(t, "artifact-schema-legacy", stamped)
		// Simulate an artifact written before SchemaHash existed.
		art, err := s.LoadArtifact(ctx, run.ID, "writer", 0)
		if err != nil {
			t.Fatal(err)
		}
		art.Contract.SchemaHash = ""
		if err := s.WriteArtifact(ctx, art); err != nil {
			t.Fatal(err)
		}
		renamed := schemaWorkflow("report_output", summary)
		if err := ValidateArtifactContracts(ctx, s, run, renamed, "", false); err == nil {
			t.Fatal("a legacy contract lost its name comparison fallback")
		}
	})
}
