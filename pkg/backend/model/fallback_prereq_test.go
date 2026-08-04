package model

import (
	"bytes"
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// usageWindowErr builds the typed error a subscription forfait raises
// when its 5h / weekly window shuts.
func usageWindowErr() error {
	return &delegate.ErrRateLimited{
		Provider: delegate.BackendClaudeCode,
		Kind:     delegate.RateLimitKindUsageWindow,
		Detail:   "You've hit your usage limit · resets 3pm",
	}
}

// countCalls returns how many Execute calls a scripted backend received
// for one provider hint.
func countCalls(b *providerScriptedBackend, hint string) int {
	n := 0
	for _, c := range b.calls {
		if c == hint {
			n++
		}
	}
	return n
}

// TestUsageWindow_SkipsRetryBudgetWhenFallbackRemains is the wall-clock
// half of ADR-087: retrying inside a shut subscription window cannot
// succeed by construction, so when the node still has somewhere to go
// the in-place budget is skipped entirely.
//
// This matters beyond latency. The whole chain runs under ONE per-node
// timeout context, so a chain that pays a full backed-off budget per
// element can hit the node deadline mid-walk and surface
// context.DeadlineExceeded — destroying the typed *ErrRateLimited that
// the run-level usage-window retry and the credential-pool donor
// cooldown both key on.
func TestUsageWindow_SkipsRetryBudgetWhenFallbackRemains(t *testing.T) {
	rec := &fallbackRecorder{}
	e := newFallbackExecutor(nil, rec.hook())
	fake := &providerScriptedBackend{fail: map[string]error{"zai": usageWindowErr()}}

	task := delegate.Task{NodeID: "n"}
	out, err := e.dispatchWithProviderFallback(context.Background(), "n",
		delegate.BackendClaudeCode,
		[]providerStep{{Provider: "zai"}, {Provider: "anthropic"}}, fake, &task)
	if err != nil {
		t.Fatalf("chain should have succeeded on anthropic: %v", err)
	}
	if got := countCalls(fake, "zai"); got != 1 {
		t.Errorf("zai attempted %d times, want exactly 1 — a shut window must not burn the retry budget when a fallback remains (calls=%v)", got, fake.calls)
	}
	if out.Output["served_by"] != "anthropic" {
		t.Errorf("served_by=%v, want anthropic", out.Output["served_by"])
	}
	if len(rec.events) != 1 {
		t.Fatalf("expected exactly one fall-through note, got %d", len(rec.events))
	}
	if rec.events[0].Reason != string(delegate.FallbackUsageWindow) {
		t.Errorf("fall-through reason = %q, want %q", rec.events[0].Reason, delegate.FallbackUsageWindow)
	}
}

// TestUsageWindow_KeepsRetryBudgetOnLastElement pins the other half of
// the carve-out: with nowhere better to go, the historical behaviour is
// exactly right — retry, then surface the typed error upward so the
// run-level policy can wait the window out.
func TestUsageWindow_KeepsRetryBudgetOnLastElement(t *testing.T) {
	e := newFallbackExecutor(nil, EventHooks{})
	fake := &providerScriptedBackend{fail: map[string]error{"zai": usageWindowErr()}}

	task := delegate.Task{NodeID: "n"}
	_, err := e.dispatchWithProviderFallback(context.Background(), "n",
		delegate.BackendClaudeCode, []providerStep{{Provider: "zai"}}, fake, &task)
	if err == nil {
		t.Fatal("expected the single-element chain to fail")
	}
	if got := countCalls(fake, "zai"); got != 2 {
		t.Errorf("zai attempted %d times, want the full budget (2) on the last element; calls=%v", got, fake.calls)
	}
	// The run-level usage-window retry and the credential-pool donor
	// cooldown both errors.As on what surfaces here.
	if !delegate.IsUsageWindow(err) {
		t.Errorf("surfaced error lost its usage-window type: %v", err)
	}
}

// TestTransientStillBurnsBudgetWithFallback proves the carve-out is
// narrow: a plain throttle or 5xx is worth re-issuing against the same
// element, so it must still pay its budget before the chain advances.
func TestTransientStillBurnsBudgetWithFallback(t *testing.T) {
	e := newFallbackExecutor(nil, EventHooks{})
	fake := &providerScriptedBackend{fail: map[string]error{
		"zai": &delegate.ErrTransient{Provider: "zai", Reason: "5xx upstream"},
	}}

	task := delegate.Task{NodeID: "n"}
	if _, err := e.dispatchWithProviderFallback(context.Background(), "n",
		delegate.BackendClaudeCode,
		[]providerStep{{Provider: "zai"}, {Provider: "anthropic"}}, fake, &task); err != nil {
		t.Fatalf("chain should have succeeded on anthropic: %v", err)
	}
	if got := countCalls(fake, "zai"); got != 2 {
		t.Errorf("transient failure attempted %d times, want the full budget (2); calls=%v", got, fake.calls)
	}
}

// TestModelFallbackEventReachesStore closes the observability gap
// ADR-087 records: OnProviderFallback has been fired since ADR-004 but
// NewStoreEventHooks never registered it, so no fall-through has ever
// reached events.jsonl. Without this event a run served by a different
// model than the one it asked for is indistinguishable, after the fact,
// from a clean one.
func TestModelFallbackEventReachesStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-fallback"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var logBuf bytes.Buffer
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelInfo, &logBuf), nil)

	if hooks.OnProviderFallback == nil {
		t.Fatal("OnProviderFallback is not wired into the store hooks")
	}
	hooks.OnProviderFallback("implement", ProviderFallbackInfo{
		BackendName: delegate.BackendClaudeCode,
		FromBackend: delegate.BackendClaudeCode,
		ToBackend:   delegate.BackendClaw,
		FromModel:   "claude-opus-5",
		ToModel:     "openai/gpt-5.5",
		From:        "anthropic",
		To:          "openai",
		Reason:      string(delegate.FallbackUsageWindow),
		Attempts:    1,
		Err:         usageWindowErr(),
	})

	evts, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var found *store.Event
	for i := range evts {
		if evts[i].Type == store.EventModelFallback {
			found = evts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no %s event in events.jsonl (%d events)", store.EventModelFallback, len(evts))
	}
	if found.NodeID != "implement" {
		t.Errorf("node_id = %q, want implement", found.NodeID)
	}
	for key, want := range map[string]any{
		"from_backend": delegate.BackendClaudeCode,
		"to_backend":   delegate.BackendClaw,
		"from_model":   "claude-opus-5",
		"to_model":     "openai/gpt-5.5",
		"reason":       string(delegate.FallbackUsageWindow),
	} {
		if got := found.Data[key]; got != want {
			t.Errorf("data[%q] = %v, want %v", key, got, want)
		}
	}
	// The operator must see the route change in run.log too — a
	// fall-through means the primary route is gone even when the run
	// then succeeds.
	if !bytes.Contains(logBuf.Bytes(), []byte("Model fallback")) {
		t.Errorf("fall-through absent from run.log:\n%s", logBuf.String())
	}
}
