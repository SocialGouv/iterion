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

func TestValidateArtifactContractsUsesCheckpointRevisionWhenIndexLags(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-checkpoint-wins", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	for version := 0; version < 2; version++ {
		contract := &store.ArtifactContract{LogicalRef: "report", ProducerNode: "writer", Version: version}
		if version == 1 {
			contract.Dependencies = []store.ArtifactDependency{{LogicalRef: "plan", NodeID: "missing", Version: 0, Required: true}}
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: run.ID, NodeID: "writer", Version: version,
			Data: map[string]any{"version": version}, Contract: contract,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The checkpoint is authoritative at writer/v1 while the Mongo cache is
	// simulated as lagging at writer/v0.
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.Checkpoint = &store.Checkpoint{
		NodeID: "next",
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"report": {NodeID: "writer", Version: 1},
		},
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	err = ValidateArtifactContracts(ctx, s, run, wf, "", false)
	if err == nil || !errors.Is(err, ErrArtifactContractUnavailable) {
		t.Fatalf("checkpoint-selected invalid revision was accepted: %v", err)
	}
}

func TestArtifactRevisionsForValidationIncludesParallelBranches(t *testing.T) {
	run := &store.Run{
		ArtifactIndex: map[string]int{"stale": 9},
		Checkpoint: &store.Checkpoint{
			ArtifactRevisions: map[string]store.ArtifactRevisionRef{"root": {NodeID: "root-node", Version: 1}},
			Parallel: &store.ParallelCheckpoint{Branches: map[string]*store.BranchCheckpoint{
				"branch": {ArtifactRevisions: map[string]store.ArtifactRevisionRef{"branch": {NodeID: "branch-node", Version: 2}}},
			}},
		},
	}
	got := artifactRevisionsForValidation(run)
	if len(got) != 2 || got[0].NodeID != "branch-node" || got[1].NodeID != "root-node" {
		t.Fatalf("checkpoint validation revisions = %+v", got)
	}
}

func TestMaterializeHumanArtifactKeepsIncomingArtifactDependency(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "human-contract", "wf", nil); err != nil {
		t.Fatal(err)
	}
	human := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "approve"}, Publish: "approval"}
	edge := &ir.Edge{From: "planner", To: "approve", With: []*ir.DataMapping{{
		Key: "plan", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"approve": human,
	}, Edges: []*ir.Edge{edge}}, s, newStubExecutor())
	revisions := map[string]store.ArtifactRevisionRef{"plan": {NodeID: "planner", Version: 0}}
	_, err := eng.materializeHumanArtifact(
		ctx, "human-contract", "approve", map[string]any{"approved": true},
		map[string]int{"planner": 1},
		map[string]map[string]any{"planner": {"ok": true}},
		map[string]map[string]any{"plan": {"ok": true}},
		revisions,
		map[string][]store.IncomingEdge{"approve": {incomingFromEdge(edge)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.LoadArtifact(ctx, "human-contract", "approve", 0)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Contract == nil || len(artifact.Contract.Dependencies) != 1 || artifact.Contract.Dependencies[0].NodeID != "planner" {
		t.Fatalf("human artifact dependencies = %+v", artifact.Contract)
	}
}

func TestRebuildArtifactRevisionsDoesNotAliasPersistedProducerAfterRename(t *testing.T) {
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new-name"},
		"legacy": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "legacy"}, Publish: "legacy-name"},
		"other":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "other"}, Publish: "old-name"},
	}}, nil, newStubExecutor())
	revisions := eng.rebuildArtifactRevisions(
		map[string]map[string]any{"writer": {"ok": true}, "legacy": {"ok": true}, "other": {"ok": true}},
		map[string]int{"writer": 2, "legacy": 1, "other": 1},
		map[string]store.ArtifactRevisionRef{"old-name": {NodeID: "writer", Version: 1}},
	)
	if got := revisions["old-name"]; got.NodeID != "writer" || got.Version != 1 {
		t.Fatalf("persisted revision = %+v", got)
	}
	if _, aliased := revisions["new-name"]; aliased {
		t.Fatalf("forced publish rename synthesized contradictory revision: %+v", revisions)
	}
	if got := revisions["legacy-name"]; got.NodeID != "legacy" || got.Version != 0 {
		t.Fatalf("partial legacy revision was not inferred: %+v", revisions)
	}
}

func TestRebuildArtifactsLoadsRecordedPhysicalVersions(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "artifact-exact-data", "wf", nil); err != nil {
		t.Fatal(err)
	}
	for version := 0; version < 2; version++ {
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: "artifact-exact-data", NodeID: "writer", Version: version,
			Data: map[string]any{"version": version},
		}); err != nil {
			t.Fatal(err)
		}
	}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new"},
	}}, s, newStubExecutor())
	artifacts, err := eng.rebuildArtifactsWithRevisions(
		ctx, "artifact-exact-data",
		map[string]map[string]any{"writer": {"version": 1}},
		map[string]store.ArtifactRevisionRef{
			"old": {NodeID: "writer", Version: 0},
			"new": {NodeID: "writer", Version: 1},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts["old"]["version"] != float64(0) || artifacts["new"]["version"] != float64(1) {
		t.Fatalf("rebuilt artifacts = %+v", artifacts)
	}
}

func TestNodeArtifactRefsIncludesPostconditionAndReviewURL(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"tool": &ir.ToolNode{
			BaseNode: ir.BaseNode{ID: "tool"}, Publish: "tool-result",
			PostcondRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
		},
		"review": &ir.HumanNode{
			BaseNode: ir.BaseNode{ID: "review"}, Publish: "verdict",
			ReviewURLRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"environment"}}},
		},
	}}
	if got := ir.NodeArtifactRefs(wf, "tool"); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("tool artifact refs = %v", got)
	}
	if got := ir.NodeArtifactRefs(wf, "review"); len(got) != 1 || got[0] != "environment" {
		t.Fatalf("review artifact refs = %v", got)
	}
}

func TestValidateArtifactContractsChecksTransitiveDependencies(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-transitive", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "b", Version: 0, Data: map[string]any{"value": "b"},
		Contract: &store.ArtifactContract{
			LogicalRef: "b", ProducerNode: "b", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "c", NodeID: "c", Version: 0, Required: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "a", Version: 0, Data: map[string]any{"value": "a"},
		Contract: &store.ArtifactContract{
			LogicalRef: "a", ProducerNode: "a", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "b", NodeID: "b", Version: 0, Required: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	run.Checkpoint = &store.Checkpoint{ArtifactRevisions: map[string]store.ArtifactRevisionRef{
		"a": {NodeID: "a", Version: 0},
	}}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "a"},
		"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}, Publish: "b"},
		"c": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "c"}, Publish: "c"},
	}}
	err = ValidateArtifactContracts(ctx, s, run, wf, "", false)
	if err == nil || !errors.Is(err, ErrArtifactContractUnavailable) {
		t.Fatalf("missing transitive dependency was accepted: %v", err)
	}
}
