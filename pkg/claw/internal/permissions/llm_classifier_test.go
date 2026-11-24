package permissions

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// scriptedClient returns a single canned response.
type scriptedClient struct {
	body string
}

func (c *scriptedClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	out := make(chan api.StreamEvent, 2)
	out <- api.StreamEvent{Delta: api.Delta{Type: "text_delta", Text: c.body}}
	close(out)
	return out, nil
}

func TestLLMClassifier_FallbackAllowsBeforeLLM(t *testing.T) {
	called := false
	client := &scriptedClient{body: `{"decision":"deny"}`}
	lc := &LLMClassifier{
		Client: stubClient{inner: client, calledBool: &called},
		Model:  "anthropic/claude-haiku-4-5",
		Fallback: ClassifierFunc(func(_ context.Context, _ string, _ map[string]any) (Decision, error) {
			return DecisionAllow, nil
		}),
	}
	dec, err := lc.Classify(context.Background(), "read_file", map[string]any{"path": "x"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("expected fallback Allow short-circuit, got %v", dec)
	}
	if called {
		t.Errorf("LLM should NOT be invoked when fallback decides")
	}
}

func TestLLMClassifier_AskFallthroughHitsLLM(t *testing.T) {
	called := false
	client := &scriptedClient{body: `{"decision":"deny","reason":"shell"}`}
	lc := &LLMClassifier{
		Client: stubClient{inner: client, calledBool: &called},
		Model:  "anthropic/claude-haiku-4-5",
		Fallback: ClassifierFunc(func(_ context.Context, _ string, _ map[string]any) (Decision, error) {
			return DecisionAsk, nil
		}),
	}
	dec, _ := lc.Classify(context.Background(), "bash", map[string]any{"command": "rm -rf /tmp/x"})
	if dec != DecisionDeny {
		t.Errorf("expected LLM Deny, got %v", dec)
	}
	if !called {
		t.Errorf("LLM should be invoked when fallback returns Ask")
	}
}

func TestLLMClassifier_CacheHitAvoidsLLM(t *testing.T) {
	hitCount := 0
	client := &scriptedClient{body: `{"decision":"allow"}`}
	lc := &LLMClassifier{
		Client: stubClient{inner: client, calledInt: &hitCount},
		Model:  "x",
		Cache:  NewClassifierCache(time.Hour),
	}
	args := map[string]any{"path": "/tmp/foo"}
	if _, _ = lc.Classify(context.Background(), "read_file", args); hitCount != 1 {
		t.Fatalf("expected 1 LLM hit on first call, got %d", hitCount)
	}
	if _, _ = lc.Classify(context.Background(), "read_file", args); hitCount != 1 {
		t.Errorf("expected cache hit on second call, got %d total LLM hits", hitCount)
	}
}

func TestLLMClassifier_NoModel_DefaultsToAsk(t *testing.T) {
	lc := &LLMClassifier{}
	dec, _ := lc.Classify(context.Background(), "bash", map[string]any{"command": "ls"})
	if dec != DecisionAsk {
		t.Errorf("expected Ask when no client/model, got %v", dec)
	}
}

func TestLLMClassifier_MalformedResponseFallsToAsk(t *testing.T) {
	lc := &LLMClassifier{
		Client: &scriptedClient{body: "not json"},
		Model:  "x",
	}
	dec, _ := lc.Classify(context.Background(), "bash", map[string]any{"command": "ls"})
	if dec != DecisionAsk {
		t.Errorf("expected Ask for malformed response, got %v", dec)
	}
}

func TestParseDecisionJSON_HandlesCodeFence(t *testing.T) {
	cases := []struct {
		raw  string
		want Decision
	}{
		{`{"decision":"allow"}`, DecisionAllow},
		{"```json\n{\"decision\":\"deny\"}\n```", DecisionDeny},
		{"```\n{\"decision\":\"ask\"}\n```", DecisionAsk},
		{"junk", DecisionAsk},
		{`{"decision":"weird"}`, DecisionAsk},
	}
	for _, tc := range cases {
		if got := parseDecisionJSON(tc.raw); got != tc.want {
			t.Errorf("parse %q → %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestClassifierCache_ExpiresEntries(t *testing.T) {
	c := NewClassifierCache(50 * time.Millisecond)
	c.Set("k", DecisionAllow)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("expected fresh hit")
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Errorf("expected expired entry to miss")
	}
}

func TestClassifierCache_EvictsOldestWhenFull(t *testing.T) {
	c := NewClassifierCacheWithSize(0, 3)
	c.Set("a", DecisionAllow)
	c.Set("b", DecisionDeny)
	c.Set("c", DecisionAsk)
	// Adding a 4th evicts "a" (FIFO).
	c.Set("d", DecisionAllow)
	if _, ok := c.Get("a"); ok {
		t.Errorf("expected oldest entry %q to be evicted", "a")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("expected %q to remain in cache", k)
		}
	}
}

func TestClassifierCache_UpdateInPlaceDoesNotEvict(t *testing.T) {
	c := NewClassifierCacheWithSize(0, 2)
	c.Set("a", DecisionAllow)
	c.Set("b", DecisionDeny)
	// Re-setting "a" must not evict "b".
	c.Set("a", DecisionAsk)
	if _, ok := c.Get("b"); !ok {
		t.Errorf("update-in-place must not evict existing entries")
	}
	if d, _ := c.Get("a"); d != DecisionAsk {
		t.Errorf("expected updated value DecisionAsk for %q, got %v", "a", d)
	}
}

func TestNewLLMClassifier_Defaults(t *testing.T) {
	client := &scriptedClient{body: `{"decision":"allow"}`}
	lc := NewLLMClassifier(client, "")
	if lc.Model != DefaultLLMClassifierModel {
		t.Errorf("expected default model %q, got %q", DefaultLLMClassifierModel, lc.Model)
	}
	if lc.Fallback == nil {
		t.Error("expected default Fallback (RuleClassifier) to be installed")
	}
	if _, ok := lc.Fallback.(*RuleClassifier); !ok {
		t.Errorf("expected *RuleClassifier fallback, got %T", lc.Fallback)
	}
	if lc.Cache == nil {
		t.Error("expected default Cache to be installed")
	}
	if lc.MaxTokens == 0 {
		t.Error("expected non-zero default MaxTokens")
	}
}

func TestNewLLMClassifier_RuleFallbackShortCircuitsOnSafeRead(t *testing.T) {
	called := false
	client := stubClient{
		inner:      &scriptedClient{body: `{"decision":"deny"}`},
		calledBool: &called,
	}
	lc := NewLLMClassifier(client, "")
	dec, err := lc.Classify(context.Background(), "read_file", map[string]any{"path": "/tmp/x"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if dec != DecisionAllow {
		t.Errorf("expected RuleClassifier Allow on read_file, got %v", dec)
	}
	if called {
		t.Error("LLM should not be called when rule fast-path decides")
	}
}

func TestNewLLMClassifier_DenyOnDestructiveBash(t *testing.T) {
	client := &scriptedClient{body: `{"decision":"deny","reason":"destructive"}`}
	lc := NewLLMClassifier(client, "anthropic/claude-haiku-4-5")
	dec, _ := lc.Classify(context.Background(), "bash", map[string]any{"command": "rm -rf /"})
	if dec != DecisionDeny {
		t.Errorf("expected Deny for destructive command, got %v", dec)
	}
}

func TestNewLLMClassifier_PromptOnAmbiguousEdit(t *testing.T) {
	client := &scriptedClient{body: `{"decision":"ask"}`}
	lc := NewLLMClassifier(client, "anthropic/claude-haiku-4-5")
	dec, _ := lc.Classify(context.Background(), "write_file", map[string]any{
		"path":    "/tmp/foo",
		"content": "data",
	})
	if dec != DecisionAsk {
		t.Errorf("expected Ask for ambiguous write_file, got %v", dec)
	}
}

func TestNewLLMClassifier_FallsBackToAskOnError(t *testing.T) {
	client := errClient{}
	lc := NewLLMClassifier(client, "anthropic/claude-haiku-4-5",
		WithFallbackClassifier(nil),
		WithLogger(io.Discard),
	)
	dec, err := lc.Classify(context.Background(), "bash", map[string]any{"command": "ls"})
	if err != nil {
		t.Fatalf("Classify must never propagate errors: %v", err)
	}
	if dec != DecisionAsk {
		t.Errorf("expected fail-safe DecisionAsk on client error, got %v", dec)
	}
}

func TestNewLLMClassifier_OptionsApply(t *testing.T) {
	cache := NewClassifierCache(0)
	lc := NewLLMClassifier(&scriptedClient{body: `{"decision":"allow"}`}, "x",
		WithFallbackClassifier(nil),
		WithClassifierCache(cache),
		WithMaxTokens(123),
		WithLogger(io.Discard),
	)
	if lc.Fallback != nil {
		t.Error("WithFallbackClassifier(nil) should clear default fallback")
	}
	if lc.Cache != cache {
		t.Error("WithClassifierCache must install the provided cache")
	}
	if lc.MaxTokens != 123 {
		t.Errorf("WithMaxTokens(123) must apply, got %d", lc.MaxTokens)
	}
	if lc.Logger == nil {
		t.Error("WithLogger should install the provided writer")
	}
}

func TestNewLLMClassifierManager_WiresClassifier(t *testing.T) {
	hits := 0
	client := stubClient{
		inner:     &scriptedClient{body: `{"decision":"deny","reason":"shell"}`},
		calledInt: &hits,
	}
	// ModeBypassPermissions normally short-circuits to Allow in the legacy
	// path; here we use ModeDefault so the classifier is consulted.
	m := NewLLMClassifierManager(ModeDefault, nil, client, "anthropic/claude-haiku-4-5",
		WithFallbackClassifier(nil),
	)
	dec := m.CheckCtx(context.Background(), "bash", `{"command":"rm -rf /tmp/x"}`)
	if dec != DecisionDeny {
		t.Errorf("expected Manager to surface classifier Deny, got %v", dec)
	}
	if hits != 1 {
		t.Errorf("expected exactly 1 LLM call, got %d", hits)
	}
}

// ----- helpers -----

// ClassifierFunc adapts a function into a Classifier (test-only).
type ClassifierFunc func(ctx context.Context, toolName string, args map[string]any) (Decision, error)

func (f ClassifierFunc) Classify(ctx context.Context, toolName string, args map[string]any) (Decision, error) {
	return f(ctx, toolName, args)
}

// stubClient counts how many times StreamResponse is called.
type stubClient struct {
	inner      *scriptedClient
	calledBool *bool
	calledInt  *int
}

func (s stubClient) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	if s.calledBool != nil {
		*s.calledBool = true
	}
	if s.calledInt != nil {
		*s.calledInt++
	}
	return s.inner.StreamResponse(ctx, req)
}

// errClient simulates a transport-level failure so we can assert the
// classifier's fail-safe path returns DecisionAsk.
type errClient struct{}

func (errClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	return nil, errors.New("synthetic transport failure")
}
