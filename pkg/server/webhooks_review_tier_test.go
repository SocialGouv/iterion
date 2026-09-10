package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// TestReviewPRVars_LaunchVarsCarryReviewTier pins the per-repo pin half of
// SocialGouv/iterion#685: `review_tier` is an ORDINARY launch var, so a
// repo's integration pinning `launch_vars: {"review_tier": "glance"}`
// (durable across re-provisioning — forge.RepoIntegration.LaunchVars) must
// reach the launched run's vars through the SAME generic pass-through every
// other operator pin (gate_context, post_to_board, …) already uses — no new
// engine code, and no bot-specific branch. reviewPRVars is what every
// forge-specific PR-open/PR-event lane (GitHub, GitLab, Forgejo) calls to
// build those vars.
func TestReviewPRVars_LaunchVarsCarryReviewTier(t *testing.T) {
	launchVars := map[string]string{
		"review_tier":  "glance",
		"gate_context": "revi/review",
	}
	vars := reviewPRVars("https://github.com/acme/widgets/pull/7", "main", "fix: typo", launchVars, nil)

	if got := vars["review_tier"]; got != "glance" {
		t.Fatalf("reviewPRVars: review_tier = %q, want %q — the integration's pin was dropped", got, "glance")
	}
	if got := vars["gate_context"]; got != "revi/review" {
		t.Fatalf("reviewPRVars: gate_context = %q, want %q", got, "revi/review")
	}
	// The base defaults survive untouched when the pin doesn't name them.
	if got := vars["pr_url"]; got != "https://github.com/acme/widgets/pull/7" {
		t.Errorf("reviewPRVars: pr_url = %q, unexpected", got)
	}
}

// TestReviewPRVars_LaunchVarsPinWinsOverBaseDefault proves the "operator's
// launchVars LAST" invariant this feature depends on: a repo that ALSO pins
// post_to_board (the base default review lanes already force to "false")
// keeps its own choice — an operator's explicit pin is never silently
// replaced by the lane's own default.
func TestReviewPRVars_LaunchVarsPinWinsOverBaseDefault(t *testing.T) {
	vars := reviewPRVars("https://github.com/acme/widgets/pull/7", "main", "", map[string]string{
		"review_tier":   "audit",
		"post_to_board": "true",
	}, nil)
	if got := vars["post_to_board"]; got != "true" {
		t.Errorf("reviewPRVars: post_to_board = %q, want the operator's pin %q to win over the lane's own %q default", got, "true", "false")
	}
	if got := vars["review_tier"]; got != "audit" {
		t.Errorf("reviewPRVars: review_tier = %q, want %q", got, "audit")
	}
}

// TestBuildPRForgeCommandVars_LaunchVarsCarryReviewTier is the GitLab/GitHub
// generic-command lane's twin: /revi (and any other command) resolves its
// launch vars through buildPRForgeCommandVars, which must forward the
// integration's review_tier pin exactly like reviewPRVars does for the
// PR-open lane — one repo, one tier, regardless of which trigger fired it.
func TestBuildPRForgeCommandVars_LaunchVarsCarryReviewTier(t *testing.T) {
	route := webhooks.CommandRoute{ContextVars: map[string]string{"thread_context": "prior discussion"}}
	note := prforge.ParsedNote{
		PRURL:      "https://gitlab.example.org/acme/widgets/-/merge_requests/9",
		IssueTitle: "fix: typo",
	}
	vars := buildPRForgeCommandVars(note, nil, route, "", map[string]string{"review_tier": "glance"})
	if got := vars["review_tier"]; got != "glance" {
		t.Fatalf("buildPRForgeCommandVars: review_tier = %q, want %q — the integration's pin was dropped", got, "glance")
	}
	if got := vars["thread_context"]; got != "prior discussion" {
		t.Errorf("buildPRForgeCommandVars: thread_context = %q, the manifest's own ContextVars must still land", got)
	}
}
