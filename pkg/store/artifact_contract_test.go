package store

import (
	"context"
	"testing"
)

func TestArtifactContractRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "artifact-contract", "wf", nil); err != nil {
		t.Fatal(err)
	}
	want := &ArtifactContract{
		LogicalRef:       "report",
		ProducerNode:     "writer",
		ProducerRevision: "rev-1",
		Version:          2,
		Schema:           "Report",
		Dependencies:     []ArtifactDependency{{LogicalRef: "plan", NodeID: "planner", Version: 1, Required: true}},
		Effects:          []string{"persist"},
	}
	if err := s.WriteArtifact(ctx, &Artifact{RunID: "artifact-contract", NodeID: "writer", Version: 2, Contract: want, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadArtifact(ctx, "artifact-contract", "writer", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Contract == nil || got.Contract.LogicalRef != want.LogicalRef || got.Contract.Dependencies[0].Version != 1 {
		t.Fatalf("contract = %+v, want %+v", got.Contract, want)
	}
}
