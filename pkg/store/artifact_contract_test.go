package store

import (
	"context"
	"reflect"
	"testing"
)

func TestArtifactContractRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	if _, err := s.CreateRun(ctx, "artifact-contract", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// Every field is set and every field is compared: a contract field the
	// persisted shape silently drops makes the gate that reads it vacuous,
	// which no narrower assertion would catch.
	want := &ArtifactContract{
		LogicalRef:        "report",
		ProducerNode:      "writer",
		ProducerRevision:  "rev-1",
		Version:           2,
		Schema:            "Report",
		SchemaFingerprint: "3d2f1e00",
		Dependencies:      []ArtifactDependency{{LogicalRef: "plan", NodeID: "planner", Version: 1, Required: true}},
		Mutable:           true,
		Effects:           []string{"persist"},
	}
	if err := s.WriteArtifact(ctx, &Artifact{RunID: "artifact-contract", NodeID: "writer", Version: 2, Contract: want, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadArtifact(ctx, "artifact-contract", "writer", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Contract, want) {
		t.Fatalf("contract = %+v, want %+v", got.Contract, want)
	}
}
