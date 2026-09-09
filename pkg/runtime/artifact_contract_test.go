package runtime

import (
	"context"
	"os"
	"path/filepath"
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

// reportSchema is the workflow shape the fingerprint tests vary.
func reportSchema(fields ...*ir.SchemaField) *ir.Workflow {
	return &ir.Workflow{
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "writer"},
				SchemaFields: ir.SchemaFields{OutputSchema: "Report"},
				Publish:      "report",
			},
		},
		Schemas: map[string]*ir.Schema{"Report": {Name: "Report", Fields: fields}},
	}
}

// The schema NAME is only a reference: editing the body of `schema Report`
// while the node still declares `output: Report` is the most common
// incompatible edit, and the name comparison alone cannot see it. Force
// does not waive it — that is precisely the assertion force makes.
func TestValidateArtifactContractsRefusesChangedSchemaBody(t *testing.T) {
	ctx := context.Background()
	written := reportSchema(
		&ir.SchemaField{Name: "summary", Type: ir.FieldTypeString},
		&ir.SchemaField{Name: "score", Type: ir.FieldTypeInt},
	)
	s, run := seedContractRun(t, "artifact-schema-body", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
		Schema: "Report", SchemaFingerprint: schemaFingerprint(written, "Report"),
	})

	// Same fields in another declaration order: outputs are maps, so this
	// is not an incompatibility and must be admitted.
	reordered := reportSchema(
		&ir.SchemaField{Name: "score", Type: ir.FieldTypeInt},
		&ir.SchemaField{Name: "summary", Type: ir.FieldTypeString},
	)
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: reordered, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("reordered schema fields refused: %v", err)
	}

	// A field whose type changed, under the same schema name.
	retyped := reportSchema(
		&ir.SchemaField{Name: "summary", Type: ir.FieldTypeString},
		&ir.SchemaField{Name: "score", Type: ir.FieldTypeString},
	)
	for _, force := range []bool{false, true} {
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: s, Run: run, Workflow: retyped, Revision: "rev-new", Force: force,
		}); err == nil {
			t.Fatalf("force=%v: changed schema body accepted", force)
		}
	}
}

// An artifact written before the fingerprint existed carries none, and must
// keep resuming: the rollout cannot break every run already on disk.
func TestValidateArtifactContractsAdmitsUnfingerprintedSchema(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-schema-legacy", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
		Schema: "Report",
	})
	wf := reportSchema(&ir.SchemaField{Name: "other", Type: ir.FieldTypeBool})
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("artifact without a schema fingerprint refused: %v", err)
	}
}

// The fingerprint must survive an engine upgrade that renumbers the
// FieldType enum, so it is taken over the type's name, never its integer.
func TestSchemaFingerprintNamesTheFieldType(t *testing.T) {
	wf := reportSchema(&ir.SchemaField{Name: "score", Type: ir.FieldTypeInt, EnumValues: []string{"b", "a"}})
	fp := schemaFingerprint(wf, "Report")
	if fp == "" {
		t.Fatal("resolved schema produced no fingerprint")
	}
	if same := schemaFingerprint(reportSchema(
		&ir.SchemaField{Name: "score", Type: ir.FieldTypeInt, EnumValues: []string{"a", "b"}},
	), "Report"); same != fp {
		t.Fatal("enum declaration order changed the fingerprint")
	}
	if other := schemaFingerprint(reportSchema(
		&ir.SchemaField{Name: "score", Type: ir.FieldTypeFloat, EnumValues: []string{"a", "b"}},
	), "Report"); other == fp {
		t.Fatal("a changed field type left the fingerprint identical")
	}
	if unknown := schemaFingerprint(wf, "Missing"); unknown != "" {
		t.Fatalf("unresolvable schema fingerprinted as %q", unknown)
	}
}

// An unreadable artifact is not a compatible one. A missing file stays
// tolerated (a stale index carries no contract to check), but a corrupt or
// unreachable one must refuse under enforce — otherwise a store blip makes
// the whole gate admit everything for its duration.
func TestValidateArtifactContractsFailsClosedOnUnreadableArtifact(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-unreadable", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	fsStore, ok := s.(*store.FilesystemRunStore)
	if !ok {
		t.Fatalf("tmpStore is %T, expected a filesystem store", s)
	}
	path := filepath.Join(fsStore.Root(), "runs", run.ID, "artifacts", "writer", "0.json")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("index entry with no artifact behind it refused: %v", err)
	}

	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err == nil {
		t.Fatal("undecodable artifact admitted under enforce")
	}
}

// Artifact versions are 0-based, so a required dependency at v0 whose
// producing node never wrote anything must not read as "persisted v0".
func TestValidateArtifactContractsRefusesAbsentRequiredDependency(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-dep", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
		Dependencies: []store.ArtifactDependency{
			{LogicalRef: "plan", NodeID: "planner", Version: 0, Required: true},
		},
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err == nil {
		t.Fatal("required dependency absent from the run accepted")
	}

	// Present at the required version: admitted.
	run.ArtifactIndex["planner"] = 0
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("satisfied dependency refused: %v", err)
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
