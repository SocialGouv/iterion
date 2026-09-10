package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
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

func TestForcedArtifactCompatibilitySurvivesWorkflowRestamp(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-forced-migration", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "rev-old"
	run.WorkflowSource = "old source"
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		Workflow: store.WorkflowContext{WorkflowRevision: "rev-old"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0,
		Contract: &store.ArtifactContract{
			LogicalRef: "old-report", ProducerNode: "writer", ProducerRevision: "rev-old", Version: 0,
		},
		Data: map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new-report"},
	}}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "rev-new", true); err != nil {
		t.Fatalf("forced migration preflight: %v", err)
	}
	eng := &Engine{
		store: s, workflowHash: "rev-new", workflowSource: "new source", forceResume: true,
	}
	eng.restampWorkflowSource(ctx, run)
	persisted, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WorkflowHash != "rev-new" || persisted.ArtifactCompatibilityRevision != "rev-new" {
		t.Fatalf("restamped migration = hash %q compatibility %q", persisted.WorkflowHash, persisted.ArtifactCompatibilityRevision)
	}
	if persisted.ExecutionContext == nil || persisted.ExecutionContext.Workflow.WorkflowRevision != "rev-new" {
		t.Fatalf("restamped execution context = %+v, want workflow revision rev-new", persisted.ExecutionContext)
	}
	if err := ValidateArtifactContracts(ctx, s, persisted, wf, "rev-new", false); err != nil {
		t.Fatalf("ordinary resume demanded --force again after accepted migration: %v", err)
	}
}

func TestForcedArtifactCompatibilityClearsStaleSourceWithoutNewSourceText(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-cloud-migration", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "rev-old"
	run.WorkflowSource = "old source"
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		Workflow: store.WorkflowContext{WorkflowRevision: "rev-old"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{store: s, workflowHash: "rev-new", forceResume: true}
	eng.restampWorkflowSource(ctx, run)
	persisted, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WorkflowHash != "rev-new" || persisted.ArtifactCompatibilityRevision != "rev-new" {
		t.Fatalf("hash-only migration = hash %q compatibility %q", persisted.WorkflowHash, persisted.ArtifactCompatibilityRevision)
	}
	if persisted.WorkflowSource != "" {
		t.Fatalf("hash-only migration kept stale workflow source %q", persisted.WorkflowSource)
	}
	if got := persisted.ExecutionContext.Workflow.WorkflowRevision; got != "rev-new" {
		t.Fatalf("execution context revision = %q, want rev-new", got)
	}
}

func TestForcedArtifactCompatibilityKeepsSourceAtUnchangedHash(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-same-revision", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "rev-current"
	run.WorkflowSource = "current source"
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := &Engine{store: s, workflowHash: "rev-current", forceResume: true}
	eng.restampWorkflowSource(ctx, run)
	persisted, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.WorkflowSource != "current source" {
		t.Fatalf("unchanged forced resume discarded workflow source %q", persisted.WorkflowSource)
	}
	if persisted.WorkflowHash != "rev-current" || persisted.ArtifactCompatibilityRevision != "rev-current" {
		t.Fatalf("unchanged migration = hash %q compatibility %q", persisted.WorkflowHash, persisted.ArtifactCompatibilityRevision)
	}
}

type artifactReadErrorStore struct{ store.RunStore }

func (artifactReadErrorStore) LoadArtifact(context.Context, string, string, int) (*store.Artifact, error) {
	return nil, errors.New("blob unavailable")
}

type artifactBlockingEventStore struct {
	store.RunStore
	sawDeadline bool
}

func (s *artifactBlockingEventStore) AppendEvent(ctx context.Context, _ string, _ store.Event) (*store.Event, error) {
	_, s.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

type artifactNodeReadErrorStore struct {
	store.RunStore
	nodeID string
}

func (s artifactNodeReadErrorStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	if nodeID == s.nodeID {
		return nil, errors.New("blob unavailable")
	}
	return s.RunStore.LoadArtifact(ctx, runID, nodeID, version)
}

func TestValidateArtifactContractsFailsClosedOnUnreadableEnforcedArtifact(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-unreadable", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
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

func TestArtifactContractDoesNotClaimUnverifiedConsumedRevision(t *testing.T) {
	consumer := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
		CommandRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{"writer": consumer}}}
	rs := &runState{
		artifacts: map[string]map[string]any{"plan": {"ok": true}},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "planner", Version: 1, Unverified: true},
		},
	}
	contract := eng.artifactContractFor("writer", consumer, 0, rs)
	if contract == nil || len(contract.Dependencies) != 0 {
		t.Fatalf("unverified artifact became a durable dependency: %+v", contract)
	}
}

func TestArtifactContractConservativelyTracksDynamicArtifactIndex(t *testing.T) {
	indexed, err := expr.Parse(`artifacts[vars.name]`)
	if err != nil {
		t.Fatal(err)
	}
	consumer := &ir.ComputeNode{
		BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report",
		Exprs: []*ir.ComputeExpr{{Key: "selected", AST: indexed}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"notes":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "notes"}, Publish: "notes"},
		"writer":  consumer,
	}}}
	rs := &runState{
		artifacts: map[string]map[string]any{
			"plan": {"ok": true}, "notes": {"ok": true},
		},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan":  {NodeID: "planner", Version: 2},
			"notes": {NodeID: "notes", Version: 4},
		},
	}
	contract := eng.artifactContractFor("writer", consumer, 0, rs)
	if contract == nil || len(contract.Dependencies) != 2 {
		t.Fatalf("dynamic artifact dependencies = %+v", contract)
	}
	if contract.Dependencies[0].LogicalRef != "notes" || contract.Dependencies[1].LogicalRef != "plan" {
		t.Fatalf("dynamic artifact dependencies are incomplete or unstable: %+v", contract.Dependencies)
	}
}

func TestValidateCheckpointArtifactAvailabilityOnlyEnforcesEnforcePolicy(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-availability", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Checkpoint = &store.Checkpoint{ArtifactRevisions: map[string]store.ArtifactRevisionRef{
		"plan": {NodeID: "planner", Version: 0},
	}}
	for _, policy := range []store.ContextPolicy{store.ContextPolicyLegacy, store.ContextPolicyReport} {
		run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: policy}
		if err := ValidateCheckpointArtifactAvailability(ctx, artifactReadErrorStore{base}, run); err != nil {
			t.Fatalf("%s policy refused unavailable observational artifact: %v", policy, err)
		}
	}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	err = ValidateCheckpointArtifactAvailability(ctx, artifactReadErrorStore{base}, run)
	if err == nil || !errors.Is(err, ErrArtifactContractUnavailable) || !strings.Contains(err.Error(), "blob unavailable") {
		t.Fatalf("enforce-policy availability error = %v", err)
	}
}

func TestPrepareResumeArtifactsFallsBackToCheckpointOutsideEnforce(t *testing.T) {
	for _, policy := range []store.ContextPolicy{store.ContextPolicyLegacy, store.ContextPolicyReport} {
		t.Run(string(policy), func(t *testing.T) {
			base := tmpStore(t)
			run := &store.Run{ID: "artifact-fallback", ExecutionContext: &store.ExecutionContext{Version: 1, Policy: policy}}
			cp := &store.Checkpoint{
				Outputs:        map[string]map[string]any{"writer": {"value": "checkpoint"}},
				Artifacts:      map[string]map[string]any{"plan": {"value": "checkpoint"}},
				ArtifactsKnown: true,
				ArtifactRevisions: map[string]store.ArtifactRevisionRef{
					"plan": {NodeID: "writer", Version: 0},
				},
				ArtifactRevisionsKnown: true,
			}
			wf := &ir.Workflow{Nodes: map[string]ir.Node{
				"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "plan"},
			}}
			eng := New(wf, artifactReadErrorStore{base}, newStubExecutor())
			state, err := eng.prepareResumeArtifacts(context.Background(), run, cp)
			if err != nil {
				t.Fatalf("prepare %s resume artifacts: %v", policy, err)
			}
			if got := state.artifacts["plan"]["value"]; got != "checkpoint" {
				t.Fatalf("%s fallback artifact value = %v, want checkpoint", policy, got)
			}
			if policy == store.ContextPolicyReport && !state.revisions["plan"].Unverified {
				t.Fatalf("report fallback claimed verified provenance: %+v", state.revisions)
			}
		})
	}
}

func TestPrepareLegacyResumeUsesCheckpointProducerWithoutArtifactRead(t *testing.T) {
	base := tmpStore(t)
	reads := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 0}
	run := &store.Run{
		ID:               "artifact-legacy-shared-publish",
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyLegacy},
	}
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{
			"a": {"value": "checkpoint-selected"},
			"z": {"value": "stale-alphabetical-winner"},
		},
		Artifacts:      map[string]map[string]any{"plan": {"value": "checkpoint-selected"}},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "a", Version: 0},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "plan"},
		"z": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "z"}, Publish: "plan"},
	}}
	eng := New(wf, reads, newStubExecutor())
	state, err := eng.prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["plan"]["value"]; got != "checkpoint-selected" {
		t.Fatalf("legacy shared-publish value = %v, want checkpoint producer output", got)
	}
	if got := state.revisions["plan"]; got.NodeID != "a" || got.Version != 0 {
		t.Fatalf("legacy shared-publish provenance = %+v", got)
	}
	if reads.loads != 0 {
		t.Fatalf("legacy resume artifact reads = %d, want zero", reads.loads)
	}
}

func TestPrepareLegacyOldCheckpointRetainsOwnershipWithoutVerifiedProvenance(t *testing.T) {
	base := tmpStore(t)
	reads := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 0}
	run := &store.Run{
		ID:               "artifact-legacy-alias-versions",
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyLegacy},
	}
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{"writer": {"value": "latest"}},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"old": {NodeID: "writer", Version: 0},
			"new": {NodeID: "writer", Version: 1},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new"},
	}}
	state, err := New(wf, reads, newStubExecutor()).prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := state.artifacts["old"]; present {
		t.Fatalf("older alias received the latest checkpoint output: %+v", state.artifacts)
	}
	if _, present := state.revisions["old"]; present {
		t.Fatalf("older unloaded alias retained false provenance: %+v", state.revisions)
	}
	if got := state.artifacts["new"]["value"]; got != "latest" {
		t.Fatalf("latest alias value = %v", got)
	}
	if _, present := state.revisions["old"]; present {
		t.Fatalf("older alias retained an unsupported output binding: %+v", state.revisions)
	}
	if revision := state.revisions["new"]; revision.NodeID != "writer" || !revision.Unverified {
		t.Fatalf("latest fallback lost unverified ownership: %+v", revision)
	}
	if owner := state.owners["new"]; owner != "writer" {
		t.Fatalf("latest fallback owner = %q", owner)
	}
	if reads.loads != 0 {
		t.Fatalf("legacy resume artifact reads = %d, want zero", reads.loads)
	}
}

func TestPrepareOldCheckpointDropsWrongProvisionalProducer(t *testing.T) {
	for _, policy := range []store.ContextPolicy{store.ContextPolicyLegacy, store.ContextPolicyReport} {
		t.Run(string(policy), func(t *testing.T) {
			base := tmpStore(t)
			run := &store.Run{
				ID:               "artifact-old-checkpoint-wrong-producer",
				ExecutionContext: &store.ExecutionContext{Version: 1, Policy: policy},
			}
			cp := &store.Checkpoint{
				Outputs: map[string]map[string]any{
					"a": {"value": "a-latest"},
					"b": {"value": "wrong-provisional"},
				},
				ArtifactRevisions: map[string]store.ArtifactRevisionRef{
					"old": {NodeID: "a", Version: 0},
					"new": {NodeID: "a", Version: 1},
				},
				ArtifactRevisionsKnown: true,
			}
			wf := &ir.Workflow{Nodes: map[string]ir.Node{
				"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "new"},
				"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}, Publish: "old"},
			}}
			state, err := New(wf, artifactReadErrorStore{base}, newStubExecutor()).prepareResumeArtifacts(
				context.Background(), run, cp,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := state.artifacts["old"]; present {
				t.Fatalf("%s resume retained a value from the wrong producer: artifacts=%+v owners=%+v", policy, state.artifacts, state.owners)
			}
			if _, present := state.owners["old"]; present {
				t.Fatalf("%s resume retained the wrong provisional owner: %+v", policy, state.owners)
			}
			if policy == store.ContextPolicyLegacy {
				if _, present := state.revisions["old"]; present {
					t.Fatalf("legacy resume retained an unreconstructable revision: %+v", state.revisions)
				}
			} else if revision := state.revisions["old"]; !revision.Unverified {
				t.Fatalf("report resume claimed verified provenance: %+v", revision)
			}
		})
	}
}

func TestPrepareLegacyResumeDoesNotBindRetainedRevisionToNewUnpublishedOutput(t *testing.T) {
	base := tmpStore(t)
	reads := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 0}
	run := &store.Run{
		ID:               "artifact-legacy-forced-unpublish",
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyLegacy},
	}
	cp := &store.Checkpoint{
		Outputs:        map[string]map[string]any{"writer": {"value": "new-unpublished-output"}},
		Artifacts:      map[string]map[string]any{"plan": {"value": "retained-published-output"}},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "writer", Version: 0},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}},
	}}
	state, err := New(wf, reads, newStubExecutor()).prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["plan"]["value"]; got != "retained-published-output" {
		t.Fatalf("retained revision was rebound to a later unpublished output: %v", got)
	}
	if got := state.revisions["plan"]; got.NodeID != "writer" || got.Version != 0 {
		t.Fatalf("retained snapshot lost its exact provenance: %+v", got)
	}
	if reads.loads != 0 {
		t.Fatalf("legacy resume artifact reads = %d, want zero", reads.loads)
	}
}

func TestPrepareReportFallbackUsesCheckpointProducer(t *testing.T) {
	base := tmpStore(t)
	run := &store.Run{
		ID:               "artifact-report-shared-publish",
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport},
	}
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{
			"a": {"value": "checkpoint-selected"},
			"z": {"value": "stale-alphabetical-winner"},
		},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "a", Version: 0},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "plan"},
		"z": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "z"}, Publish: "plan"},
	}}
	state, err := New(wf, artifactReadErrorStore{base}, newStubExecutor()).prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["plan"]["value"]; got != "checkpoint-selected" {
		t.Fatalf("report shared-publish fallback = %v, want checkpoint producer output", got)
	}
	if revision := state.revisions["plan"]; !revision.Unverified || revision.NodeID != "a" {
		t.Fatalf("report fallback lost its explicitly unverified producer binding: %+v", revision)
	}
	eng := New(wf, artifactReadErrorStore{base}, newStubExecutor())
	rs := eng.newRunState(run.ID, nil)
	eng.restoreCheckpointState(rs, cp, state)
	next := buildCheckpoint(rs, "retry")
	if !next.ArtifactsKnown || next.ArtifactOwners["plan"] != "a" {
		t.Fatalf("report fallback was not persisted as an authoritative logical snapshot: artifacts=%+v owners=%+v", next.Artifacts, next.ArtifactOwners)
	}
	second, err := eng.prepareResumeArtifacts(context.Background(), run, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.artifacts["plan"]["value"]; got != "checkpoint-selected" {
		t.Fatalf("second report resume changed shared-publish fallback to %v", got)
	}
}

func TestPrepareReportResumeReverifiesRecoveredArtifactBinding(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	const runID = "artifact-report-recovered"
	if _, err := base.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "writer", Version: 0, Data: map[string]any{"value": "persisted"},
	}); err != nil {
		t.Fatal(err)
	}
	run := &store.Run{
		ID:               runID,
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport},
	}
	cp := &store.Checkpoint{
		Outputs:        map[string]map[string]any{"writer": {"value": "checkpoint-fallback"}},
		Artifacts:      map[string]map[string]any{"plan": {"value": "checkpoint-fallback"}},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "writer", Version: 0, Unverified: true},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "plan"},
	}}
	state, err := New(wf, base, newStubExecutor()).prepareResumeArtifacts(ctx, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if state.revisions["plan"].Unverified {
		t.Fatalf("recovered artifact remained unverified: %+v", state.revisions["plan"])
	}
	if got := state.artifacts["plan"]["value"]; got != "persisted" {
		t.Fatalf("recovered persisted body = %v", got)
	}
}

func TestPrepareReportResumeMarksUnavailableParallelBindingUnverified(t *testing.T) {
	base := tmpStore(t)
	run := &store.Run{
		ID:               "artifact-report-parallel-unavailable",
		ExecutionContext: &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport},
	}
	cp := &store.Checkpoint{
		Parallel: &store.ParallelCheckpoint{Branches: map[string]*store.BranchCheckpoint{
			"branch-a": {
				BranchID:  "branch-a",
				Artifacts: map[string]map[string]any{"plan": {"value": "checkpoint-fallback"}},
				ArtifactRevisions: map[string]store.ArtifactRevisionRef{
					"plan": {NodeID: "worker", Version: 0},
				},
			},
		}},
	}
	state, err := New(&ir.Workflow{}, artifactReadErrorStore{base}, newStubExecutor()).prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	branch := state.parallel.Branches["branch-a"]
	if revision := branch.ArtifactRevisions["plan"]; !revision.Unverified || revision.NodeID != "worker" {
		t.Fatalf("parallel report fallback claimed verified provenance: %+v", revision)
	}
	if branch.ArtifactOwners["plan"] != "worker" || branch.Artifacts["plan"]["value"] != "checkpoint-fallback" {
		t.Fatalf("parallel report fallback lost value ownership: owners=%+v artifacts=%+v", branch.ArtifactOwners, branch.Artifacts)
	}
	prepared := checkpointWithPreparedParallel(cp, state)
	parent := New(&ir.Workflow{}, base, newStubExecutor()).newRunState(run.ID, nil)
	result := initBranchResult(parent, "branch-a", prepared.Parallel.Branches["branch-a"])
	local := newBranchRunState(parent, prepared.Parallel.Branches["branch-a"], result)
	consumer := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "consumer"}, Publish: "report",
		CommandRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{"consumer": consumer}}}
	if contract := eng.artifactContractFor("consumer", consumer, 0, local); len(contract.Dependencies) != 0 {
		t.Fatalf("parallel unavailable binding became a durable dependency: %+v", contract.Dependencies)
	}
}

func TestArtifactContractReportEventWriteIsBounded(t *testing.T) {
	oldTimeout := artifactContractReportWriteTimeout
	artifactContractReportWriteTimeout = 20 * time.Millisecond
	t.Cleanup(func() { artifactContractReportWriteTimeout = oldTimeout })

	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-report-timeout", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport}
	run.ArtifactIndex = map[string]int{"writer": 0}
	if err := base.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{LogicalRef: "old", ProducerNode: "writer", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	blocking := &artifactBlockingEventStore{RunStore: base}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new"},
	}}
	started := time.Now()
	if err := ValidateArtifactContracts(ctx, blocking, run, wf, "", false); err != nil {
		t.Fatalf("report validation: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("report event write remained blocked for %s", elapsed)
	}
	if !blocking.sawDeadline {
		t.Fatal("report event write context had no deadline")
	}
}

func TestValidateCheckpointArtifactAvailabilityExceptSkipsInvalidatedProducer(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-availability-rewind", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"retained", "invalidated"} {
		if err := base.WriteArtifact(ctx, &store.Artifact{
			RunID: run.ID, NodeID: nodeID, Version: 0, Data: map[string]any{"node": nodeID},
		}); err != nil {
			t.Fatal(err)
		}
	}
	run.Checkpoint = &store.Checkpoint{ArtifactRevisions: map[string]store.ArtifactRevisionRef{
		"kept":    {NodeID: "retained", Version: 0},
		"removed": {NodeID: "invalidated", Version: 0},
	}}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	s := artifactNodeReadErrorStore{RunStore: base, nodeID: "invalidated"}
	if err := ValidateCheckpointArtifactAvailability(ctx, s, run); err == nil {
		t.Fatal("unreadable invalidated producer was unexpectedly available")
	}
	if err := ValidateCheckpointArtifactAvailabilityExcept(ctx, s, run, map[string]bool{"invalidated": true}); err != nil {
		t.Fatalf("rewind availability guard did not exclude invalidated producer: %v", err)
	}
}

func TestResumeRejectsUnavailableExactParallelArtifactBeforeClaim(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	run, err := base.CreateRun(ctx, "artifact-parallel-unavailable", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	run.Checkpoint = &store.Checkpoint{
		NodeID: "router",
		Parallel: &store.ParallelCheckpoint{
			Branches: map[string]*store.BranchCheckpoint{
				"branch": {ArtifactRevisions: map[string]store.ArtifactRevisionRef{
					"plan": {NodeID: "planner", Version: 0},
				}},
			},
		},
	}
	if err := base.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	s := artifactNodeReadErrorStore{RunStore: base, nodeID: "planner"}
	err = New(&ir.Workflow{}, s, newStubExecutor()).Resume(ctx, run.ID, nil)
	if err == nil || !errors.Is(err, ErrArtifactContractUnavailable) {
		t.Fatalf("resume with unavailable branch artifact error = %v", err)
	}
	persisted, loadErr := base.LoadRun(ctx, run.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.Status != store.RunStatusFailedResumable {
		t.Fatalf("availability failure claimed the run: status=%s", persisted.Status)
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

func TestValidateArtifactContractsUsesCheckpointProducerForLogicalDependency(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-checkpoint-producer", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce}
	for _, artifact := range []*store.Artifact{
		{
			RunID: run.ID, NodeID: "z_selected", Version: 0,
			Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "z_selected", Version: 0},
		},
		{
			RunID: run.ID, NodeID: "consumer", Version: 0,
			Contract: &store.ArtifactContract{
				LogicalRef: "result", ProducerNode: "consumer", Version: 0,
				Dependencies: []store.ArtifactDependency{{LogicalRef: "plan", Version: 0, Required: true}},
			},
		},
	} {
		if err := s.WriteArtifact(ctx, artifact); err != nil {
			t.Fatal(err)
		}
	}
	run.Checkpoint = &store.Checkpoint{
		ArtifactRevisionsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan":   {NodeID: "z_selected", Version: 0},
			"result": {NodeID: "consumer", Version: 0},
		},
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a_unselected": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a_unselected"}, Publish: "plan"},
		"z_selected":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "z_selected"}, Publish: "plan"},
		"consumer":     &ir.ToolNode{BaseNode: ir.BaseNode{ID: "consumer"}, Publish: "result"},
	}}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "", false); err != nil {
		t.Fatalf("valid logical dependency rejected despite exact checkpoint producer: %v", err)
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

func TestBranchArtifactContractIncludesParentEdgeDependency(t *testing.T) {
	edge := &ir.Edge{From: "router", To: "writer", With: []*ir.DataMapping{{
		Key: "plan", Raw: "{{artifacts.plan}}",
		Refs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}}}
	consumer := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "report"}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{
		"planner": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "planner"}, Publish: "plan"},
		"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}},
		"writer":  consumer,
	}, Edges: []*ir.Edge{edge}}}
	parent := &runState{
		outputs:   map[string]map[string]any{"router": {"ok": true}},
		artifacts: map[string]map[string]any{"plan": {"value": "parent"}},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "planner", Version: 2},
		},
	}
	result := initBranchResult(parent, "branch-a", nil)
	local := newBranchRunState(parent, nil, result)
	contract := eng.artifactContractFor("writer", consumer, 0, local)
	if len(contract.Dependencies) != 1 || contract.Dependencies[0].NodeID != "planner" || contract.Dependencies[0].Version != 2 {
		t.Fatalf("branch contract omitted parent-visible dependency: %+v", contract.Dependencies)
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

func TestArtifactRevisionsForValidationHonorsKnownEmptyCheckpoint(t *testing.T) {
	run := &store.Run{
		ArtifactIndex: map[string]int{"invalidated": 7},
		Checkpoint: &store.Checkpoint{
			ArtifactRevisions:      map[string]store.ArtifactRevisionRef{},
			ArtifactRevisionsKnown: true,
		},
	}
	if got := artifactRevisionsForValidation(run); len(got) != 0 {
		t.Fatalf("known-empty checkpoint resurrected stale artifact index: %+v", got)
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

func TestRebuildArtifactRevisionsAliasesPersistedProducerWithCanonicalContractName(t *testing.T) {
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
	if got := revisions["new-name"]; got.NodeID != "writer" || got.Version != 1 || got.ContractLogicalRef != "old-name" {
		t.Fatalf("forced publish rename lost physical or canonical provenance: %+v", revisions)
	}
	if got := revisions["legacy-name"]; got.NodeID != "legacy" || got.Version != 0 {
		t.Fatalf("partial legacy revision was not inferred: %+v", revisions)
	}
}

func TestRebuildArtifactRevisionsRebindsSwappedPublishAliases(t *testing.T) {
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "second"},
		"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}, Publish: "first"},
	}}, nil, newStubExecutor())
	revisions := eng.rebuildArtifactRevisions(
		map[string]map[string]any{"a": {"producer": "a"}, "b": {"producer": "b"}},
		nil,
		map[string]store.ArtifactRevisionRef{
			"first":  {NodeID: "a", Version: 1},
			"second": {NodeID: "b", Version: 2},
		},
	)
	if got := revisions["first"]; got.NodeID != "b" || got.Version != 2 || got.ContractLogicalRef != "second" {
		t.Fatalf("first alias was not rebound to b's immutable revision: %+v", revisions)
	}
	if got := revisions["second"]; got.NodeID != "a" || got.Version != 1 || got.ContractLogicalRef != "first" {
		t.Fatalf("second alias was not rebound to a's immutable revision: %+v", revisions)
	}
}

func TestPrepareLegacyResumeSwapsAliasValuesWithTheirRevisions(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "second"},
		"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}, Publish: "first"},
	}}
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{
			"a": {"producer": "new-a"},
			"b": {"producer": "new-b"},
		},
		Artifacts: map[string]map[string]any{
			"first":  {"producer": "a"},
			"second": {"producer": "b"},
		},
		ArtifactOwners: map[string]string{"first": "a", "second": "b"},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"first":  {NodeID: "a", Version: 1},
			"second": {NodeID: "b", Version: 2},
		},
		ArtifactRevisionsKnown: true,
	}
	run := &store.Run{ID: "artifact-swapped-values", ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyLegacy}}
	state, err := New(wf, nil, newStubExecutor()).prepareResumeArtifacts(context.Background(), run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["first"]["producer"]; got != "b" {
		t.Fatalf("first alias kept the previous producer's value: %v", got)
	}
	if got := state.artifacts["second"]["producer"]; got != "a" {
		t.Fatalf("second alias kept the previous producer's value: %v", got)
	}
	if state.owners["first"] != "b" || state.owners["second"] != "a" {
		t.Fatalf("swapped alias owners = %+v", state.owners)
	}
}

func TestRebuildArtifactRevisionsRestoredAliasUsesLatestProducerRevision(t *testing.T) {
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"producer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "producer"}, Publish: "original"},
	}}, nil, newStubExecutor())
	revisions := eng.rebuildArtifactRevisions(
		map[string]map[string]any{"producer": {"value": "latest"}},
		nil,
		map[string]store.ArtifactRevisionRef{
			"original": {NodeID: "producer", Version: 0, ContractLogicalRef: "original"},
			"renamed":  {NodeID: "producer", Version: 1, ContractLogicalRef: "renamed"},
		},
	)
	if got := revisions["original"]; got.NodeID != "producer" || got.Version != 1 || got.ContractLogicalRef != "renamed" {
		t.Fatalf("restored alias did not follow latest retained producer revision: %+v", revisions)
	}
}

type failAfterArtifactLoadStore struct {
	store.RunStore
	maxLoads int
	loads    int
}

func (s *failAfterArtifactLoadStore) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	s.loads++
	if s.loads > s.maxLoads {
		return nil, errors.New("transient artifact backend outage")
	}
	return s.RunStore.LoadArtifact(ctx, runID, nodeID, version)
}

func artifactResumeWorkflow(publishName string) *ir.Workflow {
	return &ir.Workflow{
		Name:  "artifact_resume",
		Entry: "writer",
		Nodes: map[string]ir.Node{
			"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: publishName},
			"resume": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "resume"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "resume", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func TestResumeUsesPreclaimArtifactSnapshotWithoutSecondRead(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	const runID = "artifact-resume-one-read"
	run, err := base.CreateRun(ctx, runID, "artifact_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	if err := base.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "writer", Version: 0, Data: map[string]any{"value": "exact"},
		Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "writer", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{
		NodeID: "resume", Outputs: map[string]map[string]any{"writer": {"value": "checkpoint"}},
		ArtifactVersions: map[string]int{"writer": 1},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "writer", Version: 0, ContractLogicalRef: "plan"},
		},
	}
	if err := base.FailRunResumable(ctx, runID, cp, "retry", ""); err != nil {
		t.Fatal(err)
	}
	flaky := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 1}
	exec := newStubExecutor()
	exec.on("resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err := New(artifactResumeWorkflow("plan"), flaky, exec, WithWorkDir(t.TempDir()), WithSandboxOverride("none")).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("resume re-read artifacts after its claim: %v", err)
	}
	if flaky.loads != 1 {
		t.Fatalf("artifact loads = %d, want exactly one pre-claim read", flaky.loads)
	}
}

func TestResumeReusesInProcessArtifactContractPreflight(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	const runID = "artifact-resume-prevalidated"
	run, err := base.CreateRun(ctx, runID, "artifact_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	if err := base.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := base.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "writer", Version: 0, Data: map[string]any{"value": "exact"},
		Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "writer", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{
		NodeID: "resume", Outputs: map[string]map[string]any{"writer": {"value": "checkpoint"}},
		ArtifactVersions: map[string]int{"writer": 1},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "writer", Version: 0, ContractLogicalRef: "plan"},
		},
	}
	if err := base.FailRunResumable(ctx, runID, cp, "retry", ""); err != nil {
		t.Fatal(err)
	}
	flaky := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 1}
	fresh, err := flaky.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	wf := artifactResumeWorkflow("plan")
	preflight, err := ValidateResumeArtifacts(ctx, flaky, fresh, wf, "", false)
	if err != nil {
		t.Fatalf("artifact preflight: %v", err)
	}
	exec := newStubExecutor()
	exec.on("resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	eng := New(
		wf, flaky, exec,
		WithArtifactResumePreflight(preflight),
		WithWorkDir(t.TempDir()), WithSandboxOverride("none"),
	)
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("prevalidated resume re-read artifacts: %v", err)
	}
	if flaky.loads != 1 {
		t.Fatalf("artifact loads = %d, want only the preflight read", flaky.loads)
	}
	if eng.artifactResumePreflight != nil {
		t.Fatal("engine retained the one-shot artifact preflight after reconstruction")
	}
}

func TestResumeInvalidatesArtifactPreflightAfterCheckpointChange(t *testing.T) {
	ctx := context.Background()
	base := tmpStore(t)
	const runID = "artifact-resume-stale-preflight"
	run, err := base.CreateRun(ctx, runID, "artifact_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
	}
	if err := base.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for version := 0; version < 2; version++ {
		if err := base.WriteArtifact(ctx, &store.Artifact{
			RunID: runID, NodeID: "writer", Version: version, Data: map[string]any{"version": version},
			Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "writer", Version: version},
		}); err != nil {
			t.Fatal(err)
		}
	}
	cp := &store.Checkpoint{
		NodeID: "resume", Outputs: map[string]map[string]any{"writer": {"version": 0}},
		ArtifactVersions: map[string]int{"writer": 1},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "writer", Version: 0, ContractLogicalRef: "plan"},
		},
	}
	if err := base.FailRunResumable(ctx, runID, cp, "retry", ""); err != nil {
		t.Fatal(err)
	}
	flaky := &failAfterArtifactLoadStore{RunStore: base, maxLoads: 2}
	fresh, err := flaky.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	wf := artifactResumeWorkflow("plan")
	preflight, err := ValidateResumeArtifacts(ctx, flaky, fresh, wf, "", false)
	if err != nil {
		t.Fatalf("artifact preflight: %v", err)
	}
	updated, err := base.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	updated.Checkpoint.Outputs["writer"] = map[string]any{"version": 1}
	updated.Checkpoint.ArtifactVersions["writer"] = 2
	updated.Checkpoint.ArtifactRevisions["plan"] = store.ArtifactRevisionRef{
		NodeID: "writer", Version: 1, ContractLogicalRef: "plan",
	}
	if err := base.SaveRun(ctx, updated); err != nil {
		t.Fatal(err)
	}
	exec := newStubExecutor()
	exec.on("resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	eng := New(
		wf, flaky, exec,
		WithArtifactResumePreflight(preflight),
		WithWorkDir(t.TempDir()), WithSandboxOverride("none"),
	)
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("resume with changed checkpoint: %v", err)
	}
	if flaky.loads != 2 {
		t.Fatalf("artifact loads = %d, want old preflight plus refreshed checkpoint", flaky.loads)
	}
}

func TestArtifactContractPreflightDoesNotDuplicateReportEvent(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-report-preflight", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyReport}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: run.ID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{LogicalRef: "old", ProducerNode: "writer", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	run.ArtifactIndex = map[string]int{"writer": 0}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "new"},
	}}
	if err := ValidateArtifactContractsPreflight(ctx, s, run, wf, "", false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateArtifactContracts(ctx, s, run, wf, "", false); err != nil {
		t.Fatal(err)
	}
	events, err := s.LoadEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == store.EventArtifactContractViolation {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("artifact contract violation events = %d, want one", count)
	}
}

func TestResumeLegacyCheckpointOnlyForkWithoutArtifactBlobs(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "artifact-legacy-fork"
	run, err := s.CreateRun(ctx, runID, "artifact_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.ParentRunID = "old-parent"
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{
		NodeID: "resume", Outputs: map[string]map[string]any{"writer": {"value": "checkpoint-only"}},
		ArtifactVersions: map[string]int{"writer": 1},
	}
	if err := s.FailRunResumable(ctx, runID, cp, "retry", ""); err != nil {
		t.Fatal(err)
	}
	exec := newStubExecutor()
	exec.on("resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err := New(artifactResumeWorkflow("plan"), s, exec, WithWorkDir(t.TempDir()), WithSandboxOverride("none")).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("legacy checkpoint-only fork stopped being resumable: %v", err)
	}
}

func TestForcedPublishRenameRecordsCanonicalDependency(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "artifact-forced-rename"
	run, err := s.CreateRun(ctx, runID, "artifact_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "old-revision"
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "writer", Version: 0, Data: map[string]any{"value": "old"},
		Contract: &store.ArtifactContract{LogicalRef: "old-name", ProducerNode: "writer", ProducerRevision: "old-revision", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{
		NodeID:           "resume",
		Outputs:          map[string]map[string]any{"writer": {"value": "latest-output"}},
		Artifacts:        map[string]map[string]any{"old-name": {"value": "old"}},
		ArtifactsKnown:   true,
		ArtifactVersions: map[string]int{"writer": 1},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"old-name": {NodeID: "writer", Version: 0, ContractLogicalRef: "old-name"},
		},
	}
	if err := s.FailRunResumable(ctx, runID, cp, "retry", ""); err != nil {
		t.Fatal(err)
	}
	wf := artifactResumeWorkflow("new-name")
	resumeNode := wf.Nodes["resume"].(*ir.ToolNode)
	resumeNode.Publish = "result"
	resumeNode.PostcondRefs = []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"new-name"}}}
	exec := newStubExecutor()
	exec.on("resume", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	eng := New(wf, s, exec, WithWorkflowHash("new-revision"), WithForceResume(true), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatal(err)
	}
	result, err := s.LoadArtifact(ctx, runID, "resume", 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Contract == nil || len(result.Contract.Dependencies) != 1 {
		t.Fatalf("result contract dependencies = %+v", result.Contract)
	}
	dep := result.Contract.Dependencies[0]
	if dep.LogicalRef != "old-name" || dep.NodeID != "writer" || dep.Version != 0 {
		t.Fatalf("renamed alias dependency lost canonical provenance: %+v", dep)
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
		"vision": &ir.AgentNode{
			BaseNode: ir.BaseNode{ID: "vision"},
			LLMFields: ir.LLMFields{Images: []string{
				"plain.png", "{{artifacts.seed.path}}",
			}},
		},
	}}
	if got := ir.NodeArtifactRefs(wf, "tool"); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("tool artifact refs = %v", got)
	}
	if got := ir.NodeArtifactRefs(wf, "review"); len(got) != 1 || got[0] != "environment" {
		t.Fatalf("review artifact refs = %v", got)
	}
	if got := ir.NodeArtifactRefs(wf, "vision"); len(got) != 1 || got[0] != "seed" {
		t.Fatalf("image artifact refs = %v", got)
	}
}

func TestPrepareResumeArtifactsKeepsLegacyParallelCheckpointValue(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "artifact-legacy-parallel-order"
	run, err := s.CreateRun(ctx, runID, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteArtifact(ctx, &store.Artifact{
		RunID: runID, NodeID: "worker", Version: 1,
		Data: map[string]any{"item": "later-version"},
	}); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{
		Outputs:          map[string]map[string]any{"worker": {"item": "checkpoint-branch"}},
		ArtifactVersions: map[string]int{"worker": 2},
	}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"worker": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "worker"}, Publish: "result"},
	}}, s, newStubExecutor())

	state, err := eng.prepareResumeArtifacts(ctx, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["result"]["item"]; got != "checkpoint-branch" {
		t.Fatalf("legacy checkpoint value was replaced by inferred revision: %v", got)
	}
	if len(state.revisions) != 0 {
		t.Fatalf("inferred legacy revision was promoted to exact provenance: %+v", state.revisions)
	}
	rs := eng.newRunState(runID, nil)
	eng.restoreCheckpointState(rs, cp, state)
	next := buildCheckpoint(rs, "retry")
	second, err := eng.prepareResumeArtifacts(ctx, run, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.artifacts["result"]["item"]; got != "checkpoint-branch" {
		t.Fatalf("second resume replaced legacy checkpoint value: %v", got)
	}
	if owner := next.ArtifactOwners["result"]; owner != "worker" {
		t.Fatalf("legacy fallback owner was not persisted: %q", owner)
	}
}

func TestLegacyBranchArtifactDoesNotBorrowTrunkProvenance(t *testing.T) {
	parent := &runState{
		artifacts: map[string]map[string]any{"plan": {"value": "trunk"}},
		artifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "planner", Version: 0},
		},
	}
	cp := &store.BranchCheckpoint{
		Artifacts: map[string]map[string]any{"plan": {"value": "branch"}},
		Outputs:   map[string]map[string]any{"worker": {"value": "branch"}},
	}
	result := initBranchResult(parent, "branch", cp)
	local := newBranchRunState(parent, cp, result)
	consumer := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "consumer"}, Publish: "report",
		CommandRefs: []*ir.Ref{{Kind: ir.RefArtifacts, Path: []string{"plan"}}},
	}
	eng := &Engine{workflow: &ir.Workflow{Nodes: map[string]ir.Node{"consumer": consumer}}}
	if got := local.artifacts["plan"]["value"]; got != "branch" {
		t.Fatalf("legacy branch value = %v", got)
	}
	contract := eng.artifactContractFor("consumer", consumer, 0, local)
	if len(contract.Dependencies) != 0 {
		t.Fatalf("legacy branch value borrowed trunk provenance: %+v", contract.Dependencies)
	}
}

func TestLegacyCheckpointOwnerRebindsAfterPublishRename(t *testing.T) {
	ctx := context.Background()
	run := &store.Run{ID: "artifact-owner-rename"}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "old"},
	}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	legacy := &store.Checkpoint{
		NodeID:           "retry",
		Outputs:          map[string]map[string]any{"writer": {"value": "retained"}},
		ArtifactVersions: map[string]int{"writer": 1},
	}
	state, err := eng.prepareResumeArtifacts(ctx, run, legacy)
	if err != nil {
		t.Fatal(err)
	}
	rs := eng.newRunState(run.ID, nil)
	eng.restoreCheckpointState(rs, legacy, state)
	cp := buildCheckpoint(rs, "retry")
	wf.Nodes["writer"].(*ir.ToolNode).Publish = "renamed"

	next, err := eng.prepareResumeArtifacts(ctx, run, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := next.artifacts["renamed"]["value"]; got != "retained" {
		t.Fatalf("renamed artifact = %v; artifacts=%+v owners=%+v", got, next.artifacts, next.owners)
	}
	if owner := next.owners["renamed"]; owner != "writer" {
		t.Fatalf("renamed artifact owner = %q", owner)
	}
}

func TestLegacyCheckpointOwnerSurvivesRepeatedPublishRenames(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}, Publish: "original"},
	}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	run := &store.Run{ID: "artifact-repeated-owner-rename"}
	cp := &store.Checkpoint{
		NodeID:                 "retry",
		Outputs:                map[string]map[string]any{"writer": {"value": "retained"}},
		ArtifactRevisionsKnown: true,
	}
	for _, name := range []string{"original", "renamed", "final"} {
		wf.Nodes["writer"].(*ir.ToolNode).Publish = name
		state, err := eng.prepareResumeArtifacts(context.Background(), run, cp)
		if err != nil {
			t.Fatal(err)
		}
		if got := state.artifacts[name]["value"]; got != "retained" {
			t.Fatalf("artifact after rename to %q = %v; artifacts=%+v owners=%+v", name, got, state.artifacts, state.owners)
		}
		rs := eng.newRunState(run.ID, nil)
		eng.restoreCheckpointState(rs, cp, state)
		cp = buildCheckpoint(rs, "retry")
	}
}

func TestPrepareResumeArtifactsSwapsMixedProvenanceAliases(t *testing.T) {
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{
			"a": {"value": "a"},
			"b": {"value": "b"},
		},
		Artifacts:      map[string]map[string]any{"first": {"value": "a"}, "second": {"value": "b"}},
		ArtifactOwners: map[string]string{"first": "a", "second": "b"},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"second": {NodeID: "b", Version: 0},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "second"},
		"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}, Publish: "first"},
	}}
	state, err := New(wf, tmpStore(t), newStubExecutor()).prepareResumeArtifacts(
		context.Background(), &store.Run{ID: "artifact-mixed-alias-swap"}, cp,
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.artifacts["first"]["value"] != "b" || state.artifacts["second"]["value"] != "a" {
		t.Fatalf("mixed-provenance alias swap = artifacts %+v, owners %+v, revisions %+v", state.artifacts, state.owners, state.revisions)
	}
	if _, stale := state.revisions["second"]; stale {
		t.Fatalf("owner-only alias retained the old producer's exact revision: %+v", state.revisions)
	}
}

func TestPrepareResumeArtifactsRestoresNewPublishFromRetainedOutput(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"writer": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "writer"}},
	}}
	eng := New(wf, tmpStore(t), newStubExecutor(), WithForceResume(true))
	rs := eng.newRunState("artifact-publish-added", nil)
	rs.outputs["writer"] = map[string]any{"value": "retained"}
	cp := buildCheckpoint(rs, "later")
	wf.Nodes["writer"].(*ir.ToolNode).Publish = "plan"

	state, err := eng.prepareResumeArtifacts(context.Background(), &store.Run{ID: rs.runID}, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["plan"]["value"]; got != "retained" {
		t.Fatalf("new publish alias value = %v; artifacts=%+v outputs=%+v", got, state.artifacts, cp.Outputs)
	}
	if owner := state.owners["plan"]; owner != "writer" {
		t.Fatalf("new publish alias owner = %q", owner)
	}
	if _, exact := state.revisions["plan"]; exact {
		t.Fatalf("new publish alias acquired false physical provenance: %+v", state.revisions)
	}
}

func TestPrepareResumeArtifactsTransfersPublishFromRetiredOwner(t *testing.T) {
	cp := &store.Checkpoint{
		Outputs: map[string]map[string]any{
			"a": {"value": "new-owner"},
			"b": {"value": "retired-owner"},
		},
		Artifacts:      map[string]map[string]any{"plan": {"value": "retired-owner"}},
		ArtifactOwners: map[string]string{"plan": "b"},
		ArtifactsKnown: true,
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"plan": {NodeID: "b", Version: 0},
		},
		ArtifactRevisionsKnown: true,
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "a"}, Publish: "plan"},
		"b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "b"}},
	}}
	state, err := New(wf, tmpStore(t), newStubExecutor()).prepareResumeArtifacts(
		context.Background(), &store.Run{ID: "artifact-publish-transfer"}, cp,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["plan"]["value"]; got != "new-owner" {
		t.Fatalf("transferred publish value = %v; artifacts=%+v owners=%+v", got, state.artifacts, state.owners)
	}
	if owner := state.owners["plan"]; owner != "a" {
		t.Fatalf("transferred publish owner = %q", owner)
	}
	if _, exact := state.revisions["plan"]; exact {
		t.Fatalf("transferred publish acquired stale physical provenance: %+v", state.revisions)
	}
}

func TestBuildCheckpointDoesNotDuplicateOwnedOutputBodies(t *testing.T) {
	eng := New(&ir.Workflow{}, nil, newStubExecutor())
	rs := eng.newRunState("artifact-large-checkpoint", nil)
	body := strings.Repeat("x", 9<<20)
	rs.outputs["producer"] = map[string]any{"data": body, "exit_code": int64(0)}
	rs.artifacts["published"] = map[string]any{"data": body, "exit_code": float64(0)}
	rs.artifactOwners["published"] = "producer"
	rs.artifactRevisions["published"] = store.ArtifactRevisionRef{NodeID: "producer", Version: 0}

	cp := buildCheckpoint(rs, "next")
	if _, duplicated := cp.Artifacts["published"]; duplicated {
		t.Fatal("checkpoint duplicated a logical artifact already present as its owner's output")
	}
	if owner := cp.ArtifactOwners["published"]; owner != "producer" {
		t.Fatalf("compacted checkpoint lost logical owner: %q", owner)
	}
	encoded, err := bson.Marshal(bson.M{"checkpoint": cp})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 16<<20 {
		t.Fatalf("compacted checkpoint is %d BSON bytes, exceeds Mongo's 16 MiB document limit", len(encoded))
	}
	state, err := eng.prepareResumeArtifacts(context.Background(), &store.Run{ID: rs.runID}, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["published"]["data"]; got != body {
		t.Fatal("resume did not hydrate the compacted logical artifact")
	}
}

func TestBuildCheckpointReferencesLargeHistoricalArtifactBody(t *testing.T) {
	ctx := context.Background()
	st := tmpStore(t)
	eng := New(&ir.Workflow{}, st, newStubExecutor())
	rs := eng.newRunState("artifact-large-history", nil)
	oldBody := strings.Repeat("o", 9<<20)
	currentBody := strings.Repeat("n", 9<<20)
	rs.outputs["producer"] = map[string]any{"data": currentBody}
	rs.artifacts["old-name"] = map[string]any{"data": oldBody}
	rs.artifactOwners["old-name"] = "producer"
	rs.artifactRevisions["old-name"] = store.ArtifactRevisionRef{NodeID: "producer", Version: 0}
	rs.artifactVersions["producer"] = 2
	if err := st.WriteArtifact(ctx, &store.Artifact{
		RunID: rs.runID, NodeID: "producer", Version: 0,
		Data: map[string]any{"data": oldBody},
	}); err != nil {
		t.Fatal(err)
	}

	cp := buildCheckpoint(rs, "next")
	if _, embedded := cp.Artifacts["old-name"]; embedded {
		t.Fatal("checkpoint embedded a large historical body already held by an immutable revision")
	}
	if revision := cp.ArtifactRevisions["old-name"]; !revision.ValueFromRevision {
		t.Fatalf("historical body has no durable value reference: %+v", revision)
	}
	encoded, err := bson.Marshal(bson.M{"checkpoint": cp})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 16<<20 {
		t.Fatalf("checkpoint with historical body reference is %d BSON bytes, exceeds Mongo's 16 MiB document limit", len(encoded))
	}
	state, err := eng.prepareResumeArtifacts(ctx, &store.Run{
		ID: rs.runID, ExecutionContext: &store.ExecutionContext{Policy: store.ContextPolicyLegacy},
	}, cp)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.artifacts["old-name"]["data"]; got != oldBody {
		t.Fatal("legacy resume did not restore the referenced historical body")
	}
}

func TestReferencedHistoricalArtifactUnavailableDoesNotBlockCompatibilityPolicies(t *testing.T) {
	for _, policy := range []store.ContextPolicy{store.ContextPolicyLegacy, store.ContextPolicyReport} {
		t.Run(string(policy), func(t *testing.T) {
			base := tmpStore(t)
			cp := &store.Checkpoint{
				Outputs:        map[string]map[string]any{"producer": {"value": "current"}},
				Artifacts:      map[string]map[string]any{},
				ArtifactOwners: map[string]string{"old-name": "producer"},
				ArtifactsKnown: true,
				ArtifactRevisions: map[string]store.ArtifactRevisionRef{
					"old-name": {NodeID: "producer", Version: 0, ValueFromRevision: true},
				},
				ArtifactRevisionsKnown: true,
			}
			state, err := New(&ir.Workflow{}, artifactReadErrorStore{base}, newStubExecutor()).prepareResumeArtifacts(
				context.Background(),
				&store.Run{ID: "unavailable-history", ExecutionContext: &store.ExecutionContext{Policy: policy}},
				cp,
			)
			if err != nil {
				t.Fatalf("compatibility policy became an availability gate: %v", err)
			}
			if _, retained := state.artifacts["old-name"]; retained {
				t.Fatalf("unavailable historical alias retained a fabricated value: %+v", state.artifacts)
			}
		})
	}
}

func TestBranchCheckpointPreservesExpandedArtifactForV1Readers(t *testing.T) {
	result := &branchResult{
		branchID:          "branch",
		outputs:           map[string]map[string]any{"worker": {"value": "published"}},
		artifacts:         map[string]map[string]any{"plan": {"value": "published"}},
		artifactOwners:    map[string]string{"plan": "worker"},
		artifactRevisions: map[string]store.ArtifactRevisionRef{"plan": {NodeID: "worker", Version: 0}},
	}
	cp := branchCheckpointFromState(&runState{}, result, "next", false)
	if got := cp.Artifacts["plan"]["value"]; got != "published" {
		t.Fatalf("branch checkpoint omitted the expanded V1-compatible artifact: %+v", cp.Artifacts)
	}
	if owner := cp.ArtifactOwners["plan"]; owner != "worker" {
		t.Fatalf("branch checkpoint lost logical owner: %q", owner)
	}
	restored := initBranchResult(&runState{}, "branch", cp)
	if got := restored.artifacts["plan"]["value"]; got != "published" {
		t.Fatalf("branch resume did not hydrate compacted artifact: %+v", restored.artifacts)
	}
	expanded := *cp
	expanded.Artifacts = map[string]map[string]any{"plan": {"value": "published"}}
	prepared := checkpointWithPreparedParallel(
		&store.Checkpoint{},
		&resumeArtifactState{parallel: &store.ParallelCheckpoint{Branches: map[string]*store.BranchCheckpoint{"branch": &expanded}}},
	)
	if got := prepared.Parallel.Branches["branch"].Artifacts["plan"]["value"]; got != "published" {
		t.Fatalf("prepared parallel checkpoint dropped the V1-compatible branch body: %+v", prepared.Parallel.Branches["branch"].Artifacts)
	}
}

func TestLegacyCompletedBranchRecoversArtifactOwner(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "artifact-old-branch-owner", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	eng := New(&ir.Workflow{Nodes: map[string]ir.Node{
		"worker": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "worker"}, Publish: "plan"},
	}}, s, newStubExecutor())
	rs := eng.newRunState(run.ID, nil)
	rs.ctx = ctx
	branch := &store.BranchCheckpoint{
		Outputs:   map[string]map[string]any{"worker": {"value": "stale"}},
		Artifacts: map[string]map[string]any{"plan": {"value": "stale"}},
		Completed: true,
	}
	result := initBranchResult(rs, "branch", branch)
	if _, err := eng.processConvergenceTerminal(rs, []*branchResult{result}, ir.AwaitWaitAll); err != nil {
		t.Fatal(err)
	}
	cp := buildCheckpoint(rs, "later")
	if owner := cp.ArtifactOwners["plan"]; owner != "worker" {
		t.Fatalf("legacy branch artifact owner = %q; artifacts=%+v outputs=%+v", owner, cp.Artifacts, cp.Outputs)
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
