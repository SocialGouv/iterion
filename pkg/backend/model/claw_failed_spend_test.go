package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestClawBackendKeepsWhatAnAbandonedGenerationBurned drives the REAL
// ClawBackend against a scripted api.APIClient that bills a step and then
// dies — the shape of every claw failure that matters.
//
// The engine books a failed node's spend from the delegate.Result the backend
// returns beside its error. claw is in-process, so that Result is the ONLY
// place its usage exists: GenerateTextDirect hands back a best-effort partial
// carrying every completed step's tokens, and returning a bare
// delegate.Result{} made a tool loop that died on its twentieth step report
// the same zero as one that never reached the provider. claude_code has
// carried the figure through its typed failures all along; this is the parity
// half, and without it the run-level booking is inert on the default
// in-process backend.
func TestClawBackendKeepsWhatAnAbandonedGenerationBurned(t *testing.T) {
	schema := &ir.Schema{Name: "verdict", Fields: []*ir.SchemaField{{Name: "approved", Type: ir.FieldTypeBool}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	toolDefs := []delegate.ToolDef{{
		Name:        "noop",
		Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}

	// One script: the model bills 100 in / 30 out for a tool call, and the
	// NEXT request — whichever one it is per case — finds no script and fails.
	newBackend := func() (*ClawBackend, *mockAPIClient) {
		mock := newMockClient(toolUseEvents("tu_1", "noop", `{}`, 100, 30))
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
		return NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1}), mock
	}

	cases := []struct {
		name string
		task delegate.Task
	}{
		{
			// generateTextWithToolsAndSchema: the agentic path, and the
			// costliest — step 2 of the loop is the one that fails.
			name: "a text+tools loop that dies mid-step",
			task: delegate.Task{
				NodeID: "reviewer", Model: "test/test-model", UserPrompt: "Review.",
				OutputSchema: schemaJSON, HasTools: true, ToolDefs: toolDefs, ToolMaxSteps: 5,
			},
		},
		{
			// Same path, later exit: the loop ENDS at its step limit with no
			// final text, and the schema-forced recovery pass is the request
			// that fails. Two billed calls, nothing to show for either.
			name: "a tool loop whose structured recovery also fails",
			task: delegate.Task{
				NodeID: "reviewer", Model: "test/test-model", UserPrompt: "Review.",
				OutputSchema: schemaJSON, HasTools: true, ToolDefs: toolDefs, ToolMaxSteps: 1,
			},
		},
		{
			// generateText: no schema, so the plain path — which has the same
			// partial contract and dropped it the same way.
			name: "a schemaless tool loop that dies mid-step",
			task: delegate.Task{
				NodeID: "explorer", Model: "test/test-model", UserPrompt: "Explore.",
				HasTools: true, ToolDefs: toolDefs, ToolMaxSteps: 5,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, mock := newBackend()
			res, err := backend.Execute(context.Background(), tc.task)
			if err == nil {
				t.Fatal("precondition: the scripted client must fail this call")
			}
			if n := len(mock.getCalls()); n < 2 {
				t.Fatalf("precondition: want a billed call THEN a failing one, got %d call(s)", n)
			}
			if res.Tokens != 130 {
				t.Errorf("Result.Tokens = %d, want 130 (the step the provider billed)", res.Tokens)
			}
			// The map is what the engine reads (extractUsage), not the struct.
			if got := res.Output["_tokens"]; got != 130 {
				t.Errorf("_tokens = %v, want 130 — the engine books from the map", got)
			}
			if res.BackendName != delegate.BackendClaw {
				t.Errorf("BackendName = %q, want %q so the spend is attributed", res.BackendName, delegate.BackendClaw)
			}
		})
	}

	t.Run("a structured call the model answered off-schema", func(t *testing.T) {
		// generateStructured: no tools, so a single billed turn — and the
		// model answered with plain text instead of the synthetic tool_use
		// the schema forces. The call is fully billed; only its shape is
		// unusable.
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) {
			return newMockClient(textEvents("I would rather narrate.", 100, 30)), nil
		})
		backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
		res, err := backend.Execute(context.Background(), delegate.Task{
			NodeID: "judge", Model: "test/test-model", UserPrompt: "Judge.", OutputSchema: schemaJSON,
		})
		if err == nil {
			t.Fatal("precondition: text instead of the synthetic tool_use must fail")
		}
		if res.Tokens != 130 || res.Output["_tokens"] != 130 {
			t.Errorf("the billed structured turn was reported as free: Tokens=%d Output=%v", res.Tokens, res.Output)
		}
	})

	t.Run("a call that never reached the provider bills nothing", func(t *testing.T) {
		// The other half of the rule: no usage means the zero Result, not an
		// output map stamped `_tokens: 0` that reads as a result.
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return newMockClient(), nil })
		backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
		res, err := backend.Execute(context.Background(), delegate.Task{
			NodeID: "explorer", Model: "test/test-model", UserPrompt: "Explore.",
		})
		if err == nil {
			t.Fatal("precondition: a scriptless client must fail")
		}
		if res.Output != nil || res.Tokens != 0 {
			t.Errorf("a spendless failure must stay the zero Result, got %+v", res)
		}
	})
}

// TestClawParseFallbackPricesTheRecoveryPassItRan covers the OTHER exit past
// the schema-forced recovery pass — the one the recovery exists for.
//
// Both exits below it read the same `obj`: the tool loop narrated instead of
// answering, the recovery ran and was billed, and came back unusable. The
// empty-text sibling folds `obj.TotalUsage`; this one — reached whenever the
// loop left ANY text, which is the common shape — priced the result from the
// tool loop alone, so the whole recovery call reported zero and never reached
// `_cost_usd`, the caps, or the run's booking.
//
// It only became reachable-with-a-figure in this branch: GenerateObjectDirect
// used to return a bare nil beside its error, so there was nothing to fold.
// Adding the partial connected one end of that pipe and left this one open.
func TestClawParseFallbackPricesTheRecoveryPassItRan(t *testing.T) {
	schema := &ir.Schema{Name: "verdict", Fields: []*ir.SchemaField{{Name: "approved", Type: ir.FieldTypeBool}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}

	reg := NewRegistry()
	mock := newMockClient(
		// Step 1 — a real tool call (which is what keeps the nudge guard out
		// of this test's way), billed 100/30.
		toolUseEvents("tu_1", "noop", `{}`, 100, 30),
		// Step 2 — the loop ends on narrative prose, billed 40/10. Not JSON,
		// so the cheap parse below the loop cannot serve it and the recovery
		// pass fires.
		textEvents("I reviewed the diff. No findings.", 40, 10),
		// The recovery pass: schema forced, and the model narrates AGAIN
		// instead of emitting the synthetic tool_use. Fully billed at 20/5,
		// and unusable — `partial(usage)` beside "did not produce a tool_use
		// block".
		textEvents("Still narrating, sorry.", 20, 5),
	)
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})

	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "reviewer", Model: "test/test-model", UserPrompt: "Review.",
		OutputSchema: schemaJSON, HasTools: true, ToolMaxSteps: 5,
		ToolDefs: []delegate.ToolDef{{
			Name:        "noop",
			Description: "test tool",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Execute:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
		}},
	})
	if err != nil {
		t.Fatalf("the parse fallback surfaces the text, it does not fail: %v", err)
	}
	if !res.ParseFallback {
		t.Fatalf("precondition: want the parse-fallback exit, got %+v", res)
	}
	if n := len(mock.getCalls()); n != 3 {
		t.Fatalf("precondition: want 2 loop steps THEN the recovery pass, got %d call(s)", n)
	}
	// 130 (tool step) + 50 (final text) + 25 (the abandoned recovery pass).
	if res.Tokens != 205 {
		t.Errorf("Result.Tokens = %d, want 205 — the recovery pass was billed too", res.Tokens)
	}
	// The map is what the engine books from (extractUsage), not the struct.
	if got := res.Output["_tokens"]; got != 205 {
		t.Errorf("_tokens = %v, want 205 — the engine prices from the map", got)
	}
}

// TestClawRetryLoopKeepsEveryAttemptsSpend: the metered failure above is only
// worth what survives the retry loop wrapping it. `result, err = fn()`
// overwrote the previous attempt, so an expensive tool loop that hit a 429 and
// then retried into an instant, free failure reported the FREE one as the
// node's bill — the same drop this branch removed one frame down.
//
// The attempts SUM: claw opens a fresh conversation each time (it never reads
// SessionID), so no attempt's figure contains another's.
func TestClawRetryLoopKeepsEveryAttemptsSpend(t *testing.T) {
	backend := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond})
	task := delegate.Task{NodeID: "reviewer", Model: "test/test-model"}

	attempt := 0
	res, err := backend.retryLoop(context.Background(), task.NodeID, func() (delegate.Result, error) {
		attempt++
		if attempt == 1 {
			// A whole agentic turn, billed, then rate-limited: retryable, so
			// the loop buys a second one.
			return meteredFailure(task, Usage{InputTokens: 1_000, OutputTokens: 200}),
				&APIError{Message: "rate limited", StatusCode: 429, IsRetryable: true}
		}
		// The retry dies before the provider bills anything.
		return delegate.Result{}, &APIError{Message: "unauthorized", StatusCode: 401}
	})
	if err == nil {
		t.Fatal("precondition: the second attempt must fail terminally")
	}
	if attempt != 2 {
		t.Fatalf("precondition: want 2 attempts, got %d", attempt)
	}
	if res.Tokens != 1_200 {
		t.Errorf("Result.Tokens = %d, want 1200 — the retried attempt was billed", res.Tokens)
	}
	// The map is what the engine books from (extractUsage), not the struct.
	if got := res.Output["_tokens"]; got != 1_200 {
		t.Errorf("_tokens = %v, want 1200 — the engine books from the map", got)
	}
}

// TestHumanLLMHalfKeepsWhatItBurnedOnFailure: the llm half of a human node is
// a real LLM call, and when it fails the engine books what it burned from the
// map the executor returns beside the error, then degrades to the human
// pause. That booking is only as good as the map: executeHumanLLM returned a
// bare nil, so the figure — final, since a human answers next and a human
// reports no tokens — was lost at the last frame that could see it.
func TestHumanLLMHalfKeepsWhatItBurnedOnFailure(t *testing.T) {
	reg := NewRegistry()
	// A billed turn that answers with text instead of the synthetic tool_use
	// the schema forces: the provider charged, the node has nothing usable.
	reg.Register("test", func(string) (api.APIClient, error) {
		return newMockClient(textEvents("I would rather not answer in JSON.", 100, 30)), nil
	})
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"answer_schema": {
				Name:   "answer_schema",
				Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
			},
		},
	}
	exec := NewClawExecutor(reg, wf)
	node := &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "gate"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM},
		Model:             "test/test-model",
		SchemaFields:      ir.SchemaFields{OutputSchema: "answer_schema"},
	}

	output, err := exec.Execute(context.Background(), node, map[string]any{"q": "which db?"})
	if err == nil {
		t.Fatal("precondition: an off-schema answer must fail the llm half")
	}
	if output == nil {
		t.Fatal("the llm half's spend never left the executor: nil output")
	}
	if got := output["_tokens"]; got != 130 {
		t.Errorf("_tokens = %v, want 130 (the turn the provider billed)", got)
	}
}
