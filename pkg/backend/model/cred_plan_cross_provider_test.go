package model

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fold's plan join (#2288): a fallback chain books the failed routes'
// spend on the ONE delegate event of the element that ends it, and the
// runner's credential plan keys one fingerprint per (node, provider). When
// the folded routes spent under another provider than the event names, the
// figure cannot be placed on one row — the event must say so, or the runner
// books a lending donor's slice on a credential that never served it.

// delegateEventLog records the events the production store hooks append.
type delegateEventLog struct {
	mu     sync.Mutex
	events []store.Event
}

func (l *delegateEventLog) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, evt)
	return &evt, nil
}

// finished returns the single delegate_finished event the dispatch emitted.
func (l *delegateEventLog) finished(t *testing.T) store.Event {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var got []store.Event
	for _, evt := range l.events {
		if evt.Type == store.EventDelegateFinished {
			got = append(got, evt)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d delegate_finished events, want exactly one: %+v", len(got), l.events)
	}
	return got[0]
}

// dispatchThroughStoreHooks runs one node dispatch over chain through the
// production store hooks, the head element on headBackend with headModel,
// and returns the backends' recorded calls and the emitted events.
func dispatchThroughStoreHooks(t *testing.T, headBackend, headModel string, chain []chainElement, backends ...*backendScriptedBackend) *delegateEventLog {
	t.Helper()
	log := &delegateEventLog{}
	hooks := NewStoreEventHooks(context.Background(), log, "run", iterlog.Nop(), nil, nil)
	reg := delegate.NewRegistry()
	for _, b := range backends {
		reg.Register(b.name, b)
	}
	e := newFallbackExecutor(reg, hooks)
	// Two attempts per element on a transient failure: the probe's "$5 ×2".
	e.retry = RetryPolicy{MaxAttempts: 2, MaxAttemptsTransient: 2, BackoffBase: time.Millisecond}
	build := e.newElementBuilder("impl", headBackend, nil,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "impl", Model: headModel}, nil
		})
	if _, err := e.dispatchWithObservability(context.Background(), "impl", headBackend, "model: node", chain, headModel, build, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return log
}

// TestDispatch_aFoldAcrossProvidersSaysSoOnItsEvent: the review's probe. A
// hint-less codex primary burns $5 twice on openai and fails; the chain
// falls through to a claw route on anthropic that serves for $0.10. ONE
// delegate_finished carries the $10.10 under the serving word "anthropic" —
// a runner joining it to the plan books all of it on the anthropic row and
// the openai donor's slice nowhere. The event must carry the crossing.
//
// Controls (an over-eager flag voids the fine table on every fallback): a
// fold whose routes all serve one provider, and a chain whose primary spent
// nothing, carry no crossing.
func TestDispatch_aFoldAcrossProvidersSaysSoOnItsEvent(t *testing.T) {
	t.Parallel()
	transient := &delegate.ErrTransient{Reason: "overloaded"}

	t.Run("codex on openai then claw on anthropic", func(t *testing.T) {
		t.Parallel()
		head := &backendScriptedBackend{name: delegate.BackendCodex, fail: transient, tokens: 500, costUSD: 5}
		tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 10, costUSD: 0.10}
		log := dispatchThroughStoreHooks(t, delegate.BackendCodex, "openai/gpt-5", []chainElement{
			{Label: "primary"},
			{Label: "door", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5"},
		}, head, tail)
		if len(head.tasks) != 2 {
			t.Fatalf("codex ran %d attempts, want the probe's two", len(head.tasks))
		}
		evt := log.finished(t)
		if got, _ := evt.Data["provider"].(string); got != "anthropic" {
			t.Fatalf("provider = %q, want the serving claw route's anthropic", got)
		}
		if got, _ := evt.Data["cost_usd"].(float64); math.Abs(got-10.10) > 1e-9 {
			t.Fatalf("cost_usd = %v, want the folded 10.10", got)
		}
		if crosses, _ := evt.Data["spend_crosses_providers"].(bool); !crosses {
			t.Fatalf("the folded $10.10 spans openai and anthropic, but the event says nothing (data %v) — the runner books the openai slice on the anthropic credential", evt.Data)
		}
	})

	t.Run("codex then claw, both on openai", func(t *testing.T) {
		t.Parallel()
		head := &backendScriptedBackend{name: delegate.BackendCodex, fail: transient, tokens: 500, costUSD: 5}
		tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 10, costUSD: 0.10}
		log := dispatchThroughStoreHooks(t, delegate.BackendCodex, "openai/gpt-5", []chainElement{
			{Label: "primary"},
			{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
		}, head, tail)
		if crosses, ok := log.finished(t).Data["spend_crosses_providers"]; ok {
			t.Fatalf("spend_crosses_providers = %v on a fold whose routes all serve openai — one plan row places it whole", crosses)
		}
	})

	t.Run("the primary spent nothing", func(t *testing.T) {
		t.Parallel()
		head := &backendScriptedBackend{name: delegate.BackendCodex, fail: transient}
		tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 10, costUSD: 0.10}
		log := dispatchThroughStoreHooks(t, delegate.BackendCodex, "openai/gpt-5", []chainElement{
			{Label: "primary"},
			{Label: "door", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5"},
		}, head, tail)
		if crosses, ok := log.finished(t).Data["spend_crosses_providers"]; ok {
			t.Fatalf("spend_crosses_providers = %v though only the serving route spent — its own row places it", crosses)
		}
	})
}

// TestServingProvider_codexServesOnlyOpenAI: the codex CLI reads no hint and
// spends the run's openai key or its codex forfait, so its element names
// openai whatever the route says — a hint-less codex route included, whose
// bare model names no prefix the runner could derive a provider from.
func TestServingProvider_codexServesOnlyOpenAI(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ hint, model string }{
		{"", "gpt-5"},
		{"", "openai/gpt-5"},
		{"openai", "gpt-5"},
		{"zai", "gpt-5"},
	} {
		if got := servingProvider(delegate.BackendCodex, &delegate.Task{ProviderHint: row.hint, Model: row.model}); got != "openai" {
			t.Errorf("servingProvider(codex, hint=%q, model=%q) = %q, want openai", row.hint, row.model, got)
		}
	}
}

// TestDispatch_aHintLessCodexEventNamesOpenAI: the word reaches the event a
// hint-less, bare-model codex node finishes with — the one the runner's
// credential-plan join reads first.
func TestDispatch_aHintLessCodexEventNamesOpenAI(t *testing.T) {
	t.Parallel()
	codex := &backendScriptedBackend{name: delegate.BackendCodex, tokens: 10, costUSD: 0.10}
	log := dispatchThroughStoreHooks(t, delegate.BackendCodex, "gpt-5", []chainElement{{Label: "primary"}}, codex)
	if got, _ := log.finished(t).Data["provider"].(string); got != "openai" {
		t.Fatalf("a hint-less codex node finished with provider %q, want openai — the plan's openai row is the only one it can join", got)
	}
}
