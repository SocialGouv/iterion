package credpool

import (
	"testing"
	"time"
)

// A pledge's bot audience is matched EXACTLY, against the launch bot id as the
// launch path carries it. Both edges of this credential agree on that: the
// route stores what the donor sent, and servesBot compares it verbatim.
//
// The regression this pins was shipped and caught at the merge gate: the write
// route was made to canonicalise (`my_bot` -> `my-bot`) while this comparison
// stayed raw, so a pledge naming a non-canonical stored slug silently stopped
// serving the bot it named and the bill moved to another tier. Worse, the CLI
// re-sends the stored list on every pledge PUT, so toggling `enabled` rewrote a
// working row into a non-matching one.
//
// If a future change folds one edge, this reddens. Folding BOTH is legitimate —
// it is what #1368 carries — and then this test is the one to update, together
// with the route, in a single change.
func TestAPledgeServesTheBotIDExactlyAsLaunched(t *testing.T) {
	now := time.Now().UTC()
	pledge := func(bots ...string) Pledge {
		return Pledge{Enabled: true, Health: HealthOK, Bots: bots}
	}

	cases := []struct {
		name  string
		bots  []string
		botID string
		want  bool
	}{
		{"a non-canonical stored slug serves itself", []string{"my_bot"}, "my_bot", true},
		{"and is not served by its folded spelling", []string{"my_bot"}, "my-bot", false},
		{"a canonical slug serves itself", []string{"app-dev"}, "app-dev", true},
		{"and is not served by an underscore variant", []string{"app-dev"}, "app_dev", false},
		{"case is not folded either", []string{"app-dev"}, "App-Dev", false},
		{"an unrelated bot is refused", []string{"app-dev"}, "review-pr", false},
		{"an empty list serves every bot", nil, "anything", true},
	}
	for _, tc := range cases {
		ok, status := pledge(tc.bots...).AvailableForLaunch(now, tc.botID)
		if ok != tc.want {
			t.Errorf("%s: AvailableForLaunch(%q) = %v (%s), want %v", tc.name, tc.botID, ok, status, tc.want)
		}
		if !ok && tc.want == false && len(tc.bots) > 0 && status != StatusBotFiltered {
			t.Errorf("%s: refused with status %q, want %q so the donor sees why", tc.name, status, StatusBotFiltered)
		}
	}
}

// The launch path is the one input a donor does not control, so an empty bot id
// must not lift a non-empty allow-list — the same fail-closed rule the BYOK
// audience states, and the reason the two were written to mirror each other.
func TestAPledgeWithAnAudienceRefusesALaunchNamingNoBot(t *testing.T) {
	now := time.Now().UTC()
	p := Pledge{Enabled: true, Health: HealthOK, Bots: []string{"app-dev"}}

	if ok, status := p.AvailableForLaunch(now, ""); ok {
		t.Fatalf("a launch naming no bot was served by a scoped pledge (status %q)", status)
	}
	// The donor-facing status view asks the other question, and must keep
	// answering it: "is my contribution live at all?"
	if ok, status := p.Available(now, ""); !ok {
		t.Fatalf("the donor status view reports a live pledge as unavailable (status %q)", status)
	}
}
