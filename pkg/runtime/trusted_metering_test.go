package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type trustedMeteringBackend struct {
	result delegate.Result
	asks   bool
	tasks  []delegate.Task
}

func (b *trustedMeteringBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	if b.asks && len(b.tasks) == 1 {
		return delegate.Result{}, &model.ErrNeedsInteraction{
			NodeID: "worker", Questions: map[string]any{delegate.AskUserQuestionKey: "which database?"},
		}
	}
	return b.result, nil
}

// W1/W2: the JSON is the model's, Result.Tokens is the harness's. Restoring
// the nil-only stamp books 0 or 99999999 instead of 42 against the real run.
func TestTrustedDelegateSpendReachesTheRun(t *testing.T) {
	for _, forged := range []int{0, 99999999} {
		t.Run(fmt.Sprint(forged), func(t *testing.T) {
			var output map[string]any
			if err := json.Unmarshal([]byte(fmt.Sprintf(`{"answer":"blue","_tokens":%d,"_invented":true}`, forged)), &output); err != nil {
				t.Fatal(err)
			}
			backend := &trustedMeteringBackend{result: delegate.Result{Output: output, Tokens: 42, CostUSD: 0.25}}
			wf := interactionWorkflow(ir.InteractionNone)
			wf.Budget = &ir.Budget{MaxTokens: 1000}
			wf.Nodes["worker"].(*ir.AgentNode).Backend = "metered_stub"
			delegates := delegate.NewRegistry()
			delegates.Register("metered_stub", backend)
			exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(delegates))
			st := &checkpointCapturingStore{RunStore: tmpStore(t)}
			eng := New(wf, st, exec, WithWorkDir(t.TempDir()))
			if err := eng.Run(context.Background(), "trusted-delegate", nil); err != nil {
				t.Fatal(err)
			}
			cp := st.lastCheckpoint()
			if cp == nil || cp.BudgetTokensUsed != 42 || cp.BudgetCostUSD != 0.25 {
				t.Fatalf("the harness bill does not reach the run: %+v", cp)
			}
			if _, exists := output["_invented"]; exists {
				t.Fatal("model-authored reserved key survives")
			}
		})
	}
}

type meteredAnswerClient struct{ escalate bool }

func (c *meteredAnswerClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	answer := fmt.Sprintf(`{"ask_user_response":"postgres","needs_human_input":%v,"_tokens":0,"_invented":true}`, c.escalate)
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 70},
		{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "answer", Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: answer}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 30}},
		{Type: api.EventMessageStop},
	}
	ch := make(chan api.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

// W3: deleting either success booking in resume.go loses 100 run tokens.
// Returning nil spend from ExecuteHumanLLMForInteraction loses them in both
// modes; forwarding spend as answers contaminates the re-invocation prompt.
func TestSuccessfulInteractionSpendReachesTheRun(t *testing.T) {
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
			st := &checkpointCapturingStore{RunStore: tmpStore(t)}
			eng := New(wf, st, exec, WithWorkDir(t.TempDir()))
			err := eng.Run(context.Background(), "trusted-interaction", map[string]any{"task": "pick one"})
			if tc.escalate && !errors.Is(err, ErrRunPaused) || !tc.escalate && err != nil {
				t.Fatalf("run: %v", err)
			}
			cp := st.lastCheckpoint()
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
