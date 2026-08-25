package model

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Tests for the `action: skip` terminal route (the "continue and ignore"
// half of a peer-node unavailability policy) and the `when:` route gate.

func skipChain(onSkip []delegate.FallbackCategory) []chainElement {
	return []chainElement{
		{Label: "primary"},
		{Label: "give_up", Skip: true, On: onSkip},
	}
}

// TestChainSkipServesSkippedOutcome: the primary dies on a usage window,
// the skip route accepts it, and the chain completes WITHOUT error — a
// Skipped outcome carrying the failed route's spend, never content.
func TestChainSkipServesSkippedOutcome(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Detail: "weekly cap", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)},
		tokens: 500, costUSD: 0.20,
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("plan_review", delegate.BackendClaudeCode, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "plan_review", Model: "claude-opus-5"}, nil
		})
	out, err := e.dispatchChain(context.Background(), "plan_review",
		skipChain([]delegate.FallbackCategory{delegate.FallbackUsageWindow}), "claude-opus-5", build)
	if err != nil {
		t.Fatalf("expected the skip route to complete the node, got %v", err)
	}
	if !out.Skipped || !out.FellThrough {
		t.Fatalf("expected a Skipped fall-through outcome, got %+v", out)
	}
	if out.ServedBy != "give_up" {
		t.Errorf("ServedBy = %q, want the skip route's name", out.ServedBy)
	}
	// The failed route's spend must survive onto the outcome: a skipped
	// node still burned the primary's attempts.
	if out.Result.Tokens == 0 {
		t.Errorf("failed primary's spend lost on the skip outcome: %+v", out.Result)
	}
}

// TestChainSkipRespectsOnFilter: a skip route scoped to usage_window must
// NOT swallow an unrelated failure — the node still fails (resumable),
// which is the "pause and retry" policy.
func TestChainSkipRespectsOnFilter(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "revoked"},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})

	build := e.newElementBuilder("plan_review", delegate.BackendClaudeCode, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "plan_review", Model: "claude-opus-5"}, nil
		})
	_, err := e.dispatchChain(context.Background(), "plan_review",
		skipChain([]delegate.FallbackCategory{delegate.FallbackUsageWindow}), "claude-opus-5", build)
	if err == nil {
		t.Fatal("expected the auth failure to surface — the skip route's on: filter must not swallow it")
	}
}

// TestResolveChain_SkipRouteAndWhenGate: the when: gate picks the route
// set per run from vars, and a skip route survives dedupe despite its
// empty backend/model.
func TestResolveChain_SkipRouteAndWhenGate(t *testing.T) {
	fbs := []ir.Fallback{
		{Name: "give_up", Action: ir.FallbackActionSkip, When: "vars.policy == 'skip'", On: []string{"usage_window"}},
	}

	e := &ClawExecutor{vars: map[string]any{"policy": "skip"}}
	chain := e.resolveChain(chainAgentNode("x", delegate.BackendClaudeCode, "", fbs))
	if len(chain) != 2 {
		t.Fatalf("chain length %d, want 2 (primary + active skip): %+v", len(chain), chain)
	}
	if !chain[1].Skip || chain[1].Label != "give_up" {
		t.Errorf("skip route not resolved: %+v", chain[1])
	}

	e = &ClawExecutor{vars: map[string]any{"policy": "wait"}}
	chain = e.resolveChain(chainAgentNode("x", delegate.BackendClaudeCode, "", fbs))
	if len(chain) != 1 {
		t.Fatalf("when: gate false must deactivate the route, got %+v", chain)
	}
}

// TestFillZeroValues: the synthesized skip output covers every schema
// field with its zero value and leaves already-present keys alone.
func TestFillZeroValues(t *testing.T) {
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{
		{Name: "concerns", Type: ir.FieldTypeString},
		{Name: "blocking", Type: ir.FieldTypeBool},
		{Name: "count", Type: ir.FieldTypeInt},
		{Name: "score", Type: ir.FieldTypeFloat},
		{Name: "items", Type: ir.FieldTypeStringArray},
	}}
	out := map[string]any{"concerns": "kept"}
	fillZeroValues(out, schema)
	if out["concerns"] != "kept" {
		t.Errorf("existing key overwritten: %+v", out)
	}
	if out["blocking"] != false || out["count"] != 0 || out["score"] != 0.0 {
		t.Errorf("zero values wrong: %+v", out)
	}
	if arr, ok := out["items"].([]any); !ok || len(arr) != 0 {
		t.Errorf("string array zero value wrong: %+v", out["items"])
	}
}
