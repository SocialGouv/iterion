package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"strings"
	"testing"
)

type observedReceiptCutStore struct {
	*store.FilesystemRunStore
	cut bool
}

func (s *observedReceiptCutStore) RecordOutputCorrectionReceipt(ctx context.Context, id string, target store.OutputCorrectionReceiptTarget, receipt store.OutputCorrectionReceipt) (bool, error) {
	ok, err := s.FilesystemRunStore.RecordOutputCorrectionReceipt(ctx, id, target, receipt)
	if err == nil && s.cut {
		s.cut = false
		return false, errors.New("simulated loss after durable receipt before runtime accounting")
	}
	return ok, err
}
func TestObservedReceiptPreflightBeforeNextModel(t *testing.T) {
	for _, cut := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-booking", false: "after-checkpoint"}[cut], func(t *testing.T) {
			fs, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s := &observedReceiptCutStore{FilesystemRunStore: fs, cut: cut}
			wf := correctionEngineWorkflow()
			wf.Budget = &ir.Budget{MaxTokens: 10}
			calls := 0
			backend := observedBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
				calls++
				return delegate.Result{Output: map[string]any{"reply": "missing-ok"}, Tokens: 20, CostUSD: 1}, nil
			})
			makeEngine := func(st store.RunStore) *Engine {
				reg := delegate.NewRegistry()
				reg.Register(delegate.BackendClaudeCode, backend)
				exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
				return New(wf, st, exec, WithOutputCorrectionBudget(0))
			}
			if err = makeEngine(s).Run(context.Background(), "preflight", nil); err == nil {
				t.Fatal("invalid first result succeeded")
			}
			first, err := fs.LoadRun(context.Background(), "preflight")
			if err != nil {
				t.Fatal(err)
			}
			if len(first.InitialUsageReservations) != 1 || calls != 1 {
				t.Fatalf("first receipt/call %+v %d", first.InitialUsageReservations, calls)
			}
			if cut && first.Checkpoint != nil && first.Checkpoint.BudgetTokensUsed != 0 {
				t.Fatal("crash cut accidentally accounted before restart")
			}
			reopened, err := store.New(fs.Root())
			if err != nil {
				t.Fatal(err)
			}
			err = makeEngine(reopened).Resume(context.Background(), "preflight", nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "budget") || calls != 1 {
				t.Fatalf("restart bought another model calls=%d err=%v", calls, err)
			}
			final, err := reopened.LoadRun(context.Background(), "preflight")
			if err != nil {
				t.Fatal(err)
			}
			if len(final.InitialUsageReservations) != 1 {
				t.Fatal("preflight reserved another paid invocation")
			}
			t.Logf("cut=%v calls=%d restart refused before model", cut, calls)
		})
	}
}
