package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// A contract claiming another producer or another version is a misbinding,
// and is only visible when the redundant fields are compared to the identity
// the artifact was addressed by rather than to the blob's own copy.
func TestValidateArtifactContractsRefusesMisboundIdentity(t *testing.T) {
	ctx := context.Background()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	for name, contract := range map[string]*store.ArtifactContract{
		"another producer": {LogicalRef: "report", ProducerNode: "planner", Version: 0},
		"another version":  {LogicalRef: "report", ProducerNode: "writer", Version: 7},
	} {
		t.Run(name, func(t *testing.T) {
			s, run := seedContractRun(t, "artifact-misbound", contract)
			if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
				Store: s, Run: run, Workflow: wf, Revision: "rev-new",
			}); err == nil {
				t.Fatalf("contract claiming %s accepted", name)
			}
		})
	}
}

// The report tier exists so a deployment can measure the enforce flip
// before making it. Returning nil in silence made it inert: nothing
// downstream ever learned a resume would have been refused.
func TestValidateArtifactContractsReportsWhatEnforceWouldRefuse(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-report", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	run.ExecutionContext.Policy = store.ContextPolicyReport
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "summary"},
	}}
	var out bytes.Buffer
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
		Logger: iterlog.New(iterlog.LevelWarn, &out),
	}); err != nil {
		t.Fatalf("report policy refused the resume: %v", err)
	}
	if !strings.Contains(out.String(), "would be refused under the enforce policy") ||
		!strings.Contains(out.String(), `is now published as "summary"`) {
		t.Fatalf("report tier said nothing about what enforce would refuse; log = %q", out.String())
	}
}

// An artifact carrying NO contract at all is accepted. Deliberately under
// the enforce policy: a run with no execution context returns before reading
// anything, so a legacy-policy fixture would pass this without ever
// exercising the no-contract path it exists to cover.
func TestValidateArtifactContractsIgnoresLegacyArtifact(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-legacy", "wf", nil)
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
	if err := s.WriteArtifact(ctx, &store.Artifact{RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: &ir.Workflow{}, Revision: "rev",
	}); err != nil {
		t.Fatalf("legacy artifact refused: %v", err)
	}
}

// The gate is only as good as the stamping half, and nothing else covers
// it: every other test here hand-builds a store.Artifact. A regression
// making artifactContractFor return nil — nodePublish changing shape, a
// write site dropping the field in a merge — would silently disable the
// whole feature with every test still green, and the failure would only
// surface as an operator's enforce run quietly catching nothing.
func TestEngineStampsContractOnPublishedArtifacts(t *testing.T) {
	ctx := context.Background()
	wf := &ir.Workflow{
		Name:  "contract_stamp",
		Entry: "make_note",
		Nodes: map[string]ir.Node{
			"make_note": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "make_note"},
				SchemaFields: ir.SchemaFields{OutputSchema: "Note"},
				Command:      "noop",
				Publish:      "note_artifact",
			},
			"unpublished": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "unpublished"}, Command: "noop"},
			"done":        &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "make_note", To: "unpublished"},
			{From: "unpublished", To: "done"},
		},
		Schemas: map[string]*ir.Schema{
			"Note": {Name: "Note", Fields: []*ir.SchemaField{{Name: "msg", Type: ir.FieldTypeString}}},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	exec := newStubExecutor()
	exec.on("make_note", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"msg": "hi"}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec, WithWorkflowHash("rev-stamped"))
	if err := eng.Run(ctx, "run-contract-stamp", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	art, err := s.LoadArtifact(ctx, "run-contract-stamp", "make_note", 0)
	if err != nil {
		t.Fatalf("load published artifact: %v", err)
	}
	got := art.Contract
	if got == nil {
		t.Fatal("published artifact carries no contract — the enforce gate has nothing to check")
	}
	if got.LogicalRef != "note_artifact" {
		t.Errorf("LogicalRef = %q, want the node's publish name", got.LogicalRef)
	}
	if got.ProducerNode != "make_note" || got.Version != 0 {
		t.Errorf("identity = %s/v%d, want make_note/v0", got.ProducerNode, got.Version)
	}
	if got.ProducerRevision != "rev-stamped" {
		t.Errorf("ProducerRevision = %q, want the run's workflow hash", got.ProducerRevision)
	}
	if got.Schema != "Note" || got.SchemaFingerprint != schemaFingerprint(wf, "Note") {
		t.Errorf("schema = %q/%q, want Note fingerprinted from the resolved body", got.Schema, got.SchemaFingerprint)
	}

	// And the run that just wrote them resumes on its own artifacts.
	run, err := s.LoadRun(ctx, "run-contract-stamp")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-stamped",
	}); err != nil {
		t.Fatalf("the engine's own freshly written artifacts failed the gate: %v", err)
	}
}

// The resume half of the same case: a deleted node's artifact is inert
// history — rebuildArtifacts only maps nodes the workflow declares — so it
// must not refuse the resume either.
func TestValidateArtifactContractsAdmitsArtifactOfDeletedNode(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-deleted-node", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	// The workflow no longer declares "writer" at all.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"other": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "other"}},
	}}
	var out bytes.Buffer
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: wf, Revision: "rev-new",
		Logger: iterlog.New(iterlog.LevelWarn, &out),
	}); err != nil {
		t.Fatalf("artifact of a deleted node refused the resume: %v", err)
	}
	if !strings.Contains(out.String(), "no longer declares") {
		t.Fatalf("the drift was not even reported; log = %q", out.String())
	}
}

// loadFailingStore fails every artifact read with a fixed error.
type loadFailingStore struct {
	store.RunStore
	err error
}

func (f loadFailingStore) LoadArtifact(context.Context, string, string, int) (*store.Artifact, error) {
	return nil, f.err
}

// A cancelled or timed-out read is an operational failure, not a verdict on
// the contract: Engine.Resume stamps a contract violation RESUME_INVALID
// with a "restore the declaration" hint, which would send the operator after
// a problem that does not exist.
func TestValidateArtifactContractsSeparatesCancellationFromIncompatibility(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-cancelled", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"},
	}}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: loadFailingStore{RunStore: s, err: cause}, Run: run, Workflow: wf, Revision: "rev-new",
		})
		if !errors.Is(err, cause) {
			t.Fatalf("error = %v, want it to carry %v", err, cause)
		}
		if strings.Contains(err.Error(), "artifact contract incompatible") {
			t.Fatalf("a cancellation was reported as an incompatible contract: %v", err)
		}
	}
	// An ordinary read failure stays a contract violation, so an enforce
	// run still fails closed on an unreadable store.
	err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: loadFailingStore{RunStore: s, err: errors.New("s3: 503 slow down")}, Run: run, Workflow: wf, Revision: "rev-new",
	})
	if err == nil || !strings.Contains(err.Error(), "artifact contract incompatible") {
		t.Fatalf("unreadable artifact = %v, want the enforce refusal", err)
	}
}

// The legacy policy — the default until ITERION_EXECUTION_CONTEXT_POLICY says
// otherwise — means the regime does not apply, so the gate must not read a
// single artifact: that is an S3 GET per published node on cloud, on every
// resume and every usage-window retry, for a verdict nobody acts on.
func TestValidateArtifactContractsReadsNothingUnderLegacyPolicy(t *testing.T) {
	ctx := context.Background()
	s, run := seedContractRun(t, "artifact-legacy-policy", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
	})
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "summary"},
	}}
	// A store that fails every read: reaching it at all is the failure.
	failing := loadFailingStore{RunStore: s, err: errors.New("read should not happen under the legacy policy")}
	for _, ec := range []*store.ExecutionContext{nil, {Version: 1, Policy: store.ContextPolicyLegacy}} {
		run.ExecutionContext = ec
		if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
			Store: failing, Run: run, Workflow: wf, Revision: "rev-new",
		}); err != nil {
			t.Fatalf("execution context %+v: %v", ec, err)
		}
	}
}

// The stamping half and the gate are written against the same fields, which
// is exactly why they can agree in a unit test and disagree on a real run —
// a version read one side of an increment, a schema resolved from a
// different workflow copy. So drive both through the engine: publish under
// an enforce context, fail, and resume. If the pair ever diverges, an
// operator's enforce run stops resuming on artifacts iterion wrote itself,
// and that is the failure this pins.
func TestEnforceContextResumesOnTheArtifactsItWrote(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "enforce-roundtrip"
	run, err := s.CreateRun(ctx, runID, "contract_roundtrip", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "rev-e2e"
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
		Workflow: store.WorkflowContext{WorkflowRevision: "rev-e2e"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	wf := &ir.Workflow{
		Name:  "contract_roundtrip",
		Entry: "survey",
		Nodes: map[string]ir.Node{
			"survey": &ir.ToolNode{
				BaseNode:     ir.BaseNode{ID: "survey"},
				SchemaFields: ir.SchemaFields{OutputSchema: "Note"},
				Command:      "noop",
				Publish:      "survey_report",
			},
			"flaky": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "flaky"}, Command: "noop"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "survey", To: "flaky"}, {From: "flaky", To: "done"}},
		Schemas: map[string]*ir.Schema{
			"Note": {Name: "Note", Fields: []*ir.SchemaField{{Name: "msg", Type: ir.FieldTypeString}}},
		},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	failFirst := true
	exec := newStubExecutor()
	exec.on("survey", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"msg": "surveyed"}, nil
	})
	exec.on("flaky", func(_ map[string]any) (map[string]any, error) {
		if failFirst {
			failFirst = false
			return nil, errors.New("transient boom")
		}
		return map[string]any{"msg": "recovered"}, nil
	})

	if err := New(wf, s, exec, WithWorkflowHash("rev-e2e")).Run(ctx, runID, nil); err == nil {
		t.Fatal("the seeded node failure did not fail the run")
	}
	parked, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable so the resume path is the one under test", parked.Status)
	}
	if _, ok := parked.ArtifactIndex["survey"]; !ok {
		t.Fatal("the published artifact never reached the index, so the gate would have nothing to read")
	}

	if err := New(wf, s, exec, WithWorkflowHash("rev-e2e")).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("enforce resume refused the artifacts the same engine wrote: %v", err)
	}
	finished, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", finished.Status)
	}
}

// A digest from another generation of the algorithm is UNKNOWN, not unequal.
// Without this, changing what goes into the fingerprint would refuse every
// artifact already on disk, fleet-wide, at the next engine upgrade.
func TestValidateArtifactContractsSkipsAForeignFingerprintGeneration(t *testing.T) {
	ctx := context.Background()
	written := reportSchema(&ir.SchemaField{Name: "summary", Type: ir.FieldTypeString})
	if !strings.HasPrefix(schemaFingerprint(written, "Report"), schemaFingerprintPrefix) {
		t.Fatal("the fingerprint carries no generation tag, so it cannot be evolved safely")
	}
	s, run := seedContractRun(t, "artifact-fp-generation", &store.ArtifactContract{
		LogicalRef: "report", ProducerNode: "writer", ProducerRevision: "rev-new", Version: 0,
		Schema: "Report", SchemaFingerprint: "v0:whatever-the-previous-generation-emitted",
	})
	// A body that genuinely differs: the comparison must still be skipped,
	// because a digest we cannot recompute proves nothing either way.
	other := reportSchema(&ir.SchemaField{Name: "summary", Type: ir.FieldTypeInt})
	if err := ValidateArtifactContracts(ctx, ArtifactContractCheck{
		Store: s, Run: run, Workflow: other, Revision: "rev-new",
	}); err != nil {
		t.Fatalf("a fingerprint from another generation was treated as unequal: %v", err)
	}
}
