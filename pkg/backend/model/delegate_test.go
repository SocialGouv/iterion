package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// Test doubles for delegation
// ---------------------------------------------------------------------------

// stubBackend implements delegate.Backend for testing.
type stubBackend struct {
	mu       sync.Mutex
	calls    int
	results  []delegate.Result
	errors   []error
	fallback delegate.Result // used when calls exceeds len(results)
}

func (b *stubBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	idx := b.calls
	b.calls++
	b.mu.Unlock()

	if idx < len(b.errors) && b.errors[idx] != nil {
		return delegate.Result{}, b.errors[idx]
	}
	if idx < len(b.results) {
		return b.results[idx], nil
	}
	return b.fallback, nil
}

func newDelegateTestExecutor(backend delegate.Backend, hooks EventHooks) *ClawExecutor {
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{
			"sys": {Body: "system prompt"},
			"usr": {Body: "user prompt"},
		},
		Schemas: map[string]*ir.Schema{},
	}

	return NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithEventHooks(hooks),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: 10 * time.Millisecond}),
	)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDelegation_EmitsStartedAndFinished(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output:          map[string]any{"result": "ok"},
			Tokens:          100,
			Duration:        500 * time.Millisecond,
			BackendName:     "test_backend",
			RawOutputLen:    42,
			EffectiveModel:  "glm-4.6",
			ContextWindow:   200_000,
			MaxOutputTokens: 8192,
		}},
	}

	var startedCalls, finishedCalls int
	var startedInfo, finishedInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted: func(nodeID string, info DelegateInfo) {
			startedCalls++
			startedInfo = info
		},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			finishedCalls++
			finishedInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "test_node"},
		LLMFields: ir.LLMFields{Backend: "test_backend", Model: "anthropic/claude-opus-5"},
	}

	output, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if startedCalls != 1 {
		t.Errorf("expected 1 OnDelegateStarted call, got %d", startedCalls)
	}
	if startedInfo.BackendName != "test_backend" {
		t.Errorf("expected backend 'test_backend', got %q", startedInfo.BackendName)
	}
	if startedInfo.DeclaredModel != "anthropic/claude-opus-5" {
		t.Errorf("started DeclaredModel = %q, want anthropic/claude-opus-5", startedInfo.DeclaredModel)
	}
	if finishedCalls != 1 {
		t.Errorf("expected 1 OnDelegateFinished call, got %d", finishedCalls)
	}
	if finishedInfo.BackendName != "test_backend" {
		t.Errorf("expected backend 'test_backend' in info, got %q", finishedInfo.BackendName)
	}
	if finishedInfo.Tokens != 100 {
		t.Errorf("expected 100 tokens, got %d", finishedInfo.Tokens)
	}
	if finishedInfo.RawOutputLen != 42 {
		t.Errorf("expected raw output len 42, got %d", finishedInfo.RawOutputLen)
	}
	if finishedInfo.DeclaredModel != "anthropic/claude-opus-5" {
		t.Errorf("finished DeclaredModel = %q", finishedInfo.DeclaredModel)
	}
	if finishedInfo.EffectiveModel != "glm-4.6" {
		t.Errorf("finished EffectiveModel = %q, want glm-4.6 (captured then dropped before #474)", finishedInfo.EffectiveModel)
	}
	if finishedInfo.ContextWindow != 200_000 || finishedInfo.MaxOutputTokens != 8192 {
		t.Errorf("window fields dropped: window=%d max_out=%d", finishedInfo.ContextWindow, finishedInfo.MaxOutputTokens)
	}

	// Verify metadata is attached to output.
	if output["_backend"] != "test_backend" {
		t.Errorf("expected _backend='test_backend', got %v", output["_backend"])
	}
	if output["_tokens"] != 100 {
		t.Errorf("expected _tokens=100, got %v", output["_tokens"])
	}
}

// TestDelegation_InteractionSignalSkipsSchemaValidation is the regression
// guard for the ask_user-on-schema+tools bug: a node with BOTH an output
// schema AND interaction, whose backend returns a _needs_interaction pause
// Result, must surface ErrNeedsInteraction WITHOUT first running schema
// validation. Validating the {_needs_interaction:…} Output against the
// node's schema would fail and trigger the schema-validation backend retry,
// which replays the unanswered tool_call into a fresh generation (openai
// 400 "tool_call_ids did not have response messages"). See
// docs/bot-runs/evolve.md. The fix moves the interaction short-circuit
// ahead of schema validation in executor.go.
func TestDelegation_InteractionSignalSkipsSchemaValidation(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output: map[string]any{
				"_needs_interaction": true,
				"_interaction_questions": map[string]any{
					"ask_user_response": "What is your favorite color?",
				},
			},
			BackendName: "test_backend",
		}},
		// If the executor wrongly retries (the bug), the 2nd call returns
		// this schema-shaped Result and the test would NOT see a pause —
		// the calls-count assertion catches the regression either way.
		fallback: delegate.Result{
			Output:      map[string]any{"answer": "blue"},
			BackendName: "test_backend",
		},
	}

	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"out_schema": {
				Name:   "out_schema",
				Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
			},
		},
	}
	exec := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
	)

	node := &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "investigate"},
		LLMFields:         ir.LLMFields{Backend: "test_backend"},
		SchemaFields:      ir.SchemaFields{OutputSchema: "out_schema"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}

	_, err := exec.executeBackend(context.Background(), node, map[string]any{})
	var ni *ErrNeedsInteraction
	if !errors.As(err, &ni) {
		t.Fatalf("expected *ErrNeedsInteraction (clean pause), got %T: %v", err, err)
	}
	if got := ni.Questions["ask_user_response"]; got != "What is your favorite color?" {
		t.Errorf("interaction question not propagated: %v", ni.Questions)
	}
	if backend.calls != 1 {
		t.Fatalf("expected exactly 1 backend call (interaction must skip schema-validation retry), got %d", backend.calls)
	}
}

func TestDelegation_EmitsErrorOnFailure(t *testing.T) {
	// Non-retryable error (no "exit status" or "signal:" in message).
	backend := &stubBackend{
		errors: []error{fmt.Errorf("delegate: parse error: invalid JSON")},
	}

	var errorCalls int
	var errorInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted: func(nodeID string, info DelegateInfo) {},
		OnDelegateError: func(nodeID string, info DelegateInfo) {
			errorCalls++
			errorInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "fail_node"},
		LLMFields: ir.LLMFields{Backend: "test_backend"},
	}

	_, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}

	if errorCalls != 1 {
		t.Errorf("expected 1 OnDelegateError call, got %d", errorCalls)
	}
	if errorInfo.BackendName != "test_backend" {
		t.Errorf("expected backend 'test_backend', got %q", errorInfo.BackendName)
	}
	if errorInfo.Error == nil {
		t.Error("expected non-nil Error in DelegateInfo")
	}
}

func TestDelegation_EmitsRetryOnTransientError(t *testing.T) {
	// First call fails with retryable error (signal-based exit), second succeeds.
	backend := &stubBackend{
		errors: []error{fmt.Errorf("delegate: exit status 137")},
		results: []delegate.Result{
			{}, // placeholder for first call (error)
			{
				Output:      map[string]any{"result": "ok"},
				Tokens:      50,
				BackendName: "test_backend",
			},
		},
	}

	var retryCalls int
	var retryInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted:  func(nodeID string, info DelegateInfo) {},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {},
		OnDelegateRetry: func(nodeID string, info DelegateInfo) {
			retryCalls++
			retryInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "retry_node"},
		LLMFields: ir.LLMFields{Backend: "test_backend"},
	}

	_, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if retryCalls != 1 {
		t.Errorf("expected 1 OnDelegateRetry call, got %d", retryCalls)
	}
	if retryInfo.Attempt != 1 {
		t.Errorf("expected attempt 1, got %d", retryInfo.Attempt)
	}
	if retryInfo.BackendName != "test_backend" {
		t.Errorf("expected backend 'test_backend', got %q", retryInfo.BackendName)
	}
	if retryInfo.Error == nil {
		t.Error("expected non-nil Error in retry info")
	}
}

func TestDelegation_ParseFallbackMetadata(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output:        map[string]any{"text": "plain text response"},
			Tokens:        30,
			BackendName:   "test_backend",
			ParseFallback: true,
		}},
	}

	var finishedInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted: func(nodeID string, info DelegateInfo) {},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			finishedInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "fallback_node"},
		LLMFields: ir.LLMFields{Backend: "test_backend"},
	}

	output, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !finishedInfo.ParseFallback {
		t.Error("expected ParseFallback=true in DelegateInfo")
	}

	// Verify _parse_fallback metadata is added to output.
	if output["_parse_fallback"] != true {
		t.Error("expected _parse_fallback=true in output")
	}
}

// ---------------------------------------------------------------------------
// LLM router delegation tests
// ---------------------------------------------------------------------------

func TestLLMRouterDelegated_SelectsRoute(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output: map[string]any{
				"selected_route": "agent_a",
				"reasoning":      "code issues dominate",
			},
			Tokens: 100,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "fix_router"},
		LLMFields:  ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"},
		RouterMode: ir.RouterLLM,
	}

	input := map[string]any{
		"_route_candidates": []string{"agent_a", "agent_b"},
		"code_review":       "some review",
	}

	output, err := exec.executeLLMRouterUnified(context.Background(), node, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := output["selected_route"]; got != "agent_a" {
		t.Errorf("expected selected_route=agent_a, got %v", got)
	}
	if got := output["_backend"]; got != "test_backend" {
		t.Errorf("expected _backend=test_backend, got %v", got)
	}
}

func TestLLMRouterDelegated_MultiRoute(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output: map[string]any{
				"selected_routes": []any{"agent_a", "agent_b"},
				"reasoning":       "both routes needed",
			},
			Tokens: 120,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "multi_router"},
		LLMFields:   ir.LLMFields{Backend: "test_backend"},
		RouterMode:  ir.RouterLLM,
		RouterMulti: true,
	}

	input := map[string]any{
		"_route_candidates": []string{"agent_a", "agent_b", "agent_c"},
	}

	output, err := exec.executeLLMRouterUnified(context.Background(), node, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	routes, ok := output["selected_routes"]
	if !ok {
		t.Fatal("expected selected_routes in output")
	}
	routeSlice, ok := routes.([]any)
	if !ok {
		t.Fatalf("expected []interface{}, got %T", routes)
	}
	if len(routeSlice) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(routeSlice))
	}
}

func TestLLMRouterDelegated_ParseFallbackJSON(t *testing.T) {
	// Backend returns text-wrapped output, but text contains valid JSON.
	backend := &stubBackend{
		results: []delegate.Result{{
			Output:        map[string]any{"text": `{"selected_route":"agent_b","reasoning":"arch issue"}`},
			ParseFallback: true,
			Tokens:        50,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "router"},
		LLMFields:  ir.LLMFields{Backend: "test_backend"},
		RouterMode: ir.RouterLLM,
	}

	input := map[string]any{
		"_route_candidates": []string{"agent_a", "agent_b"},
	}

	output, err := exec.executeLLMRouterUnified(context.Background(), node, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := output["selected_route"]; got != "agent_b" {
		t.Errorf("expected selected_route=agent_b, got %v", got)
	}
}

func TestLLMRouterDelegated_ParseFallbackPlainTextFails(t *testing.T) {
	// Backend returns plain text that isn't JSON — should fail.
	backend := &stubBackend{
		results: []delegate.Result{{
			Output:        map[string]any{"text": "I think agent_a is best"},
			ParseFallback: true,
			Tokens:        30,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "router"},
		LLMFields:  ir.LLMFields{Backend: "test_backend"},
		RouterMode: ir.RouterLLM,
	}

	input := map[string]any{
		"_route_candidates": []string{"agent_a", "agent_b"},
	}

	_, err := exec.executeLLMRouterUnified(context.Background(), node, input)
	if err == nil {
		t.Fatal("expected error for plain text fallback")
	}
}
