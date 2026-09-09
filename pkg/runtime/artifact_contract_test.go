package runtime

import (
	"context"
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
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	check := ArtifactContractCheck{Store: s, Run: run, Workflow: wf, CurrentRevision: "rev-new"}
	if err := ValidateArtifactContracts(ctx, check); err == nil {
		t.Fatal("incompatible artifact revision accepted")
	}
	forced := check
	forced.Force = true
	if err := ValidateArtifactContracts(ctx, forced); err != nil {
		t.Fatalf("forced source-change resume rejected: %v", err)
	}
	// The caller is about to invalidate this very node (a rewind): its
	// artifact must not refuse the operation that discards it.
	skipping := check
	skipping.Skip = map[string]bool{"writer": true}
	if err := ValidateArtifactContracts(ctx, skipping); err != nil {
		t.Fatalf("artifact inside the invalidated set refused the operation: %v", err)
	}
	run.ExecutionContext.Policy = store.ContextPolicyReport
	if err := ValidateArtifactContracts(ctx, check); err != nil {
		t.Fatalf("report-only context refused artifact: %v", err)
	}
}

// TestValidateArtifactContractsSkipsOnlyTheNamedNodes pins the other half of
// the skip contract: an incompatible artifact OUTSIDE the set a rewind
// invalidates still refuses, so the skip cannot be read as "a rewind waives
// contract checking".
func TestValidateArtifactContractsSkipsOnlyTheNamedNodes(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-skip-scope", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"survivor": 0, "rewound": 0}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"survivor", "rewound"} {
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: run.ID, NodeID: nodeID, Version: 0,
			Contract: &store.ArtifactContract{
				LogicalRef: nodeID + "-out", ProducerNode: nodeID, Version: 0, Schema: "old_schema",
			},
			Data: map[string]any{"ok": true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Both nodes now declare a different schema than the one they published.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"survivor": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "survivor"}, Publish: "survivor-out", SchemaFields: ir.SchemaFields{OutputSchema: "new_schema"}},
		"rewound":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "rewound"}, Publish: "rewound-out", SchemaFields: ir.SchemaFields{OutputSchema: "new_schema"}},
	}}
	err = ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Skip: map[string]bool{"rewound": true},
	})
	if err == nil {
		t.Fatal("an incompatible SURVIVING artifact was admitted under enforce")
	}
	if strings.Contains(err.Error(), "rewound-out") {
		t.Fatalf("the skipped node was still reported: %v", err)
	}
	if !strings.Contains(err.Error(), "survivor-out") {
		t.Fatalf("the surviving node was not reported: %v", err)
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
		Store: s, Run: run, Workflow: &ir.Workflow{}, CurrentRevision: "rev",
	}); err != nil {
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
	err = ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, CurrentRevision: "rev",
	})
	if err == nil || !strings.Contains(err.Error(), "absent from the run") {
		t.Fatalf("missing version-zero dependency error = %v", err)
	}
}
