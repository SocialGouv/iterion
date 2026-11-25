package webhooks

import "testing"

// LegacyIssueWinner is the election every unflipped integration answers an
// issue label with, and the one the forge subscription planner adopts its
// winner from: the two must name the same bot for the same config, so the
// election is pinned here, shape by shape. The role bots are arguments —
// the deployment's own names, not this package's.
func TestLegacyIssueWinner(t *testing.T) {
	const impl, rev = "feature-dev", "review-pr"
	bots := func(ids ...string) Config { return Config{BotIDs: ids} }
	pinned := func(def string, ids ...string) Config { return Config{BotIDs: ids, DefaultBotID: def} }
	wildcard := Config{BotIDs: []string{"*"}, WildcardBots: true}

	for _, c := range []struct {
		name                  string
		cfg                   Config
		implementer, reviewer string
		wantBot               string
		wantPermitted         bool
	}{
		{"the implementer among several bots", bots(rev, "branch-improve-loop", impl), impl, rev, impl, true},
		{"a pinned default beats a permitted implementer", pinned(rev, rev, impl), impl, rev, rev, true},
		// The pin is the operator's word: it is not replaced by a bot the
		// scope does allow, the delivery is refused naming it.
		{"a pinned default outside the scope", pinned("nestor", impl), impl, rev, "nestor", false},
		{"a reviewer-only webhook keeps its reviewer", bots(rev), impl, rev, rev, true},
		{"the implementer alone", bots(impl), impl, rev, impl, true},
		{"a sole bot that holds no role", bots("nestor"), impl, rev, "nestor", true},
		// Two label bots, neither a role bot: the election falls through to
		// the reviewer, which the scope does not allow — the refusal #3530
		// reports, and the reason the spine exists.
		{"two label bots, the reviewer not permitted", bots("nestor", "saboteur"), impl, rev, rev, false},
		{"two label bots beside a permitted reviewer", bots("nestor", "saboteur", rev), impl, rev, rev, true},
		{"two label bots beside the implementer", bots("nestor", impl, "saboteur"), impl, rev, impl, true},
		{"a wildcard scope elects the implementer", wildcard, impl, rev, impl, true},
		// No implementer role: a wildcard scope names no sole bot either, so
		// the reviewer answers, and the wildcard permits it.
		{"a wildcard scope with no implementer role", wildcard, "", rev, rev, true},
		{"a deployment whose roles are renamed", bots("builder", "critic"), "builder", "critic", "builder", true},
		{"no role and no sole bot", bots("nestor", "saboteur"), "", "", "", false},
		{"an empty scope", Config{}, impl, rev, rev, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bot, permitted := LegacyIssueWinner(c.cfg, c.implementer, c.reviewer)
			if bot != c.wantBot || permitted != c.wantPermitted {
				t.Fatalf("LegacyIssueWinner(bots %v, default %q, implementer %q, reviewer %q) = (%q, %v), want (%q, %v)",
					c.cfg.BotIDs, c.cfg.DefaultBotID, c.implementer, c.reviewer, bot, permitted, c.wantBot, c.wantPermitted)
			}
		})
	}
}
