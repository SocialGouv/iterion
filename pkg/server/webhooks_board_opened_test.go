package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// A merge request the Studio opened from a card is authored by the bot — the
// board lane posts as the repository's bot — but it is human work: the
// marker the board lane writes into its description makes the auto-review
// guard review it, where a bot run's own merge request stays filtered
// (#2681).
func TestGitLabWebhook_AMergeRequestOpenedFromACardIsReviewed(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-1", nil
	}
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-bot"
	}
	cfg := glConfig()
	byBot := strings.Replace(glOpenMR, `"object_kind": "merge_request",`, `"object_kind": "merge_request", "user": {"username": "iterion-bot"},`, 1)

	// The bot's own merge request: filtered, as before.
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), byBot, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusOK || calls != 0 || !strings.Contains(w.Body.String(), webhooks.StatusFiltered) {
		t.Fatalf("a bot run's own MR: code=%d calls=%d body=%s, want filtered", w.Code, calls, w.Body.String())
	}
	// The same author, opened from a card: reviewed.
	fromCard := strings.Replace(byBot, `"description": "desc"`, `"description": "desc\n\n`+boardOpenedPullMarker+`"`, 1)
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), fromCard, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("an MR opened from a card by the bot: code=%d calls=%d body=%s, want a review launched", w.Code, calls, w.Body.String())
	}
}

// The GitHub twin: a pull request the Studio opened from a card is authored
// by the App; the marker makes the guard review it.
func TestGitHubWebhook_APullRequestOpenedFromACardIsReviewed(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-1", nil
	}
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-bot"
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "test-gate", nil
	}
	cfg, pt := ghConfig(t, s)
	byBot := strings.Replace(ghOpenPR, `"sender": {"login": "alice"}`, `"sender": {"login": "iterion-bot"}`, 1)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), byBot, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 0 || !strings.Contains(w.Body.String(), webhooks.StatusFiltered) {
		t.Fatalf("a bot run's own PR: code=%d calls=%d body=%s, want filtered", w.Code, calls, w.Body.String())
	}
	fromCard := strings.Replace(byBot, `"body": "desc"`, `"body": "desc\n\n`+boardOpenedPullMarker+`"`, 1)
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), fromCard, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("a PR opened from a card by the App: code=%d calls=%d body=%s, want a review launched", w.Code, calls, w.Body.String())
	}
}
