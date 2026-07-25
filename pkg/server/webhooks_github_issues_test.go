package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// ghLabeledIssue is the wire shape GitHub sends on an `issues` event with
// the "labeled" action — the issue plus the single .label just applied.
const ghLabeledIssue = `{
  "action": "labeled",
  "issue": {
    "number": 42,
    "title": "Add a CSV export endpoint",
    "body": "Users need their data as CSV.",
    "html_url": "https://github.com/acme/widgets/issues/42",
    "state": "open",
    "user": {"login": "maintainer-bob"},
    "author_association": "MEMBER"
  },
  "label": {"name": "implement"},
  "repository": {"id": 7, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "sender": {"login": "maintainer-bob"}
}`

// issueConfig is ghConfig pinned to featurly + scoped to the "implement"
// label — the realistic shape of an issue-labeled webhook.
func issueConfig(t *testing.T, s *Server) (webhooks.Config, string) {
	t.Helper()
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"feature-dev"}
	cfg.LabelAllowlist = []string{"implement"}
	return cfg, pt
}

func TestGitHubWebhook_IssueLabeled_HappyPath(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef = botID, vars, repoURL, repoRef
		return "run-42", nil
	}
	cfg, pt := issueConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghLabeledIssue, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-42" {
		t.Fatalf("resp: %v", resp)
	}
	if calls != 1 || gotBot != "feature-dev" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	// The implementer-bot contract: feature prompt from title+body, open_mr,
	// and source_issue_ref pointing at the issue URL for the back-link.
	if gotVars["feature_prompt"] != "Add a CSV export endpoint\n\nUsers need their data as CSV." {
		t.Fatalf("feature_prompt: %q", gotVars["feature_prompt"])
	}
	if gotVars["open_mr"] != "true" || gotVars["source_issue_ref"] != "https://github.com/acme/widgets/issues/42" {
		t.Fatalf("mr vars: %v", gotVars)
	}
	// repoURL = clone url, repoRef empty (runner clones default branch).
	if gotURL != "https://github.com/acme/widgets.git" || gotRef != "" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
}

// Operator-pinned LaunchVars win over the handler-derived defaults.
func TestGitHubWebhook_IssueLabeled_LaunchVarsOverride(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-x", nil
	}
	cfg, pt := issueConfig(t, s)
	cfg.LaunchVars = map[string]string{"open_mr": "false", "extra": "y"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghLabeledIssue, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotVars["open_mr"] != "false" || gotVars["extra"] != "y" {
		t.Fatalf("launch-var override not applied: %v", gotVars)
	}
}

// A non-"labeled" action (e.g. opened/closed/unlabeled) must not launch.
func TestGitHubWebhook_IssueNonLabeledActionFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("non-labeled issue action must not launch")
		return "", nil
	}
	cfg, pt := issueConfig(t, s)
	body := strings.Replace(ghLabeledIssue, `"action": "labeled"`, `"action": "opened"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("opened: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("opened should be filtered: %v", resp)
	}
}

// Zero-touch lane: with AutoImplementOnOpen, an "opened" action launches the
// implementer without any label (the opt-in that turns every new issue into a
// PR). The default (flag off) stays filtered — see IssueNonLabeledActionFiltered.
func TestGitHubWebhook_IssueOpened_ZeroTouch(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-open", nil
	}
	cfg, pt := issueConfig(t, s)
	cfg.AutoImplementOnOpen = true // opt in to zero-touch
	body := strings.Replace(ghLabeledIssue, `"action": "labeled"`, `"action": "opened"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("opened+zero-touch should launch: code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotBot != "feature-dev" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	// Same implementer contract as the labeled path — it opens a back-linked PR.
	if gotVars["open_mr"] != "true" || gotVars["source_issue_ref"] == "" {
		t.Fatalf("zero-touch must carry the implement contract: %v", gotVars)
	}
}

// Zero-touch lane, UNTRUSTED author: an opened issue whose author has no
// proven repo rights (author_association NONE, no allowlist, no forge token
// for the API fallback) must be filtered — the budget boundary against
// drive-by issues. The labeled lane stays ungated (labeling needs triage+
// rights on the forge already).
func TestGitHubWebhook_IssueOpened_UntrustedAuthorParked(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("untrusted author's opened issue must not launch")
		return "", nil
	}
	cfg, pt := issueConfig(t, s)
	cfg.AutoImplementOnOpen = true
	body := strings.Replace(ghLabeledIssue, `"action": "labeled"`, `"action": "opened"`, 1)
	body = strings.Replace(body, `"user": {"login": "maintainer-bob"}`, `"user": {"login": "drive-by"}`, 1)
	body = strings.Replace(body, `"author_association": "MEMBER"`, `"author_association": "NONE"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("untrusted opened: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("untrusted opened should be filtered: %v", resp)
	}

	// Same author, but on the operator's static allowlist → launches.
	cfg.AuthorAllowlist = []string{"drive-by"}
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-a", nil
	}
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("allowlisted author should launch: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// A label outside the allowlist must not launch.
func TestGitHubWebhook_IssueWrongLabelFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a label outside the allowlist must not launch")
		return "", nil
	}
	cfg, pt := issueConfig(t, s) // allowlist ["implement"]
	body := strings.Replace(ghLabeledIssue, `"name": "implement"`, `"name": "question"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("wrong label: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("wrong label should be filtered: %v", resp)
	}
}

// Re-delivering the same (issue, label) is an idempotent replay — one launch.
func TestGitHubWebhook_IssueLabeled_IdempotentReplay(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-42", nil
	}
	cfg, pt := issueConfig(t, s)
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), ghLabeledIssue, prforge.EventHeaderIssues, pt))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghLabeledIssue, prforge.EventHeaderIssues, pt))
	if w2.Code != http.StatusOK {
		t.Fatalf("replay code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate || resp["run_id"] != "run-42" {
		t.Fatalf("duplicate resp: %v", resp)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one launch, got %d", calls)
	}
}
