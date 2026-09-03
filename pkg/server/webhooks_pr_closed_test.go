package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

const ghClosedPR = `{
  "action": "closed",
  "number": 7,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7", "state": "closed", "merged": true,
    "head": {"ref": "feature/x", "sha": "abc123"}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`

// A merged or closed pull request ends every review still bound to it: a
// run in flight burns provider quota on a diff nobody will merge, and a
// PARKED run would wake hours later to comment on a dead PR.
func TestGitHubWebhook_ClosedPRStopsItsRuns(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)

	// Two launched runs on this PR (two different bots) plus one on
	// another PR, which must be left alone.
	for _, d := range []webhooks.Delivery{
		{ID: "d1", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "review-pr", RunID: "run-a", Status: webhooks.StatusLaunched},
		{ID: "d2", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "branch-improve-loop", RunID: "run-b", Status: webhooks.StatusLaunched},
		{ID: "d3", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:9", BotID: "review-pr", RunID: "run-c", Status: webhooks.StatusLaunched},
		// A filtered delivery launched nothing, so it carries no run id —
		// which is exactly what makes it invisible to the by-subject query
		// (the store defines "launched" as having one, like CountLaunched).
		{ID: "d4", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "review-pr", Status: webhooks.StatusFiltered},
		// SAME subject id, ANOTHER repo on the same multi-project webhook:
		// PR numbers collide freely across repos, and cancelling this one
		// would block an unrelated pull request.
		{ID: "d5", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/other", SubjectID: "pr:7", BotID: "review-pr", RunID: "run-e", Status: webhooks.StatusLaunched},
		// A `/command` comment and a review-thread reply on THIS PR. Their
		// own subjects are per-comment (the idempotency key: one launch per
		// comment), so only the parent handle reaches them — without it a
		// `/revi` re-review in flight kept burning quota on the dead PR.
		{ID: "d6", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "comment:99", ParentSubjectID: "pr:7", BotID: "docs-refresh", RunID: "run-f", Status: webhooks.StatusLaunched},
		{ID: "d7", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "rc:88", ParentSubjectID: "pr:7", BotID: "revi-converse", RunID: "run-g", Status: webhooks.StatusLaunched},
		// A comment on ANOTHER pull request of the same repo: the parent
		// handle is scoped, not a wildcard.
		{ID: "d8", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "comment:77", ParentSubjectID: "pr:9", BotID: "docs-refresh", RunID: "run-h", Status: webhooks.StatusLaunched},
	} {
		if err := s.webhookDeliveries.Insert(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghClosedPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a closed PR must answer 200/filtered (a 4xx gets the hook disabled): code=%d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("a closed PR must launch nothing, launched=%d", launched)
	}
	got := map[string]bool{}
	for _, id := range cancelled {
		got[id] = true
	}
	for _, want := range []string{"run-a", "run-b", "run-f", "run-g"} {
		if !got[want] {
			t.Errorf("%s is bound to pr:7 and must stop, got %v", want, cancelled)
		}
	}
	for _, id := range cancelled {
		if id == "run-c" {
			t.Fatal("another PR's run was cancelled — the stop must be scoped to the subject")
		}
		if id == "run-d" {
			t.Fatal("a delivery that never launched carries no run to cancel")
		}
		if id == "run-e" {
			t.Fatal("a same-numbered PR of ANOTHER repo was cancelled — the stop must be project-scoped")
		}
		if id == "run-h" {
			t.Fatal("a comment on ANOTHER PR was cancelled — the parent handle must be scoped too")
		}
	}
	if len(cancelled) != 4 {
		t.Fatalf("exactly the four live runs of pr:7 must stop, got %v", cancelled)
	}
}

// The meta a comment lane builds must carry the SECOND handle naming the
// pull request it hangs off — that is what newWebhookDelivery stamps and
// what the closed-PR stop matches on. The comment's own subject stays
// per-comment: it is the idempotency key (one launch per comment).
func TestCommentLaneMetaCarriesItsPRSubject(t *testing.T) {
	onPR, err := prforge.ParseIssueComment([]byte(ghIssueCommentFeaturly))
	if err != nil {
		t.Fatal(err)
	}
	meta := prforgeNoteMeta(onPR)
	if meta.ParentSubjectID != "pr:7" {
		t.Errorf("a PR comment must name its pull request, got %q", meta.ParentSubjectID)
	}
	if meta.SubjectID != "comment:555" {
		t.Errorf("the comment's own subject must stay per-comment, got %q", meta.SubjectID)
	}
	if d := newWebhookDelivery(webhooks.Config{}, meta, webhooks.StatusAccepted, "", ""); d.ParentSubjectID != "pr:7" {
		t.Errorf("newWebhookDelivery must stamp it — it is the single point every delivery-creating lane goes through, got %q", d.ParentSubjectID)
	}
}

// The action is what says "this PR is over": a synchronize whose payload
// happens to carry a closed state is a race, and must not end a review.
func TestParsedIsClosed(t *testing.T) {
	closed, err := prforge.ParsePullRequest([]byte(ghClosedPR))
	if err != nil {
		t.Fatal(err)
	}
	if !closed.IsClosed() {
		t.Fatalf("merged PR must read as closed: %+v", closed)
	}
	open, err := prforge.ParsePullRequest([]byte(ghOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if open.IsClosed() {
		t.Fatal("an opened PR must not read as closed")
	}
}
