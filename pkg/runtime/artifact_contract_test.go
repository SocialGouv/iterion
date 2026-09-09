package runtime

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// countingArtifactStore counts artifact-body reads so a test can assert that
// a policy which cannot act on the result does not pay for them.
type countingArtifactStore struct {
	store.RunStore
	loads int
}

func (c *countingArtifactStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	c.loads++
	return c.RunStore.LoadArtifact(ctx, runID, nodeID, version)
}

// TestValidateArtifactContractsLegacyPolicyReadsNothing pins the ordering:
// the context policy is resolved BEFORE any artifact is loaded. `legacy` is
// the default for every run predating the contract and can neither refuse nor
// report, so one full artifact-body read per published node — an S3 GET each
// on the cloud store, three times per resume across the call sites — would
// buy nothing at all.
func TestValidateArtifactContractsLegacyPolicyReadsNothing(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-legacy-policy", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	if err := base.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{LogicalRef: "gone", ProducerNode: "writer", Version: 0},
		Data:     map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	counting := &countingArtifactStore{RunStore: base}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}},
	}}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: counting, Run: run, Workflow: wf, CurrentRevision: "rev",
	}); err != nil {
		t.Fatalf("legacy policy refused: %v", err)
	}
	if counting.loads != 0 {
		t.Errorf("legacy policy performed %d artifact read(s), want 0", counting.loads)
	}

	// enforce, by contrast, must read — and refuse.
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: counting, Run: run, Workflow: wf, CurrentRevision: "rev",
	}); err == nil {
		t.Fatal("enforce accepted an artifact whose publish reference is gone")
	}
	if counting.loads == 0 {
		t.Error("enforce performed no artifact read")
	}
}

// TestValidateArtifactContractsFailsClosedOnUnreadableArtifact: an integrity
// policy must not read "I could not fetch the contract" as "the contract is
// compatible". On the cloud store LoadArtifact IS the authoritative S3 GET,
// so an outage, an authz failure or a corrupt object arrived here as a silent
// skip — enforce resumed having verified nothing.
func TestValidateArtifactContractsFailsClosedOnUnreadableArtifact(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-unreadable", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The index names a version that was never persisted: LoadArtifact errors.
	run.ArtifactIndex = map[string]int{"writer": 7}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	err = ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, CurrentRevision: "rev",
	})
	if err == nil {
		t.Fatal("enforce admitted a resume whose persisted contract could not be read")
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("error does not name the unreadable artifact: %v", err)
	}
}

// TestValidateArtifactContractsReportPolicyIsObservable: `report` is the
// migration probe an operator runs before flipping a deployment to `enforce`.
// It used to return nil in silence, which made it a policy that paid for
// every artifact read and answered nothing.
func TestValidateArtifactContractsReportPolicyIsObservable(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-report", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{LogicalRef: "report", ProducerNode: "writer", Version: 0},
		Data:     map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "renamed"},
	}}
	var buf bytes.Buffer
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, CurrentRevision: "rev",
		Logger: iterlog.New(iterlog.LevelWarn, &buf),
	}); err != nil {
		t.Fatalf("report policy refused the resume: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "renamed") || !strings.Contains(got, run.ID) {
		t.Errorf("report policy recorded nothing actionable, log = %q", got)
	}
}

// TestValidateArtifactContractsComparesSchemaShapeNotName is the guard for
// the incompatibility that actually matters. `ir.NodeOutputSchema` returns
// the schema REFERENCE NAME, so editing a schema's fields — removing the
// field a downstream node reads as `outputs.x.field`, i.e. exactly the change
// that invalidates a persisted artifact — kept the name and sailed through
// `enforce`; while renaming a schema whose body was unchanged was refused for
// a shape that never moved.
func TestValidateArtifactContractsComparesSchemaShapeNotName(t *testing.T) {
	ctx := context.Background()

	// The workflow that produced the artifact: schema `out` with two fields.
	produced := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
				SchemaFields: ir.SchemaFields{OutputSchema: "out"},
			},
		},
		Schemas: map[string]*ir.Schema{"out": {Name: "out", Fields: []*ir.SchemaField{
			{Name: "value", Type: ir.FieldTypeString},
			{Name: "ok", Type: ir.FieldTypeBool},
		}}},
	}
	producedHash := schemaFingerprint(produced, "out")
	if producedHash == "" {
		t.Fatal("fixture: schema fingerprint is empty")
	}

	seed := func(t *testing.T, id string) (store.RunStore, *store.Run) {
		t.Helper()
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, id, "wf", nil)
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
				LogicalRef: "report", ProducerNode: "writer", Version: 0,
				Schema: "out", SchemaHash: producedHash,
			},
			Data: map[string]any{"value": "v", "ok": true},
		}); err != nil {
			t.Fatal(err)
		}
		return s, run
	}

	t.Run("same name, a field removed, refused", func(t *testing.T) {
		s, run := seed(t, "artifact-schema-edited")
		edited := &ir.Workflow{
			Nodes: produced.Nodes,
			Schemas: map[string]*ir.Schema{"out": {Name: "out", Fields: []*ir.SchemaField{
				{Name: "value", Type: ir.FieldTypeString},
			}}},
		}
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: edited})
		if err == nil {
			t.Fatal("enforce admitted an artifact whose schema lost a field")
		}
		if !strings.Contains(err.Error(), "different definition") {
			t.Fatalf("error does not name the definition drift: %v", err)
		}
	})

	t.Run("renamed schema, identical body, admitted", func(t *testing.T) {
		s, run := seed(t, "artifact-schema-renamed")
		renamed := &ir.Workflow{
			Nodes: map[string]ir.Node{
				"writer": &ir.ToolNode{
					BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
					SchemaFields: ir.SchemaFields{OutputSchema: "result"},
				},
			},
			Schemas: map[string]*ir.Schema{"result": {Name: "result", Fields: []*ir.SchemaField{
				// Declared in the other order too: an artifact is a JSON
				// object keyed by field name, so order is not shape.
				{Name: "ok", Type: ir.FieldTypeBool},
				{Name: "value", Type: ir.FieldTypeString},
			}}},
		}
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: renamed}); err != nil {
			t.Fatalf("enforce refused a schema rename that changed no shape: %v", err)
		}
	})

	t.Run("legacy contract without a fingerprint falls back to the name", func(t *testing.T) {
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, "artifact-schema-legacy", "wf", nil)
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
				LogicalRef: "report", ProducerNode: "writer", Version: 0, Schema: "out",
			},
			Data: map[string]any{"ok": true},
		}); err != nil {
			t.Fatal(err)
		}
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: produced}); err != nil {
			t.Fatalf("a pre-fingerprint contract with a matching name was refused: %v", err)
		}
	})
}
