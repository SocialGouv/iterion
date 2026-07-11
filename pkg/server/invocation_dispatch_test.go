package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// TestBoardCardBody_LabeledIssue: an issue-LABELED launch puts the issue
// title+body into the route's args var (feature_prompt) and the issue URL in
// meta.SubjectURL, leaving scope_notes empty. The card body must therefore
// carry a clickable link to the issue AND the issue's title/body (the mission)
// — not just an empty trigger line.
func TestBoardCardBody_LabeledIssue(t *testing.T) {
	route := webhooks.CommandRoute{BotID: "feature-dev", ArgsVar: "feature_prompt"}
	vars := map[string]string{
		"feature_prompt": "Add CSV export\n\nUsers want to download their data.",
	}
	meta := webhookEventMeta{
		ProjectPath: "acme/widgets",
		SubjectID:   "issue:104",
		SubjectURL:  "https://github.com/acme/widgets/issues/104",
	}

	body := boardCardBody(route, vars, meta)
	if !strings.Contains(body, "(https://github.com/acme/widgets/issues/104)") {
		t.Errorf("body must link the issue URL: %q", body)
	}
	if !strings.Contains(body, "[acme/widgets/issue:104]") {
		t.Errorf("body must label the link with the subject: %q", body)
	}
	if !strings.Contains(body, "Add CSV export") || !strings.Contains(body, "Users want to download their data.") {
		t.Errorf("body must carry the mission (issue title+body): %q", body)
	}
	if strings.Contains(body, "Triggered by") {
		t.Errorf("with a subject URL the body should link it, not fall back to the trigger line: %q", body)
	}

	title := boardCardTitle(route, vars)
	if title != "feature-dev — Add CSV export" {
		t.Errorf("title should derive from the issue's first line, got %q", title)
	}
}

// TestBoardCardBody_CommandPath: a slash-command launch stamps scope_notes as
// the mission. When the subject URL is known the body links it and appends the
// mission; scope_notes wins over the args var when both are present.
func TestBoardCardBody_CommandPath(t *testing.T) {
	route := webhooks.CommandRoute{BotID: "branch-improve-loop", ArgsVar: "scope_notes"}
	vars := map[string]string{"scope_notes": "fix the flaky test"}
	meta := webhookEventMeta{
		ProjectPath: "acme/widgets",
		SubjectID:   "pr:7",
		SubjectURL:  "https://github.com/acme/widgets/pull/7",
	}

	body := boardCardBody(route, vars, meta)
	if !strings.Contains(body, "(https://github.com/acme/widgets/pull/7)") {
		t.Errorf("body must link the PR URL: %q", body)
	}
	if !strings.Contains(body, "fix the flaky test") {
		t.Errorf("body must carry the mission text: %q", body)
	}
	if got := boardCardTitle(route, vars); got != "branch-improve-loop — fix the flaky test" {
		t.Errorf("title = %q", got)
	}
}

// TestBoardCardBody_NoSubjectURL: with no subject URL (e.g. a bare command with
// no back-link), the body falls back to the provenance trigger line and still
// appends the mission so the card is never bodyless.
func TestBoardCardBody_NoSubjectURL(t *testing.T) {
	route := webhooks.CommandRoute{BotID: "docs-refresh", ArgsVar: "scope_notes"}
	vars := map[string]string{"scope_notes": "align the README"}
	meta := webhookEventMeta{ProjectPath: "acme/widgets", SubjectID: "note:9"}

	body := boardCardBody(route, vars, meta)
	if !strings.Contains(body, "Triggered by a /docs-refresh-style command on acme/widgets/note:9.") {
		t.Errorf("no-URL body must keep the trigger line: %q", body)
	}
	if !strings.Contains(body, "align the README") {
		t.Errorf("no-URL body must still carry the mission: %q", body)
	}
}

// TestBoardCardTitle_FallbackToBotID: when no mission text is available the
// title is the bare bot id (no dangling em-dash).
func TestBoardCardTitle_FallbackToBotID(t *testing.T) {
	route := webhooks.CommandRoute{BotID: "sec-audit-source", ArgsVar: "scope_notes"}
	if got := boardCardTitle(route, map[string]string{}); got != "sec-audit-source" {
		t.Errorf("title should fall back to the bot id, got %q", got)
	}
}
