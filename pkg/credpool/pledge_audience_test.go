package credpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A pledge's bot audience is matched EXACTLY, against the launch bot id as the
// launch path carries it. Both edges of this credential agree on that: the
// route stores what the donor sent, and servesBot compares it verbatim. The
// pair holds because the launch id arrives canonical (or empty) — the
// resolver hands back the tier's own spelling, never a private variant.
//
// The regression this pins was shipped and caught at the merge gate: the write
// route was made to canonicalise (`my_bot` -> `my-bot`) while this comparison
// stayed raw, so a pledge naming a non-canonical stored slug silently stopped
// serving the bot it named and the bill moved to another tier. Worse, the CLI
// re-sends the stored list on every pledge PUT, so toggling `enabled` rewrote a
// working row into a non-matching one.
//
// If a future change folds one edge, this reddens. Folding BOTH is legitimate —
// and then this test is the one to update, together with the route, in a
// single change.
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

// Available reports a cooldown and closed sharing hours before the bot
// allow-list, so a skip's Status alone cannot tell an entry that will serve
// the launch once its window opens from one that never will. The eligibility
// skip carries the verdict no clock clears (Refuses), the acquisition's skips
// and the preview's candidates alike: a borrower that reads a cooling entry as
// a wait reads this first.
func TestAnEligibilitySkipCarriesTheRefusalTheClockMasks(t *testing.T) {
	h := newHarness(t)
	cooling := h.now.Add(time.Hour)
	closed := &Window{StartHour: (h.now.Hour() + 12) % 24, EndHour: (h.now.Hour() + 13) % 24}
	for _, row := range []struct {
		name    string
		shape   func(*Pledge)
		botID   string
		status  Status
		refuses Status
	}{
		{"cooling, its allow-list naming the launch's bot", func(p *Pledge) { p.Bots, p.CooldownUntil = []string{"docs-refresh"}, &cooling }, "docs-refresh", StatusCooling, ""},
		{"cooling, its allow-list naming another bot", func(p *Pledge) { p.Bots, p.CooldownUntil = []string{"another-bot"}, &cooling }, "docs-refresh", StatusCooling, StatusBotFiltered},
		{"out of hours, its allow-list naming another bot", func(p *Pledge) { p.Bots, p.Window = []string{"another-bot"}, closed }, "docs-refresh", StatusOutOfHours, StatusBotFiltered},
		{"cooling, a launch naming no bot against an allow-list", func(p *Pledge) { p.Bots, p.CooldownUntil = []string{"another-bot"}, &cooling }, "", StatusCooling, StatusBotFiltered},
		{"paused", func(p *Pledge) { p.Enabled = false }, "docs-refresh", StatusPaused, StatusPaused},
		{"unhealthy", func(p *Pledge) { p.Health = HealthTokenExpired }, "docs-refresh", StatusUnhealthy, StatusUnhealthy},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.donor(t, "alice", Limits{}, row.shape)
			req := h.request("run-1")
			req.BotID = row.botID
			_, err := h.broker.Acquire(context.Background(), req)
			var nd *NoDonorError
			if !errors.As(err, &nd) || len(nd.Skips) != 1 {
				t.Fatalf("Acquire = %v, want one skip", err)
			}
			if s := nd.Skips[0]; s.Status != row.status || s.Refuses != row.refuses {
				t.Fatalf("skip = (%s, refuses %q), want (%s, refuses %q)", s.Status, s.Refuses, row.status, row.refuses)
			}
			view, err := h.broker.Preview(context.Background(), req)
			if err != nil || len(view.Candidates) != 1 {
				t.Fatalf("Preview = %+v, %v, want one candidate", view, err)
			}
			if c := view.Candidates[0]; c.Status != row.status || c.Refuses != row.refuses {
				t.Fatalf("preview candidate = (%s, refuses %q), want (%s, refuses %q)", c.Status, c.Refuses, row.status, row.refuses)
			}
		})
	}
}
