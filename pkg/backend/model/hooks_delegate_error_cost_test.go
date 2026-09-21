package model

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A delegate_error carries the spend its delegation added, under the same
// rule as delegate_finished (omitted when no price source knew the model).
// emitReaskOutcome computes exactly this figure for a schema re-ask that
// failed; serializing it is what makes the documented event shape true and
// gives a reader closing the org-metering gap something to read. Mutating
// the serialization away reddens the readback.
func TestDelegateErrorCarriesItsCost(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-error-cost"
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.New(iterlog.LevelError, nil), nil)
	hooks.OnDelegateError("judge", DelegateInfo{
		BackendName: "claude_code",
		Tokens:      900,
		CostUSD:     0.01,
		Error:       errors.New("structured output invalid; the resume_session re-ask failed"),
	})
	events, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Type != store.EventDelegateError {
			continue
		}
		found = true
		if got := eventCostUSD(ev.Data); got <= 0 {
			t.Fatalf("delegate_error carries no cost_usd: %+v", ev.Data)
		}
	}
	if !found {
		t.Fatal("no delegate_error event in the store")
	}
	// A delegation whose price sources knew nothing stays key-less: "unknown"
	// and a measured $0 stay distinguishable, the delegate_finished rule.
	const runID2 = "run-error-no-cost"
	if _, err := st.CreateRun(ctx, runID2, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks2 := NewStoreEventHooks(ctx, st, runID2, iterlog.New(iterlog.LevelError, nil), nil)
	hooks2.OnDelegateError("judge", DelegateInfo{BackendName: "claude_code", Error: errors.New("boom")})
	events2, err := st.LoadEvents(ctx, runID2)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events2 {
		if ev.Type != store.EventDelegateError {
			continue
		}
		if _, ok := ev.Data["cost_usd"]; ok {
			t.Fatalf("a zero-cost error must omit the key: %+v", ev.Data)
		}
	}
}

func eventCostUSD(data map[string]any) float64 {
	switch n := data["cost_usd"].(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}
