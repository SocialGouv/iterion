package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The review-thread REPLY gate resolved its client from the webhook's
// forge_token binding alone, so a connection-only integration — a GitHub
// App, no binding, the ordinary shape — answered "no forge token resolved"
// on every reply, while the same integration authorized /revi and the
// re-request lane through its connection. The gate resolves the client the
// way those lanes do: the covering connection first (its App client fetches
// the thread under pull_requests:read), the binding as the fallback.
func TestReviewThreadReplyServesThroughAConnectionWithoutABinding(t *testing.T) {
	s, f, cfg, pt := appConnWorld(t, false, false)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	f.perms["alice"] = "write"
	f.reviewComments = []map[string]any{
		{"id": 9002, "in_reply_to_id": 9001, "body": "why?", "path": "pkg/x/y.go", "created_at": "2026-09-02T10:05:00Z", "user": map[string]any{"login": "alice"}},
		{"id": 9001, "body": "the SSRF is reachable", "path": "pkg/x/y.go", "created_at": "2026-09-02T10:00:00Z", "user": map[string]any{"login": "iterion-forge-x[bot]"}},
	}
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"}
	launched := 0
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		launched++
		gotBot, gotVars = botID, vars
		return "run-reply", nil
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "why is this SSRF reachable?"), prforge.EventHeaderReviewComment, pt))
	if launched != 1 || gotBot != "revi-converse" {
		t.Fatalf("a connection-only integration must serve the reply gate through its connection: code=%d body=%s launched=%d bot=%q", w.Code, w.Body.String(), launched, gotBot)
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotVars["thread_context"], "iterion-forge-x[bot] (you, the bot)") {
		t.Errorf("the thread fetched through the connection must ground the bot, got thread_context=%q", gotVars["thread_context"])
	}
	bearers := f.bearersFor("review_comments")
	if len(bearers) == 0 {
		t.Fatal("the thread must have been fetched")
	}
	for _, b := range bearers {
		if !strings.HasPrefix(b, "Bearer ghs_") {
			t.Fatalf("the thread fetch must ride the connection's minted token, got %v", bearers)
		}
	}
}
