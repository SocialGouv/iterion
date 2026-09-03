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
		{ID: "d1", TenantID: cfg.TenantID, WebhookID: cfg.ID, SubjectID: "pr:7", BotID: "review-pr", RunID: "run-a", Status: webhooks.StatusLaunched},
		{ID: "d2", TenantID: cfg.TenantID, WebhookID: cfg.ID, SubjectID: "pr:7", BotID: "branch-improve-loop", RunID: "run-b", Status: webhooks.StatusLaunched},
		{ID: "d3", TenantID: cfg.TenantID, WebhookID: cfg.ID, SubjectID: "pr:9", BotID: "review-pr", RunID: "run-c", Status: webhooks.StatusLaunched},
		{ID: "d4", TenantID: cfg.TenantID, WebhookID: cfg.ID, SubjectID: "pr:7", BotID: "review-pr", RunID: "run-d", Status: webhooks.StatusFiltered},
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
	if len(cancelled) != 2 {
		t.Fatalf("both live runs of pr:7 must stop, got %v", cancelled)
	}
	for _, id := range cancelled {
		if id == "run-c" {
			t.Fatal("another PR's run was cancelled — the stop must be scoped to the subject")
		}
		if id == "run-d" {
			t.Fatal("a delivery that never launched carries no run to cancel")
		}
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
