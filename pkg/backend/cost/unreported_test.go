package cost

import (
	"encoding/json"
	"math"
	"testing"
)

// The unreported-call count reads back after a JSON round trip — a sandbox
// relay or a checkpoint turns its int into a float64 — and an absent or
// non-positive count reads as every call reported. A zero write removes the
// key, whoever put it there.
func TestUnreportedCalls_SurvivesAJSONRoundTrip(t *testing.T) {
	out := map[string]any{}
	SetUnreportedCalls(out, 3)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := UnreportedCalls(back); got != 3 {
		t.Errorf("after a JSON round trip: %d unreported calls, want 3", got)
	}
	SetUnreportedCalls(back, 0)
	if _, kept := back[UnreportedCallsKey]; kept {
		t.Errorf("a zero write left %v, want the key removed", back)
	}
	if got := UnreportedCalls(map[string]any{}); got != 0 {
		t.Errorf("absent key reads %d, want 0", got)
	}
	if got := UnreportedCalls(map[string]any{UnreportedCallsKey: -2}); got != 0 {
		t.Errorf("a negative count reads %d, want 0", got)
	}
}

// A count no run could reach — a model's own JSON naming the key — is
// clamped, never wrapped into a negative int; what is not a count reads 0.
func TestUnreportedCalls_ClampsWhatNoRunCouldReach(t *testing.T) {
	for _, v := range []any{1e300, int64(1) << 62, 1 << 62} {
		if got := UnreportedCalls(map[string]any{UnreportedCallsKey: v}); got != maxUnreportedCalls {
			t.Errorf("%v reads %d, want the %d clamp", v, got, maxUnreportedCalls)
		}
	}
	for _, v := range []any{math.NaN(), 0.5, -1e300, "3"} {
		if got := UnreportedCalls(map[string]any{UnreportedCallsKey: v}); got != 0 {
			t.Errorf("%v reads %d, want 0", v, got)
		}
	}
}
