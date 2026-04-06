package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
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

func newDelegateTestExecutor(backend delegate.Backend, hooks EventHooks) *GoaiExecutor {
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)

	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{
			"sys": {Body: "system prompt"},
			"usr": {Body: "user prompt"},
		},
		Schemas: map[string]*ir.Schema{},
	}

	return NewGoaiExecutor(NewRegistry(), wf,
		WithDelegateRegistry(reg),
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
			Output:       map[string]interface{}{"result": "ok"},
			Tokens:       100,
			Duration:     500 * time.Millisecond,
			BackendName:  "test_backend",
			RawOutputLen: 42,
		}},
	}

	var startedCalls, finishedCalls int
	var startedBackend string
	var finishedInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted: func(nodeID string, backendName string) {
			startedCalls++
			startedBackend = backendName
		},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			finishedCalls++
			finishedInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.Node{
		ID:       "test_node",
		Kind:     ir.NodeAgent,
		Delegate: "test_backend",
	}

	output, err := exec.executeDelegation(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if startedCalls != 1 {
		t.Errorf("expected 1 OnDelegateStarted call, got %d", startedCalls)
	}
	if startedBackend != "test_backend" {
		t.Errorf("expected backend 'test_backend', got %q", startedBackend)
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

	// Verify metadata is attached to output.
	if output["_delegate"] != "test_backend" {
		t.Errorf("expected _delegate='test_backend', got %v", output["_delegate"])
	}
	if output["_tokens"] != 100 {
		t.Errorf("expected _tokens=100, got %v", output["_tokens"])
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
		OnDelegateStarted: func(nodeID string, backendName string) {},
		OnDelegateError: func(nodeID string, info DelegateInfo) {
			errorCalls++
			errorInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.Node{
		ID:       "fail_node",
		Kind:     ir.NodeAgent,
		Delegate: "test_backend",
	}

	_, err := exec.executeDelegation(context.Background(), node, map[string]interface{}{})
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
				Output:      map[string]interface{}{"result": "ok"},
				Tokens:      50,
				BackendName: "test_backend",
			},
		},
	}

	var retryCalls int
	var retryInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted:  func(nodeID string, backendName string) {},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {},
		OnDelegateRetry: func(nodeID string, info DelegateInfo) {
			retryCalls++
			retryInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.Node{
		ID:       "retry_node",
		Kind:     ir.NodeAgent,
		Delegate: "test_backend",
	}

	_, err := exec.executeDelegation(context.Background(), node, map[string]interface{}{})
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
			Output:        map[string]interface{}{"text": "plain text response"},
			Tokens:        30,
			BackendName:   "test_backend",
			ParseFallback: true,
		}},
	}

	var finishedInfo DelegateInfo

	hooks := EventHooks{
		OnDelegateStarted: func(nodeID string, backendName string) {},
		OnDelegateFinished: func(nodeID string, info DelegateInfo) {
			finishedInfo = info
		},
	}

	exec := newDelegateTestExecutor(backend, hooks)

	node := &ir.Node{
		ID:       "fallback_node",
		Kind:     ir.NodeAgent,
		Delegate: "test_backend",
	}

	output, err := exec.executeDelegation(context.Background(), node, map[string]interface{}{})
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
			Output: map[string]interface{}{
				"selected_route": "agent_a",
				"reasoning":      "code issues dominate",
			},
			Tokens: 100,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.Node{
		ID:           "fix_router",
		Kind:         ir.NodeRouter,
		RouterMode:   ir.RouterLLM,
		Delegate:     "test_backend",
		SystemPrompt: "sys",
	}

	input := map[string]interface{}{
		"_route_candidates": []string{"agent_a", "agent_b"},
		"code_review":       "some review",
	}

	output, err := exec.executeLLMRouterDelegated(context.Background(), node, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := output["selected_route"]; got != "agent_a" {
		t.Errorf("expected selected_route=agent_a, got %v", got)
	}
	if got := output["_delegate"]; got != "test_backend" {
		t.Errorf("expected _delegate=test_backend, got %v", got)
	}
}

func TestLLMRouterDelegated_MultiRoute(t *testing.T) {
	backend := &stubBackend{
		results: []delegate.Result{{
			Output: map[string]interface{}{
				"selected_routes": []interface{}{"agent_a", "agent_b"},
				"reasoning":       "both routes needed",
			},
			Tokens: 120,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.Node{
		ID:          "multi_router",
		Kind:        ir.NodeRouter,
		RouterMode:  ir.RouterLLM,
		Delegate:    "test_backend",
		RouterMulti: true,
	}

	input := map[string]interface{}{
		"_route_candidates": []string{"agent_a", "agent_b", "agent_c"},
	}

	output, err := exec.executeLLMRouterDelegated(context.Background(), node, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	routes, ok := output["selected_routes"]
	if !ok {
		t.Fatal("expected selected_routes in output")
	}
	routeSlice, ok := routes.([]interface{})
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
			Output:        map[string]interface{}{"text": `{"selected_route":"agent_b","reasoning":"arch issue"}`},
			ParseFallback: true,
			Tokens:        50,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.Node{
		ID:         "router",
		Kind:       ir.NodeRouter,
		RouterMode: ir.RouterLLM,
		Delegate:   "test_backend",
	}

	input := map[string]interface{}{
		"_route_candidates": []string{"agent_a", "agent_b"},
	}

	output, err := exec.executeLLMRouterDelegated(context.Background(), node, input)
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
			Output:        map[string]interface{}{"text": "I think agent_a is best"},
			ParseFallback: true,
			Tokens:        30,
		}},
	}

	exec := newDelegateTestExecutor(backend, EventHooks{})

	node := &ir.Node{
		ID:         "router",
		Kind:       ir.NodeRouter,
		RouterMode: ir.RouterLLM,
		Delegate:   "test_backend",
	}

	input := map[string]interface{}{
		"_route_candidates": []string{"agent_a", "agent_b"},
	}

	_, err := exec.executeLLMRouterDelegated(context.Background(), node, input)
	if err == nil {
		t.Fatal("expected error for plain text fallback")
	}
}
