package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"testing"
)

type admissionSnapshotStore struct {
	store.RunStore
	change              func(*store.Run)
	changed             bool
	conflicts, attempts int
}

func (s *admissionSnapshotStore) Unwrap() store.RunStore { return s.RunStore }
func (s *admissionSnapshotStore) SaveRun(ctx context.Context, r *store.Run) error {
	if r.Admission != nil {
		s.attempts++
		if !s.changed && s.change != nil {
			s.changed = true
			fresh, err := s.LoadRun(ctx, r.ID)
			if err != nil {
				return err
			}
			s.change(fresh)
			if err := s.RunStore.SaveRun(ctx, fresh); err != nil {
				return err
			}
		}
	}
	err := s.RunStore.SaveRun(ctx, r)
	if errors.Is(err, store.ErrRunConflict) {
		s.conflicts++
	}
	return err
}
func admissionSnapshotEngine(t *testing.T, s store.RunStore, c *store.ExecutionContext, calls *int) *Engine {
	t.Helper()
	wf := admissionWorkflow()
	wf.Worktree = "none"
	wf.Entry = "probe"
	wf.Nodes["probe"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "probe"}, Command: "unused"}
	wf.Edges = []*ir.Edge{{From: "probe", To: "done"}}
	ex := newStubExecutor()
	ex.on("probe", func(map[string]any) (map[string]any, error) { *calls++; return map[string]any{}, nil })
	return New(wf, s, ex, WithExecutionContext(c), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
}

func TestAdmissionRecomputesEverySnapshot(t *testing.T) {
	for _, name := range []string{"enforce to report", "report to enforce", "nil to enforce", "enforce to nil", "trusted to fork", "fork to trusted", "version bad", "version repaired", "metadata"} {
		t.Run(name, func(t *testing.T) {
			base := tmpStore(t)
			actual, err := store.ResolveStoreContextRef(t.Context(), base)
			if err != nil {
				t.Fatal(err)
			}
			wrong := store.NewStoreContextRef("filesystem", "other-root")
			c := &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: actual}
			row, err := base.CreateRun(t.Context(), "cas-direct", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			wantCode := "context_match"
			wantDeny := false
			switch name {
			case "enforce to report":
				c.RunStore = wrong
				wantCode = "context_report_only"
			case "report to enforce":
				c.Policy = store.ContextPolicyReport
				c.RunStore = wrong
				wantCode = "context_mismatch"
				wantDeny = true
			case "nil to enforce":
				c = nil
				wantCode = "context_mismatch"
				wantDeny = true
			case "enforce to nil":
				wantCode = "legacy_context"
			case "trusted to fork":
				wantCode = "untrusted_without_sandbox"
				wantDeny = true
			case "fork to trusted":
				row.Trust = store.RunTrustFork
				wantCode = "untrusted_without_sandbox"
				wantDeny = true // SaveRun intentionally preserves write-once Trust.
			case "version bad":
				wantCode = "context_mismatch"
				wantDeny = true
			case "version repaired":
				c.Version = 2
			}
			row.ExecutionContext = c
			if err := base.SaveRun(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			wrapped := &admissionSnapshotStore{RunStore: base, change: func(fresh *store.Run) {
				switch name {
				case "enforce to report":
					fresh.ExecutionContext.Policy = store.ContextPolicyReport
				case "report to enforce":
					fresh.ExecutionContext.Policy = store.ContextPolicyEnforce
				case "nil to enforce":
					fresh.ExecutionContext = &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: wrong}
				case "enforce to nil":
					fresh.ExecutionContext = nil
				case "trusted to fork":
					fresh.Trust = store.RunTrustFork
				case "fork to trusted":
					fresh.Trust = ""
				case "version bad":
					fresh.ExecutionContext.Version = 2
				case "version repaired":
					fresh.ExecutionContext.Version = 1
				case "metadata":
					fresh.Name = "concurrent rename"
					fresh.Inputs = map[string]any{"new": "value"}
				}
			}}
			calls := 0
			eng := admissionSnapshotEngine(t, wrapped, nil, &calls)
			gateErr := eng.admitRun(t.Context(), row.ID, row)
			fresh, err := base.LoadRun(t.Context(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("case=%s conflict=%d attempts=%d gateError=%v callerContext=%+v savedAdmission=%+v", name, wrapped.conflicts, wrapped.attempts, gateErr, row.ExecutionContext, fresh.Admission)
			if wrapped.conflicts != 1 || wrapped.attempts != 2 {
				t.Fatal("fixture did not cross exactly one CAS conflict")
			}
			if (gateErr != nil) != wantDeny || fresh.Admission == nil || fresh.Admission.Code != wantCode {
				t.Fatalf("want deny=%v code=%s", wantDeny, wantCode)
			}
			if !reflect.DeepEqual(row.ExecutionContext, fresh.ExecutionContext) || row.Trust != fresh.Trust || row.Name != fresh.Name || !reflect.DeepEqual(row.Inputs, fresh.Inputs) {
				t.Fatal("caller keeps stale admitted snapshot")
			}
		})
	}
}

func TestAdmissionStatusChangeDuringAdmission(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, status := range []store.RunStatus{store.RunStatusFinished, store.RunStatusCancelled, store.RunStatusQueued} {
			t.Run(map[bool]string{false: "run", true: "resume"}[resume]+"/"+string(status), func(t *testing.T) {
				base := tmpStore(t)
				ref, err := store.ResolveStoreContextRef(t.Context(), base)
				if err != nil {
					t.Fatal(err)
				}
				c := &store.ExecutionContext{Version: 1, Policy: store.ContextPolicyEnforce, RunStore: ref}
				row, err := base.CreateRun(t.Context(), "cas-status", "admission", nil)
				if err != nil {
					t.Fatal(err)
				}
				row.ExecutionContext = c
				if resume {
					row.Status = store.RunStatusFailedResumable
				}
				if err := base.SaveRun(t.Context(), row); err != nil {
					t.Fatal(err)
				}
				wrapped := &admissionSnapshotStore{RunStore: base, change: func(fresh *store.Run) { fresh.Status = status }}
				calls := 0
				eng := admissionSnapshotEngine(t, wrapped, c, &calls)
				if resume {
					err = eng.Resume(t.Context(), row.ID, nil)
				} else {
					err = eng.Run(t.Context(), row.ID, nil)
				}
				fresh, loadErr := base.LoadRun(t.Context(), row.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				t.Logf("resume=%v changedStatus=%s conflicts=%d err=%v calls=%d finalStatus=%s admission=%+v", resume, status, wrapped.conflicts, err, calls, fresh.Status, fresh.Admission)
				if !wrapped.changed || wrapped.conflicts == 0 {
					t.Fatal("no CAS interleaving")
				}
				if calls != 0 {
					t.Fatal("execution reached after concurrent status changed during admission")
				}
				if fresh.Status != status || fresh.Admission != nil {
					t.Fatal("stopped admission overwrote the concurrent state")
				}
			})
		}
	}
}
