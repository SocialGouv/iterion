package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func admissionWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:    "admission",
		Entry:   "done",
		Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}
}

func TestAdmissionEnforceDeniesBeforeExecution(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "admit-deny", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "actual"
	run.ExecutionContext = &store.ExecutionContext{
		Version:  1,
		Policy:   store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
		Workflow: store.WorkflowContext{WorkflowRevision: "declared"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	err = New(admissionWorkflow(), s, newStubExecutor()).Run(ctx, run.ID, nil)
	if err == nil {
		t.Fatal("admission accepted a mismatched context")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != store.FailureLaunchFailed {
		t.Fatalf("error = %v, want launch failure runtime error", err)
	}
	got, loadErr := s.LoadRun(ctx, run.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got.Admission == nil || got.Admission.Decision != "denied" || got.Admission.Code != "context_mismatch" {
		t.Fatalf("admission decision = %+v, want durable denial", got.Admission)
	}
	if got.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}

func TestAdmissionLegacyRunIsRecordedAndAllowed(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := New(admissionWorkflow(), s, newStubExecutor())
	if err := eng.Run(ctx, "admit-legacy", nil); err != nil {
		t.Fatal(err)
	}
	run, err := s.LoadRun(ctx, "admit-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if run.Admission == nil || run.Admission.Decision != "allowed" || run.Admission.Code != "legacy_context" {
		t.Fatalf("admission decision = %+v, want legacy allow", run.Admission)
	}
}

func TestAdmissionResumeDenialDoesNotDestroyResumableRun(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	run, err := s.CreateRun(ctx, "admit-resume", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, run.ID, store.RunStatusFailedResumable, "needs repair"); err != nil {
		t.Fatal(err)
	}
	run, err = s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkflowHash = "actual"
	run.ExecutionContext = &store.ExecutionContext{
		Version: 1, Policy: store.ContextPolicyEnforce,
		RunStore: store.ContextRef{ID: "run", Kind: "filesystem"},
		Workflow: store.WorkflowContext{WorkflowRevision: "declared"},
	}
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	eng := New(admissionWorkflow(), s, newStubExecutor())
	if err := eng.Resume(ctx, run.ID, nil); err == nil {
		t.Fatal("resume accepted a mismatched context")
	}
	got, err := s.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunStatusFailedResumable {
		t.Fatalf("resume denial changed status to %s", got.Status)
	}
	if got.Admission == nil || got.Admission.Decision != "denied" {
		t.Fatalf("resume admission = %+v, want denied", got.Admission)
	}
}
