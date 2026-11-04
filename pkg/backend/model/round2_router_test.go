package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The LLM router cannot choose a route by extracting the parse diagnostic.
func TestR2_RouterRouteFromTheErrorDescription(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	node := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"selected_route":"agent_`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"selected_route":"agent_`, 50, 20),
		toolUseEvents("tu_3", "structured_output", `{"selected_route":"agent_`, 60, 20),
		toolUseEvents("tu_4", "structured_output", `{"selected_route":"agent_`, 60, 20),
	)
	extraction := newMockClient(
		toolUseEvents("ex_1", "structured_output", `{"selected_route":"agent_b","reasoning":"the answer named an agent route"}`, 200, 30),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return node, nil })
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{"sys": {Body: "Pick the route."}, "usr": {Body: "Which agent handles this?"}}, Schemas: map[string]*ir.Schema{}}
	exec := newTestClawExecutor(reg, wf, WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	router := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "pick"},
		LLMFields:  ir.LLMFields{Model: "test/test-model", SystemPrompt: "sys", UserPrompt: "usr"},
		RouterMode: ir.RouterLLM,
	}
	out, err := exec.executeLLMRouterUnified(WithRunID(context.Background(), "run-router"), router, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}})
	var schemaErr *OutputSchemaError
	if !errors.As(err, &schemaErr) || len(extraction.getCalls()) != 0 {
		t.Fatalf("router accepted error metadata: err=%v extraction=%d out=%v", err, len(extraction.getCalls()), out)
	}
	t.Logf("router err=%v", err)
	t.Logf("router output selected_route=%v reasoning=%v", out["selected_route"], out["reasoning"])
	t.Logf("router provider calls=%d extraction calls=%d", len(node.getCalls()), len(extraction.getCalls()))
}
