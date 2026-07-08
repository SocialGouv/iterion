package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// TestSelectForgePRBot pins the deterministic PR-open routing: a same-repo PR
// that implements a tracked issue routes to the branch-improvement bot (Billy);
// fork PRs, ticket-less PRs, and repos that don't enable Billy all fall through
// (return "") to the default reviewer resolution. This is the fork guard + the
// ticket↔PR dedup, both encoded in one pure function.
func TestSelectForgePRBot(t *testing.T) {
	billyEnabled := webhooks.Config{BotIDs: []string{"review-pr", branchImproveBotID}}
	billyDisabled := webhooks.Config{BotIDs: []string{"review-pr"}}
	wildcard := webhooks.Config{BotIDs: []string{"*"}}

	ticketPR := prforge.Parsed{
		ProjectPath: "acme/widgets", HeadRepoFullName: "acme/widgets",
		Title: "Add subtract", Description: "Implements subtraction.\n\nFixes #12",
	}
	forkTicketPR := prforge.Parsed{
		ProjectPath: "acme/widgets", HeadRepoFullName: "mallory/widgets",
		Title: "Add subtract", Description: "Fixes #12",
	}
	standalonePR := prforge.Parsed{
		ProjectPath: "acme/widgets", HeadRepoFullName: "acme/widgets",
		Title: "Chore: tidy", Description: "no linked issue here",
	}

	cases := []struct {
		name string
		cfg  webhooks.Config
		p    prforge.Parsed
		want string
	}{
		{"same-repo ticket PR + billy enabled → billy", billyEnabled, ticketPR, branchImproveBotID},
		{"same-repo ticket PR + wildcard webhook → billy", wildcard, ticketPR, branchImproveBotID},
		{"fork ticket PR → fall through (fork guard)", billyEnabled, forkTicketPR, ""},
		{"standalone PR (no ticket) → fall through", billyEnabled, standalonePR, ""},
		{"ticket PR but billy not enabled → fall through", billyDisabled, ticketPR, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectForgePRBot(tc.cfg, tc.p); got != tc.want {
				t.Errorf("selectForgePRBot = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBranchImproveVars checks Billy's PR-open launch vars: it reviews the PR's
// branch diff over the PR base, carries the PR title+body as scope, and does
// NOT open a second MR (the PR already exists). Operator LaunchVars win last.
func TestBranchImproveVars(t *testing.T) {
	v := branchImproveVars("main", "Add subtract\n\nFixes #12", map[string]string{"max_passes": "3"})
	if v["base_ref"] != "main" {
		t.Errorf("base_ref = %q, want main", v["base_ref"])
	}
	if v["open_mr"] != "false" {
		t.Errorf("open_mr = %q, want false (PR already exists)", v["open_mr"])
	}
	if v["scope_notes"] == "" {
		t.Error("scope_notes must carry the PR title+body")
	}
	if v["max_passes"] != "3" {
		t.Errorf("operator LaunchVars must win: max_passes = %q, want 3", v["max_passes"])
	}
}
