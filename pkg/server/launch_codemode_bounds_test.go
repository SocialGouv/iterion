package server

import (
	"testing"
	"time"
)

// The launch request's bounds raise (#3345): durations parse with the same
// rule the budget's max_duration teaches — an unparsable value is a named
// 400, never a silent inherit — and an absent object converts to nil, the
// "the launch did not choose" the resolver's field-by-field rule reads.
func TestLaunchCodemodeBoundsParseOrNameTheField(t *testing.T) {
	t.Parallel()
	var absent *launchCodemodeBounds
	got, err := absent.toBounds()
	if got != nil || err != nil {
		t.Errorf("an absent bounds object converted to %+v / %v, want nil / nil", got, err)
	}

	got, err = (&launchCodemodeBounds{ToolCalls: 256, Wall: "90s", CPU: "2m"}).toBounds()
	if err != nil {
		t.Fatalf("well-formed bounds refused: %v", err)
	}
	if got.ToolCalls != 256 || got.Wall != 90*time.Second || got.CPU != 2*time.Minute {
		t.Errorf("bounds = %+v, want ToolCalls 256 / Wall 90s / CPU 2m", got)
	}
	if got.Memory != 0 || got.ScriptBytes != 0 {
		t.Errorf("bounds = %+v, want the unset fields to stay zero", got)
	}

	for name, bad := range map[string]launchCodemodeBounds{
		"wall": {Wall: "four minutes"},
		"cpu":  {CPU: "quick"},
	} {
		if _, err := bad.toBounds(); err == nil {
			t.Errorf("%s: an unparsable duration was accepted — the run would read it as nothing", name)
		}
	}
}
