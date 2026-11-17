package server

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// The launch request's bounds raise (#3345): durations parse with the same
// rule the budget's max_duration teaches — an unparsable value is a named
// 400, never a silent inherit — and an absent object converts to nil, the
// "the launch did not choose" the resolver's field-by-field rule reads.
func TestLaunchCodemodeBoundsParseOrNameTheField(t *testing.T) {
	t.Parallel()
	var absent *runview.CodemodeBoundsRequest
	got, err := absent.ToBounds()
	if got != nil || err != nil {
		t.Errorf("an absent bounds object converted to %+v / %v, want nil / nil", got, err)
	}

	got, err = (&runview.CodemodeBoundsRequest{ToolCalls: 256, Wall: "90s", CPU: "2m"}).ToBounds()
	if err != nil {
		t.Fatalf("well-formed bounds refused: %v", err)
	}
	if got.ToolCalls != 256 || got.Wall != 90*time.Second || got.CPU != 2*time.Minute {
		t.Errorf("bounds = %+v, want ToolCalls 256 / Wall 90s / CPU 2m", got)
	}
	if got.Memory != 0 || got.ScriptBytes != 0 {
		t.Errorf("bounds = %+v, want the unset fields to stay zero", got)
	}

	for name, bad := range map[string]runview.CodemodeBoundsRequest{
		"wall": {Wall: "four minutes"},
		"cpu":  {CPU: "quick"},
	} {
		if _, err := bad.ToBounds(); err == nil {
			t.Errorf("%s: an unparsable duration was accepted — the run would read it as nothing", name)
		}
	}

	// A NEGATIVE bound is a typo, not a choice: the resolver's pick rule
	// would silently read it as unset — the inherit-without-saying the
	// launch surface refuses everywhere else. Found by the adversarial
	// round; the refusal names the field.
	for name, bad := range map[string]runview.CodemodeBoundsRequest{
		"tool_calls":        {ToolCalls: -5},
		"memory":            {Memory: -1},
		"wall":              {Wall: "-5s"},
		"cpu":               {CPU: "-2m"},
		"tool_result_bytes": {ToolResultBytes: -8},
	} {
		_, err := bad.ToBounds()
		if err == nil {
			t.Errorf("%s: a negative bound was accepted — it would silently invert into an inherit", name)
			continue
		}
		if !strings.Contains(err.Error(), "negative") {
			t.Errorf("%s: the refusal %v does not name the defect", name, err)
		}
	}
}
