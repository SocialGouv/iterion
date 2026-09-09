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
		artifacts:        map[string]map[string]any{"plan": {"ok": true}},
		artifactVersions: map[string]int{"planner": 2},
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
	if err == nil || !strings.Contains(err.Error(), "absent from the run") {
		t.Fatalf("missing version-zero dependency error = %v", err)
	}
	if err = ValidateArtifactContracts(ctx, s, run, wf, "rev", true); err == nil || !strings.Contains(err.Error(), "absent from the run") {
		t.Fatalf("force waived persisted dependency integrity: %v", err)
	}
}
