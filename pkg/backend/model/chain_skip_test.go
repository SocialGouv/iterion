package model

import (
	"context"
	"errors"
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
		name:   delegate.BackendClaudeCode,
		fail:   &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Detail: "weekly cap", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)},
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
	// The outcome must name the route that EXECUTED and spent — the
	// runner's cost accumulator keys its claw double-count exclusion on
	// that name, so an empty BackendName (→ the node's REQUESTED backend
	// at the event layer) can erase a metered route's spend from the org
	// cap and the credpool donor ledger.
	if out.BackendName != delegate.BackendClaudeCode {
		t.Errorf("skip outcome BackendName = %q, want the executed route's (%q)", out.BackendName, delegate.BackendClaudeCode)
	}
}

// TestChainSkipNamesTheSpendingRoute: primary on one backend, a second
// route on ANOTHER backend burns and fails, then skip — the outcome must
// name the LAST executed route (the spend's origin), not the first.
func TestChainSkipNamesTheSpendingRoute(t *testing.T) {
	winErr := &delegate.ErrRateLimited{Provider: "x", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)}
	head := &backendScriptedBackend{name: delegate.BackendClaw, fail: winErr, tokens: 10}
	second := &backendScriptedBackend{name: delegate.BackendPi, fail: winErr, tokens: 4242, costUSD: 3.5}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, head)
	reg.Register(delegate.BackendPi, second)
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("x", delegate.BackendClaw, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "x", Model: "m"}, nil
		})
	chain := []chainElement{
		{Label: "primary"},
		{Label: "metered", Backend: delegate.BackendPi, Model: "m2", On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
		{Label: "give_up", Skip: true, On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
	}
	out, err := e.dispatchChain(context.Background(), "x", chain, "m", build)
	if err != nil || !out.Skipped {
		t.Fatalf("expected skip outcome, got err=%v out=%+v", err, out)
	}
	if out.BackendName != delegate.BackendPi {
		t.Errorf("skip outcome BackendName = %q, want %q (the metered route that spent $3.50 — a claw label would erase it from RunTotals)", out.BackendName, delegate.BackendPi)
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

// TestChainSkipRefusesUnclassifiedFailure: an indescribable failure (a
// bare error — CLI exit 1, provider 400, flattened sandbox error) may
// try another BACKEND, but it must never be CONVERTED INTO A SUCCESS by
// a filtered skip route. Only an explicit `on: [any]` opts into that.
func TestChainSkipRefusesUnclassifiedFailure(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: errors.New("claude_code: exit status 1: prompt too long"),
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
		t.Fatal("unclassified failure swallowed by a skip route scoped to usage_window — the node must fail, not fake success")
	}

	// The explicit escape hatch: on: [any] (resolved to an empty filter)
	// does convert it — the author said so out loud.
	out, err := e.dispatchChain(context.Background(), "plan_review",
		skipChain(nil), "claude-opus-5", build)
	if err != nil || !out.Skipped {
		t.Fatalf("on:[any] skip should serve the unclassified failure, got err=%v out=%+v", err, out)
	}
}

// TestChainSkipNotReachableViaBuildError: a BUILD failure on the
// preceding route (unresolvable backend, uncredentialed element) must
// not fall into a filtered skip either — the walk consults the same
// on: acceptance as the execute-failure path.
func TestChainSkipNotReachableViaBuildError(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("x", delegate.BackendClaudeCode, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "x", Model: "claude-opus-5"}, nil
		})
	chain := []chainElement{
		{Label: "primary"},
		// rescue names a backend the registry does not have → build error.
		{Label: "rescue", Backend: "codex", Model: "gpt-5.5", On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
		// give_up accepts auth ONLY — neither the usage_window of the
		// primary nor the rescue build error may land here.
		{Label: "give_up", Skip: true, On: []delegate.FallbackCategory{delegate.FallbackAuth}},
	}
	_, err := e.dispatchChain(context.Background(), "x", chain, "claude-opus-5", build)
	if err == nil {
		t.Fatal("a build error on the previous route fell into a skip scoped to auth — the node must fail")
	}
}

// TestChainSkipBuildErrorKeepsOriginalCategory: a usage_window outage
// followed by an UNBUILDABLE rescue route must still reach a
// usage_window-filtered skip — the build error routes on the last
// EXECUTE failure's category, not on Unclassified (which would disarm
// the operator's skip policy and turn it into wait).
func TestChainSkipBuildErrorKeepsOriginalCategory(t *testing.T) {
	head := &backendScriptedBackend{
		name: delegate.BackendClaudeCode,
		fail: &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(time.Hour)},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("x", delegate.BackendClaudeCode, head,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "x", Model: "claude-opus-5"}, nil
		})
	chain := []chainElement{
		{Label: "primary"},
		{Label: "rescue", Backend: "codex", Model: "gpt-5.5"}, // unregistered → build error
		{Label: "give_up", Skip: true, On: []delegate.FallbackCategory{delegate.FallbackUsageWindow}},
	}
	out, err := e.dispatchChain(context.Background(), "x", chain, "claude-opus-5", build)
	if err != nil || !out.Skipped {
		t.Fatalf("usage_window + unbuildable rescue must still reach the usage_window skip: err=%v out=%+v", err, out)
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
