package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
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

// TestBranchImproveVars checks Billy's PR-open launch vars: it reviews the PR's
// branch diff over the PR base, carries the PR title+body as scope, does NOT
// open a second MR (the PR already exists), and declares the PR's source
// branch as push_branch so the bot's push-back lands its commits ON the PR.
// Operator LaunchVars win last.
func TestBranchImproveVars(t *testing.T) {
	// Default (asPR=false): push directly onto the PR's source branch.
	v := fixerPRVars("main", "feat/subtract", "https://github.com/acme/api/pull/12", "Add subtract\n\nFixes #12", false, map[string]string{"max_passes": "3"})
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
	if v["pr_url"] != "https://github.com/acme/api/pull/12" {
		t.Errorf("pr_url = %q, want the PR URL (for Billy's review comment)", v["pr_url"])
	}
}

// TestBranchImproveVars_AsPR: with branch_improve_as_pr, Billy opens a separate
// PR targeting the contributor's source branch (open_mr=true, mr_base=source)
// instead of pushing in-place — the author reviews the bot's diff in isolation.
func TestBranchImproveVars_AsPR(t *testing.T) {
	v := fixerPRVars("main", "feat/subtract", "https://github.com/acme/api/pull/9", "notes", true, nil)
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

func TestIsDependencyBotAuthor(t *testing.T) {
	for _, tc := range []struct {
		login string
		want  bool
	}{
		{"dependabot[bot]", true},
		{"Dependabot[bot]", true},
		{"renovate[bot]", true},
		{"renovate", true},
		{"renovate-bot", true},
		{"renovate[my-org]", true},
		{"", false},
		{"devthejo", false},
		{"someone-renovating", false},
	} {
		if got := isDependencyBotAuthor(tc.login); got != tc.want {
			t.Errorf("isDependencyBotAuthor(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
}
