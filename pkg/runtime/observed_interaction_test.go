package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"strings"
	"testing"
)

func TestObservedInteractionIndependentSpend(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     ir.InteractionMode
		escalate bool
	}{
		{"llm", ir.InteractionLLM, false},
		{"llm_or_human_answers", ir.InteractionLLMOrHuman, false},
		{"llm_or_human_escalates", ir.InteractionLLMOrHuman, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := interactionWorkflow(tc.mode)
			wf.Budget = &ir.Budget{MaxTokens: 1000}
			node := wf.Nodes["worker"].(*ir.AgentNode)
			node.Backend = "metered_stub"
			node.InteractionModel = "anthropic/claude-opus-5"
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return &meteredAnswerClient{escalate: tc.escalate}, nil })
			backend := &trustedMeteringBackend{asks: true, result: delegate.Result{Output: map[string]any{"answer": "done"}}}
			delegates := delegate.NewRegistry()
			delegates.Register("metered_stub", backend)
			exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(delegates), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			st := tmpStore(t)
			eng := New(wf, st, exec, WithWorkDir(t.TempDir()))
			err := eng.Run(context.Background(), "trusted-interaction", map[string]any{"task": "pick one"})
			if tc.escalate && !errors.Is(err, ErrRunPaused) || !tc.escalate && err != nil {
				t.Fatalf("run: %v", err)
			}
			stored, loadErr := st.LoadRun(context.Background(), "trusted-interaction")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			cp := stored.Checkpoint
			if cp == nil || cp.BudgetTokensUsed != 100 || cp.BudgetCostUSD <= 0 {
				t.Fatalf("the auto-answer's bill does not reach the run: %+v", cp)
			}
			if !tc.escalate {
				if len(backend.tasks) != 2 {
					t.Fatalf("delegate calls = %d, want 2", len(backend.tasks))
				}
				prompt := backend.tasks[1].UserPrompt
				if !strings.Contains(prompt, "postgres") || strings.Contains(prompt, "_tokens") || strings.Contains(prompt, "_invented") {
					t.Fatalf("the next turn does not receive clean answers: %s", prompt)
				}
			}
		})
	}
}
