package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// TestSelectIssueLabeledBot pins the issue-labeled routing: a freshly-labeled
// issue has no diff to review, so it must route to the implementer (Featurly),
// NOT the reviewer default — the live bug where a 3-bot webhook sent issue:85
// to review-pr, which stopped at diff_precheck. Precedence: pinned DefaultBotID
// wins; else Featurly when permitted; else the SelectBot/review-pr fallback for
// reviewer-only webhooks. The permitted path never touches Server state.
func TestSelectIssueLabeledBot(t *testing.T) {
	s := &Server{}
	meta := webhookEventMeta{Kind: "issues"}
	pick := func(cfg webhooks.Config) (string, bool) {
		return s.selectIssueLabeledBot(context.Background(), httptest.NewRecorder(), cfg, meta, "hash", "1.2.3.4")
	}
	cases := []struct {
		name string
		cfg  webhooks.Config
		want string
	}{
		{"3-bot webhook routes to implementer", webhooks.Config{BotIDs: []string{"review-pr", branchImproveBotID, featureDevBotID}}, featureDevBotID},
		{"pinned default wins", webhooks.Config{BotIDs: []string{"review-pr", featureDevBotID}, DefaultBotID: "review-pr"}, "review-pr"},
		{"reviewer-only webhook falls back", webhooks.Config{BotIDs: []string{"review-pr"}}, "review-pr"},
		{"featurly-only", webhooks.Config{BotIDs: []string{featureDevBotID}}, featureDevBotID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pick(c.cfg)
			if !ok {
				t.Fatalf("selectIssueLabeledBot returned ok=false for %+v", c.cfg.BotIDs)
			}
			if got != c.want {
				t.Errorf("selectIssueLabeledBot = %q, want %q", got, c.want)
			}
		})
	}
}

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
// branch diff over the PR base, carries the PR title+body as scope, does NOT
// open a second MR (the PR already exists), and declares the PR's source
// branch as push_branch so the bot's push-back lands its commits ON the PR.
// Operator LaunchVars win last.
func TestBranchImproveVars(t *testing.T) {
	// Default (asPR=false): push directly onto the PR's source branch.
	v := branchImproveVars("main", "feat/subtract", "Add subtract\n\nFixes #12", false, map[string]string{"max_passes": "3"})
	if v["base_ref"] != "main" {
		t.Errorf("base_ref = %q, want main", v["base_ref"])
	}
	if v["open_mr"] != "false" {
		t.Errorf("open_mr = %q, want false (PR already exists)", v["open_mr"])
	}
	if v["push_branch"] != "feat/subtract" {
		t.Errorf("push_branch = %q, want feat/subtract (the PR source branch)", v["push_branch"])
	}
	if v["scope_notes"] == "" {
		t.Error("scope_notes must carry the PR title+body")
	}
	if v["max_passes"] != "3" {
		t.Errorf("operator LaunchVars must win: max_passes = %q, want 3", v["max_passes"])
	}
}

// TestBranchImproveVars_AsPR: with branch_improve_as_pr, Billy opens a separate
// PR targeting the contributor's source branch (open_mr=true, mr_base=source)
// instead of pushing in-place — the author reviews the bot's diff in isolation.
func TestBranchImproveVars_AsPR(t *testing.T) {
	v := branchImproveVars("main", "feat/subtract", "notes", true, nil)
	if v["open_mr"] != "true" {
		t.Errorf("open_mr = %q, want true (opens a PR)", v["open_mr"])
	}
	if v["mr_base"] != "feat/subtract" {
		t.Errorf("mr_base = %q, want feat/subtract (Billy's PR targets the source branch)", v["mr_base"])
	}
	if _, direct := v["push_branch"]; direct {
		t.Errorf("push_branch must NOT be set in as-PR mode (no in-place push): %v", v)
	}
}
