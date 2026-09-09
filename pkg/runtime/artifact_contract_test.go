package runtime

import (
	"bytes"
	"context"
	"errors"
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

// seedContractRun parks a resumable run carrying one published artifact whose
// contract is described by mutate, under the `enforce` context policy.
func seedContractRun(t *testing.T, id string, mutate func(*store.ArtifactContract)) (store.RunStore, *ir.Workflow) {
	t.Helper()
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, id, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.WorkflowHash = "rev-persisted"
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	run.Checkpoint = &store.Checkpoint{NodeID: "writer", Outputs: map[string]map[string]any{}}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	contract := &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-persisted", Version: 0,
	}
	mutate(contract)
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: id, NodeID: "writer", Version: 0, Contract: contract, Data: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	return s, &ir.Workflow{
		Name: "wf", Entry: "writer",
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report", Command: "true"},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "writer", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}
}

// TestResumeSurfacesTheSourceHashErrorFirst: one edit trips both the run-level
// source-hash guard and the artifact contract, and ONLY the hash error names
// `--force`, the flag that unblocks it. Leading with the contract refusal sent
// the operator looking for a "migrate the artifact contract" command that does
// not exist.
func TestResumeSurfacesTheSourceHashErrorFirst(t *testing.T) {
	s, wf := seedContractRun(t, "resume-hash-first", func(c *store.ArtifactContract) {})
	eng := New(wf, s, newStubExecutor(), WithWorkflowHash("rev-edited"))

	err := eng.Resume(context.Background(), "resume-hash-first", nil)
	if err == nil {
		t.Fatal("resume against an edited source was accepted")
	}
	if !IsWorkflowSourceChanged(err) {
		t.Fatalf("resume reported %v, want the source-hash error that names --force", err)
	}
}

// TestResumeHintNamesTheRecoveryThatFitsTheViolation: `--force` waives
// producer-revision drift and nothing else, so a hint naming it for a
// publish-name or schema change walks the operator in a circle. That class is
// what `iterion rewind` is for.
func TestResumeHintNamesTheRecoveryThatFitsTheViolation(t *testing.T) {
	t.Run("revision drift points at --force", func(t *testing.T) {
		// The desync restampWorkflowSource leaves behind: the RUN's hash was
		// refreshed by an earlier forced resume, while this artifact keeps the
		// revision it was written under. The run-level check therefore passes
		// and the per-artifact one is the only signal left.
		s, wf := seedContractRun(t, "resume-hint-revision", func(c *store.ArtifactContract) {
			c.ProducerRevision = "rev-older"
		})
		eng := New(wf, s, newStubExecutor(), WithWorkflowHash("rev-persisted"))

		err := eng.Resume(context.Background(), "resume-hint-revision", nil)
		var rt *RuntimeError
		if !errors.As(err, &rt) {
			t.Fatalf("resume error = %v, want a typed RuntimeError", err)
		}
		if !strings.Contains(rt.Hint, "--force") {
			t.Errorf("hint = %q, want it to name --force", rt.Hint)
		}
	})

	t.Run("a publish-name change points at rewind", func(t *testing.T) {
		s, wf := seedContractRun(t, "resume-hint-publish", func(c *store.ArtifactContract) {
			c.LogicalRef = "renamed-since"
		})
		eng := New(wf, s, newStubExecutor(), WithWorkflowHash("rev-persisted"))

		err := eng.Resume(context.Background(), "resume-hint-publish", nil)
		var rt *RuntimeError
		if !errors.As(err, &rt) {
			t.Fatalf("resume error = %v, want a typed RuntimeError", err)
		}
		if !strings.Contains(rt.Hint, "iterion rewind") {
			t.Errorf("hint = %q, want it to name `iterion rewind`", rt.Hint)
		}
		if strings.Contains(rt.Hint, "--force to accept") {
			t.Errorf("hint = %q offers --force, which does not waive a publish-name change", rt.Hint)
		}
	})
}

// mislabelingStore simulates a store handing back an object that is not the
// one asked for — a key collision, a bad migration, a hand-edited artifact.
type mislabelingStore struct {
	store.RunStore
	nodeID string
}

func (m *mislabelingStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	a, err := m.RunStore.LoadArtifact(ctx, runID, nodeID, version)
	if a != nil {
		a.NodeID = m.nodeID
	}
	return a, err
}

// TestValidateArtifactContractsBindsTheOutputToItsProducer: binding an output
// to its producer is the contract's stated purpose, but only non-emptiness
// was checked — the workflow node was then resolved by the artifact-index KEY,
// so a contract naming a different producer was validated against the wrong
// node's declaration and admitted, and the loaded body's own identity was
// never compared to the one requested.
func TestValidateArtifactContractsBindsTheOutputToItsProducer(t *testing.T) {
	ctx := context.Background()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
		"other":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "other"}, Publish: "other-report"},
	}}
	seed := func(t *testing.T, id, producer string) (store.RunStore, *store.Run) {
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
			RunID: id, NodeID: "writer", Version: 0,
			Contract: &store.ArtifactContract{LogicalRef: "report", ProducerNode: producer, Version: 0},
			Data:     map[string]any{"ok": true},
		}); err != nil {
			t.Fatal(err)
		}
		return s, run
	}

	t.Run("contract names a producer other than the index key", func(t *testing.T) {
		s, run := seed(t, "artifact-identity-producer", "other")
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: wf})
		if err == nil {
			t.Fatal("enforce admitted a contract naming a different producer")
		}
		if !strings.Contains(err.Error(), "names producer") {
			t.Fatalf("error = %v, want it to report the producer mismatch", err)
		}
	})

	t.Run("the loaded body belongs to another node", func(t *testing.T) {
		s, run := seed(t, "artifact-identity-body", "writer")
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: &mislabelingStore{RunStore: s, nodeID: "other"}, Run: run, Workflow: wf,
		})
		if err == nil {
			t.Fatal("enforce admitted an artifact body belonging to another node")
		}
		if !strings.Contains(err.Error(), "loaded as") {
			t.Fatalf("error = %v, want it to report the identity mismatch", err)
		}
	})
}

// TestValidateArtifactContractsResolvesDependencyByLogicalRef: a dependency
// that names only its logical ref used to be looked up in run.ArtifactIndex,
// which is keyed by NODE ID — so a present artifact was reported "absent from
// the run" purely because its publish name and its node id differ.
func TestValidateArtifactContractsResolvesDependencyByLogicalRef(t *testing.T) {
	ctx := context.Background()
	// The producer's node id ("planner") differs from its publish name
	// ("plan"), which is the ordinary case and the one that broke.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
	}}
	seed := func(t *testing.T, id string, dep store.ArtifactDependency, withPlan bool) (store.RunStore, *store.Run) {
		t.Helper()
		s := tmpStore(t)
		run, err := s.CreateRun(ctx, id, "wf", nil)
		if err != nil {
			t.Fatal(err)
		}
		run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
		if err := s.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if withPlan {
			if err := s.WriteArtifact(ctx, &store.Artifact{
				RunID: id, NodeID: "planner", Version: 0,
				Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "planner", Version: 0},
				Data:     map[string]any{"ok": true},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.WriteArtifact(ctx, &store.Artifact{
			RunID: id, NodeID: "writer", Version: 0,
			Contract: &store.ArtifactContract{
				LogicalRef: "report", ProducerNode: "writer", Version: 0,
				Dependencies: []store.ArtifactDependency{dep},
			},
			Data: map[string]any{"ok": true},
		}); err != nil {
			t.Fatal(err)
		}
		reloaded, err := s.LoadRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return s, reloaded
	}

	t.Run("a present dependency named by ref is accepted", func(t *testing.T) {
		s, run := seed(t, "artifact-dep-ref", store.ArtifactDependency{LogicalRef: "plan", Version: 0, Required: true}, true)
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: wf}); err != nil {
			t.Fatalf("a dependency that IS present was refused: %v", err)
		}
	})

	t.Run("a genuinely absent dependency is still refused", func(t *testing.T) {
		s, run := seed(t, "artifact-dep-absent", store.ArtifactDependency{LogicalRef: "plan", Version: 0, Required: true}, false)
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: wf})
		if err == nil || !strings.Contains(err.Error(), "absent from the run") {
			t.Fatalf("missing dependency error = %v", err)
		}
	})

	t.Run("a ref no node publishes is reported as such", func(t *testing.T) {
		s, run := seed(t, "artifact-dep-unknown", store.ArtifactDependency{LogicalRef: "ghost", Version: 0, Required: true}, false)
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{Store: s, Run: run, Workflow: wf})
		if err == nil || !strings.Contains(err.Error(), "no node of this workflow publishes") {
			t.Fatalf("unknown logical ref error = %v", err)
		}
	})
}
