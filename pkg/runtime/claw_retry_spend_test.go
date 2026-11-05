package runtime

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestClawFailedRetriesBookEveryCall(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"gw/m":{"context_window":256000,"input_usd_per_mtok":0.672,"output_usd_per_mtok":2.016}}`)
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
	wf := compileBotText(t, `schema result:
  payload: string

agent gw:
  backend: claw
  model: "openai_compatible/gw/m"
  user: "Return the payload."
  output: result
  tools: []

workflow w:
  worktree: none
  sandbox: none
  entry: gw
  budget:
    max_tokens: 10000
  gw -> done
`)
	var scripts [][]api.StreamEvent
	for _, inputTokens := range []int{1130, 1267} {
		events := askToolUse("result", "structured_output", `{"payload":null}`)
		events[0].InputTokens = inputTokens
		events[4].Usage = api.UsageDelta{OutputTokens: 125, Reported: true}
		scripts = append(scripts, events)
	}
	mock := &askMock{scripts: scripts}
	reg := model.NewRegistry()
	reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
	ws := t.TempDir()
	exec := model.NewClawExecutor(reg, wf, model.WithWorkDir(ws), model.WithBackendRegistry(br),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	t.Cleanup(func() { _ = exec.Close() })
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		if prior(ErrCodeExecutionFailed) == 0 {
			return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeExecutionFailed
		}
		return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
	})
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	eng := New(wf, st, exec, WithWorkDir(ws), WithSandboxOverride("none"), WithRecoveryDispatch(dispatch))
	if err := eng.Run(t.Context(), "claw-retry-spend", nil); err == nil || !strings.Contains(err.Error(), `field "payload" is null`) {
		t.Fatalf("expected schema failure after two calls, got %v", err)
	}
	if len(mock.calls) != 2 {
		t.Fatalf("expected two paid calls, got %d", len(mock.calls))
	}
	cp := st.lastCheckpoint()
	if cp == nil {
		t.Fatal("no failure checkpoint")
	}
	if cp.BudgetTokensUsed != 2647 {
		t.Errorf("failed claw retries booked %d tokens, want 2647 (1255 + 1392)", cp.BudgetTokensUsed)
	}
	if math.Abs(cp.BudgetCostUSD-0.002114784) > 1e-12 {
		t.Errorf("failed claw retries booked %.9f USD, want 0.002114784", cp.BudgetCostUSD)
	}
}

func TestRecoveryCumulativeCostLeavesBudgetForCorrection(t *testing.T) {
	wf := validationWorkflow()
	wf.Budget = &ir.Budget{MaxCostUSD: 2.5}
	exec := &usageCorrectingExecutor{stubExecutor: newStubExecutor()}
	attempts := 0
	exec.on("my_agent", func(_ map[string]any) (map[string]any, error) {
		attempts++
		if attempts == 1 {
			return map[string]any{"_tokens": 100, "_cost_usd": 1.0, "_cost_is_session_total": true, "_backend": "claude_code", "_session_id": "same"}, errors.New("stream closed")
		}
		return map[string]any{"summary": "initial", "score": "bad", "_tokens": 100, "_cost_usd": 1.6, "_cost_is_session_total": true, "_backend": "claude_code", "_session_id": "same"}, nil
	})
	exec.correct = func(_ context.Context, _ map[string]any, _ error) (map[string]any, OutputCorrectionUsage, error) {
		return map[string]any{"summary": "repaired", "score": 7}, OutputCorrectionUsage{Tokens: 10, CostUSD: .1}, nil
	}
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		if prior(ErrCodeExecutionFailed) > 0 {
			return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeExecutionFailed
		}
		return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeExecutionFailed
	})
	st := tmpStore(t)
	err := New(wf, st, exec, WithRecoveryDispatch(dispatch), WithOutputValidation(true), WithOutputCorrectionBudget(2)).Run(context.Background(), "review-g2-correction", nil)
	if err != nil {
		t.Fatalf("actual session total 1.6 below correction boundary 2.25, correction calls=%d: %v", exec.calls, err)
	}
	if exec.calls != 1 {
		t.Fatalf("correction calls=%d want 1", exec.calls)
	}
	run, loadErr := st.LoadRun(context.Background(), "review-g2-correction")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	cp := run.Checkpoint
	if cp == nil || cp.BudgetTokensUsed != 210 || math.Abs(cp.BudgetCostUSD-1.7) > 1e-12 {
		t.Fatalf("corrected retry must book 210 tokens and 1.7 USD: %+v", cp)
	}
}
