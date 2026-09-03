package model

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newUsageHooks builds store-backed event hooks over a real filesystem
// store and returns a loader for the run's persisted usage_progress
// events — the store is the oracle, not the hook's internal state.
func newUsageHooks(t *testing.T, runID string) (EventHooks, func() []store.Event) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(context.Background(), st, runID, iterlog.Nop(), nil)
	return hooks, func() []store.Event {
		all, err := st.LoadEvents(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadEvents: %v", err)
		}
		var out []store.Event
		for _, e := range all {
			if e.Type == store.EventUsageProgress {
				out = append(out, *e)
			}
		}
		return out
	}
}

// The claude_code feed reports call-cumulative usage on a priced model:
// the first sample past the floor emits with an estimated USD "used",
// insignificant growth is debounced, significant growth emits again.
func TestUsageProgress_PricedDebounce(t *testing.T) {
	hooks, load := newUsageHooks(t, "run-up-priced")

	// ~$0.6 on opus input pricing — comfortably past the $0.05 floor.
	hooks.OnUsageProgress("review", UsageProgressInfo{
		Model: "claude-opus-5", InputTokens: 40000, OutputTokens: 500,
	})
	// +5% — under the 20% growth gate, must be debounced.
	hooks.OnUsageProgress("review", UsageProgressInfo{
		Model: "claude-opus-5", InputTokens: 42000, OutputTokens: 500,
	})
	// ~2× — must emit a second sample.
	hooks.OnUsageProgress("review", UsageProgressInfo{
		Model: "claude-opus-5", InputTokens: 80000, OutputTokens: 2000,
	})

	evts := load()
	if len(evts) != 2 {
		t.Fatalf("got %d usage_progress events, want 2 (first + significant growth): %+v", len(evts), evts)
	}
	for i, e := range evts {
		if e.NodeID != "review" {
			t.Errorf("event %d node = %q", i, e.NodeID)
		}
		used, ok := e.Data["used"].(float64)
		if !ok || used <= 0 {
			t.Errorf("event %d: priced sample must carry a positive used, got %v", i, e.Data["used"])
		}
		if e.Data["model"] != "claude-opus-5" {
			t.Errorf("event %d model = %v", i, e.Data["model"])
		}
	}
	u0, _ := evts[0].Data["used"].(float64)
	u1, _ := evts[1].Data["used"].(float64)
	if u1 <= u0 {
		t.Errorf("second sample used=%v must exceed first=%v (cumulative)", u1, u0)
	}
}

// An unknown model cannot be priced: the sample carries tokens only —
// never a "used" of 0, which would read as "free" to a cost_gt monitor
// (zero means unknown, never free).
func TestUsageProgress_UnpricedTokensOnly(t *testing.T) {
	hooks, load := newUsageHooks(t, "run-up-unpriced")

	hooks.OnUsageProgress("review", UsageProgressInfo{
		Model: "totally-unknown-model", InputTokens: 9000, OutputTokens: 1000,
	})
	evts := load()
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evts), evts)
	}
	if _, ok := evts[0].Data["used"]; ok {
		t.Fatalf("unpriced sample must OMIT used, got %v", evts[0].Data["used"])
	}
	if tok, _ := evts[0].Data["tokens"].(float64); int(tok) != 10000 {
		t.Errorf("tokens = %v, want 10000", evts[0].Data["tokens"])
	}
}

// A sample under both floors (cost and tokens) is noise — no event.
func TestUsageProgress_UnderFloorIsSilent(t *testing.T) {
	hooks, load := newUsageHooks(t, "run-up-floor")
	hooks.OnUsageProgress("review", UsageProgressInfo{
		Model: "totally-unknown-model", InputTokens: 100, OutputTokens: 10,
	})
	if evts := load(); len(evts) != 0 {
		t.Fatalf("under-floor sample emitted: %+v", evts)
	}
}

// The claw feed accumulates per-step DELTAS, priced via the model the
// node's llm_request reported (the step payload carries no model), and a
// new loop iteration starts a fresh accumulation.
func TestUsageProgress_ClawStepFeed(t *testing.T) {
	hooks, load := newUsageHooks(t, "run-up-claw")

	hooks.OnLLMRequest("review", LLMRequestInfo{Model: "claude-opus-5"})
	hooks.OnLLMStepFinish("review", LLMStepInfo{Number: 1, InputTokens: 30000, OutputTokens: 400})
	hooks.OnLLMStepFinish("review", LLMStepInfo{Number: 2, InputTokens: 35000, OutputTokens: 600})

	evts := load()
	if len(evts) < 1 {
		t.Fatalf("claw step feed emitted nothing")
	}
	last := evts[len(evts)-1]
	if used, _ := last.Data["used"].(float64); used <= 0 {
		t.Fatalf("claw sample must be priced via the llm_request model, got %v", last.Data["used"])
	}
	if tok, _ := last.Data["tokens"].(float64); int(tok) != 66000 {
		t.Errorf("cumulative tokens = %v, want 66000 (deltas summed)", last.Data["tokens"])
	}

	// New loop iteration: the accumulation resets (the sample reflects
	// only the fresh iteration's spend, not the previous one's).
	hooks.OnLLMStepFinish("review", LLMStepInfo{Number: 1, Iteration: 1, InputTokens: 40000, OutputTokens: 500})
	evts = load()
	last = evts[len(evts)-1]
	if tok, _ := last.Data["tokens"].(float64); int(tok) != 40500 {
		t.Errorf("post-reset tokens = %v, want 40500", last.Data["tokens"])
	}
}
