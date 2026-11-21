package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func TestObservedLostAuthorityIsNonOutcome(t *testing.T) {
	for _, mode := range []string{"trunk", "branch", "reinvoke"} {
		t.Run(mode, func(t *testing.T) {
			s, x := correctionFixture(t)
			wf := correctionEngineWorkflow()
			if mode == "branch" {
				wf.Entry = "fork"
				wf.Nodes["fork"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fork"}, RouterMode: ir.RouterFanOutAll}
				wf.Nodes["done"].(*ir.DoneNode).AwaitMode = ir.AwaitBestEffort
				wf.Edges = []*ir.Edge{{From: "fork", To: "worker"}, {From: "worker", To: "done"}}
			}
			calls := 0
			var successor store.RunExecutionAuthority
			b := observedBackend(func(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
				calls++
				if err := s.UpdateRunStatus(ctx, "correction", store.RunStatusPausedOperator, ""); err != nil {
					t.Fatal(err)
				}
				r := readCorrection(t, s)
				var ok bool
				var err error
				successor, ok, err = s.AcquireRunExecutionIf(ctx, r.ID, store.RunExecutionExpectation{Version: r.CASVersion, Epoch: r.ExecutionEpoch, Status: r.Status, QueuedAt: r.QueuedAt}, store.ExecutionResume)
				if err != nil || !ok {
					t.Fatal(ok, err)
				}
				return delegate.Result{Output: map[string]any{"reply": "x", "ok": true}, Tokens: 7, CostUSD: 1}, nil
			})
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, b)
			ex := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			e := New(wf, s, ex)
			var err error
			if mode == "reinvoke" {
				rs := e.newRunState("correction", nil)
				rs.ctx = x.context(context.Background())
				err = e.reInvokeBackend(rs.ctx, rs, "worker", wf.Nodes["worker"], &model.ErrNeedsInteraction{NodeID: "worker", Backend: delegate.BackendClaudeCode, SessionID: "s", Questions: map[string]any{"q": "yes?"}}, map[string]any{"answer": "yes"}, 0)
			} else {
				err = e.Run(context.Background(), "correction", nil)
			}
			r := readCorrection(t, s)
			if calls != 1 || !errors.Is(err, ErrAdmissionStopped) || r.Status != store.RunStatusRunning || r.ExecutionEpoch != successor.Epoch {
				t.Fatalf("lost owner wrote outcome: mode=%s calls=%d err=%v status=%s epoch=%s successor=%s", mode, calls, err, r.Status, r.ExecutionEpoch, successor.Epoch)
			}
			paid := 0
			for _, res := range r.InitialUsageReservations {
				if res.Receipt != nil {
					paid += res.Receipt.Usage.Tokens
					if res.ExecutionEpoch == successor.Epoch {
						t.Fatal("receipt retargeted successor")
					}
				}
			}
			if paid != 7 {
				t.Fatalf("stopped attempt lost bill %d", paid)
			}
		})
	}
}
