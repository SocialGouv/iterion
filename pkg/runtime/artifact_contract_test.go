package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// seedContractRun persists a run whose single artifact carries contract,
// under the enforce policy — the shape every gate test below varies.
func seedContractRun(t *testing.T, runID string, contract *store.ArtifactContract) (store.RunStore, *store.Run) {
	t.Helper()
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, runID, "wf", nil)
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
		RunID: runID, NodeID: "writer", Version: 0,
		Contract: contract,
		Data:     map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	return s, run
}

// A revision mismatch alone must NOT refuse. It is the same coarse signal
// ValidateResumeWorkflowHash already gates (where --force is the documented
// override), and refusing on it here wedged the run permanently: a forced
// resume restamps Run.WorkflowHash while older artifacts keep the previous
// revision, so every later resume would need a --force nothing indicates.
func TestValidateArtifactContractsAdmitsRevisionDrift(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-drift", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-old", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	for _, force := range []bool{false, true} {
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: s, Run: run, Workflow: wf, Revision: "rev-new", Force: force,
		}); err != nil {
			t.Fatalf("force=%v: revision drift refused: %v", force, err)
		}
	}
}

// Force is the operator asserting the stored outputs are compatible, not a
// request to stop checking whether they are: a renamed publish reference
// still refuses under enforce, forced or not.
func TestValidateArtifactContractsRefusesRenamedPublishEvenForced(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-preflight", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "summary"},
	}}
	for _, force := range []bool{false, true} {
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: s, Run: run, Workflow: wf, Revision: "rev-new", Force: force,
		})
		if err == nil {
			t.Fatalf("force=%v: renamed publish reference accepted", force)
		}
	}
	run.ExecutionContext.Policy = store.ContextPolicyReport
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("report-only context refused artifact: %v", err)
	}
}

// A rewind is about to invalidate the artifact, so it must not be refused
// by the very output it is discarding.
func TestValidateArtifactContractsSkipsInvalidatedNodes(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-skip", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "summary"},
	}}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Skip: map[string]bool{"writer": true},
	}); err != nil {
		t.Fatalf("artifact the caller is discarding refused the operation: %v", err)
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
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: &ir.Workflow{}, Revision: "rev",
	}); err != nil {
		t.Fatalf("legacy artifact refused: %v", err)
	}
}
