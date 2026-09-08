package model

import (
	"context"
	"encoding/json"
	"testing"

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
