package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Copy the persisted document byte for byte: rewriting its identity or version
// would hide the migration/misrouting that admission has to detect.
func copyAdmissionRun(t *testing.T, from, to store.RunStore, id string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(from.Root(), "runs", id, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := to.CreateRun(t.Context(), id, "admission", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to.Root(), "runs", id, "run.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionUsesEffectiveStoreOnRunAndResume(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			move     bool
			policy   store.ContextPolicy
			required bool
			code     string
			denied   bool
		}{
			{"same", false, store.ContextPolicyEnforce, true, "context_match", false},
			{"same_optional", false, store.ContextPolicyEnforce, false, "context_match", false},
			{"moved_enforce", true, store.ContextPolicyEnforce, true, "context_mismatch", true},
			{"moved_report", true, store.ContextPolicyReport, true, "context_report_only", false},
			{"moved_legacy", true, store.ContextPolicyLegacy, true, "legacy_context_unvalidated", false},
			{"moved_optional", true, store.ContextPolicyEnforce, false, "context_store_optional", false},
		} {
			name := "run/" + tc.name
			if resume {
				name = "resume/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				a := tmpStore(t)
				ref, err := store.ResolveStoreContextRef(ctx, a)
				if err != nil {
					t.Fatal(err)
				}
				ref.Required = tc.required
				wire := &store.ExecutionContext{Version: 1, Policy: tc.policy, RunStore: ref}
				run, err := a.CreateRun(ctx, "migrated", "admission", nil)
				if err != nil {
					t.Fatal(err)
				}
				run.ExecutionContext = wire.Clone()
				if resume {
					run.Status = store.RunStatusFailedResumable
				}
				if err := a.SaveRun(ctx, run); err != nil {
					t.Fatal(err)
				}
				effective := a
				if tc.move {
					effective = tmpStore(t)
					copyAdmissionRun(t, a, effective, run.ID)
					b, err := store.ResolveStoreContextRef(ctx, effective)
					if err != nil || b.ID == ref.ID {
						t.Fatalf("fixture stores are not distinct: %+v %v", b, err)
					}
				}
				wf := admissionWorkflow()
				wf.Worktree = "none"
				wf.Entry = "probe"
				wf.Nodes["probe"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "probe"}, Command: "unused"}
				wf.Edges = []*ir.Edge{{From: "probe", To: "done"}}
				calls := 0
				executor := newStubExecutor()
				executor.on("probe", func(map[string]any) (map[string]any, error) {
					calls++
					return map[string]any{}, nil
				})
				eng := New(wf, effective, executor, WithExecutionContext(wire), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
				if resume {
					err = eng.Resume(ctx, run.ID, nil)
				} else {
					err = eng.Run(ctx, run.ID, nil)
				}
				got, loadErr := effective.LoadRun(ctx, run.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if (err != nil) != tc.denied || got.Admission == nil || got.Admission.Code != tc.code || got.Admission.Policy != tc.policy {
					t.Fatalf("admission=%+v error=%v, want code %s denied=%v", got.Admission, err, tc.code, tc.denied)
				}
				if tc.move && !strings.Contains(got.Admission.Reason, "differs from the effective run store") {
					t.Fatalf("store divergence lost: %+v", got.Admission)
				}
				if tc.denied {
					var rtErr *RuntimeError
					if !errors.As(err, &rtErr) || rtErr.Code != store.FailureLaunchFailed || calls != 0 || got.Admission.Decision != "denied" {
						t.Fatalf("denial did not precede executor: calls=%d err=%v admission=%+v", calls, err, got.Admission)
					}
					wantStatus := store.RunStatusFailed
					if resume {
						wantStatus = store.RunStatusFailedResumable
					}
					if got.Status != wantStatus {
						t.Fatalf("status=%s want %s", got.Status, wantStatus)
					}
				} else if calls != 1 || got.Status != store.RunStatusFinished || got.Admission.Decision != "allowed" {
					t.Fatalf("allowed run did not execute: calls=%d status=%s admission=%+v", calls, got.Status, got.Admission)
				}
			})
		}
	}
}

type admissionLegacyStore struct{ store.RunStore }
type admissionIdentityStore struct {
	store.RunStore
	ref store.ContextRef
	err error
}

func (s admissionIdentityStore) ContextIdentity(context.Context) (store.ContextRef, error) {
	return s.ref, s.err
}

type admissionStoreDecorator struct{ store.RunStore }

func (s admissionStoreDecorator) Unwrap() store.RunStore { return s.RunStore }

func TestAdmissionStoreComparabilityIsIndependentOfDeclaration(t *testing.T) {
	for _, tc := range []string{"decorated", "no_capability", "legacy", "identity_error", "forged_id", "kind", "namespace", "revision"} {
		for _, required := range []bool{false, true} {
			name := tc + "/optional"
			if required {
				name = tc + "/required"
			}
			t.Run(name, func(t *testing.T) {
				base := tmpStore(t)
				ref, err := store.ResolveStoreContextRef(t.Context(), base)
				if err != nil {
					t.Fatal(err)
				}
				effective := base
				switch tc {
				case "decorated":
					effective = admissionStoreDecorator{base}
				case "no_capability":
					effective = admissionLegacyStore{base}
				case "legacy":
					ref.ID = store.StableContextID(ref.Kind, ref.Namespace)
				case "identity_error":
					effective = admissionIdentityStore{RunStore: base, err: errors.New("mongodb://secret:password@private.invalid")}
				case "forged_id":
					ref.ID = "store-v1:forged"
				case "kind":
					ref.Kind = "other"
				case "namespace":
					ref.Namespace += "/other"
				case "revision":
					current := ref
					current.Revision = "current"
					effective = admissionIdentityStore{RunStore: base, ref: current}
					ref.Revision = "previous"
				}
				ref.Required = required
				run, err := base.CreateRun(t.Context(), "compare", "admission", nil)
				if err != nil {
					t.Fatal(err)
				}
				run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: ref}
				err = New(admissionWorkflow(), effective, newStubExecutor(), WithExecutionContext(run.ExecutionContext)).admitRun(t.Context(), run.ID, run)
				got, loadErr := base.LoadRun(t.Context(), run.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				wantCode := "context_store_optional"
				denied := required && tc != "decorated"
				if tc == "decorated" {
					wantCode = "context_match"
				} else if required {
					wantCode = "context_mismatch"
				}
				if (err != nil) != denied || got.Admission == nil || got.Admission.Code != wantCode {
					t.Fatalf("admission=%+v error=%v, want code %s denied=%v", got.Admission, err, wantCode, denied)
				}
				for _, secret := range []string{"secret", "password", "private.invalid", "mongodb://"} {
					if strings.Contains(got.Admission.Reason, secret) {
						t.Fatalf("private capability error persisted: %+v", got.Admission)
					}
				}
			})
		}
	}
}

func TestAdmissionOptionalStoreDoesNotWaiveOtherChecks(t *testing.T) {
	for _, tc := range []string{"workflow", "parent", "missing_parent", "version", "trust", "wire_workflow", "wire_parent"} {
		t.Run(tc, func(t *testing.T) {
			s := tmpStore(t)
			other := tmpStore(t)
			ref, err := store.ResolveStoreContextRef(t.Context(), other)
			if err != nil {
				t.Fatal(err)
			}
			ref.Required = false
			run, err := s.CreateRun(t.Context(), "other-check", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: ref}
			wire := run.ExecutionContext.Clone()
			switch tc {
			case "workflow":
				run.WorkflowHash = "actual"
				run.ExecutionContext.Workflow.WorkflowRevision = "declared"
			case "parent":
				run.ExecutionContext.Lineage.ParentRunID = "other"
			case "missing_parent":
				run.ParentRunID = "parent"
			case "version":
				run.ExecutionContext.Version = 2
			case "trust":
				run.Trust = store.RunTrustFork
			case "wire_workflow":
				wire.Workflow.WorkflowRevision = "other"
			case "wire_parent":
				wire.Lineage.ParentRunID = "other"
			}
			err = New(admissionWorkflow(), s, newStubExecutor(), WithExecutionContext(wire), WithSandboxOverride("none")).admitRun(t.Context(), run.ID, run)
			got, loadErr := s.LoadRun(t.Context(), run.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantCode := "context_mismatch"
			if tc == "trust" {
				wantCode = "untrusted_without_sandbox"
			}
			if err == nil || got.Admission == nil || got.Admission.Decision != "denied" || got.Admission.Code != wantCode {
				t.Fatalf("optional store waived another check: %+v error=%v", got.Admission, err)
			}
			if tc != "trust" && !strings.Contains(got.Admission.Reason, "run_store.required=false") {
				t.Fatalf("other violation erased the store observation: %+v", got.Admission)
			}
		})
	}
}

func TestAdmissionRetainsPersistedPolicyWhenWireRequestsEnforce(t *testing.T) {
	for _, tc := range []struct {
		policy store.ContextPolicy
		code   string
	}{
		{"", "legacy_context_unvalidated"},
		{store.ContextPolicyLegacy, "legacy_context_unvalidated"},
		{store.ContextPolicyReport, "context_report_only"},
		{store.ContextPolicyEnforce, "context_match"},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			s := tmpStore(t)
			ref, err := store.ResolveStoreContextRef(t.Context(), s)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.CreateRun(t.Context(), "policy", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: tc.policy, RunStore: ref}
			wire := run.ExecutionContext.Clone()
			wire.Policy = store.ContextPolicyEnforce
			err = New(admissionWorkflow(), s, newStubExecutor(), WithExecutionContext(wire)).admitRun(t.Context(), run.ID, run)
			got, loadErr := s.LoadRun(t.Context(), run.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantPolicy := tc.policy
			if wantPolicy == "" {
				wantPolicy = store.ContextPolicyLegacy
			}
			if err != nil || got.Admission == nil || got.Admission.Code != tc.code || got.Admission.Policy != wantPolicy || got.Admission.Decision != "allowed" {
				t.Fatalf("policy ladder changed: error=%v admission=%+v", err, got.Admission)
			}
		})
	}
}
