package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// An unavailable permission API must neither authorize work nor make the
// forge disable its webhook. The delivery audit is the operator's evidence.
func TestWebhookAuthzErrorsAreAcknowledgedWithoutLaunching(t *testing.T) {
	for _, lane := range []string{"github-command", "github-reply", "github-rerequest", "gitlab-command", "gitlab-reply", "gitlab-rerequest"} {
		t.Run(lane, func(t *testing.T) {
			s := newWebhookTestServer(t)
			s.cfg.Bots.Paths = []string{botsDirAbs(t)}
			failure := errors.New("permission service unavailable")
			s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
				return gateAuthorized, "", failure
			}
			s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
				return gateAuthorized, "", failure
			}
			s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
				return true, "", "", failure
			}
			s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, error) {
				return true, true, "", "", failure
			}
			s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
				return true, "", failure
			}
			s.webhookReviewRequestGate = func(context.Context, webhooks.Config, gitlab.Parsed, string) (bool, string, error) {
				return true, "", failure
			}
			s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
				return requested("iterion-bot")
			}
			calls := 0
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				calls++
				return "unexpected", nil
			}
			w := httptest.NewRecorder()
			var cfg webhooks.Config
			if strings.HasPrefix(lane, "github") {
				var token string
				cfg, token = ghConfig(t, s)
				cfg.BotIDs = []string{"feature-dev", "review-pr", "revi-converse"}
				cfg.CommandMap = map[string][]webhooks.CommandRoute{"featurly": {{BotID: "feature-dev", Scope: "any"}}}
				body, event := ghIssueCommentFeaturly, prforge.EventHeaderIssueComment
				if lane == "github-reply" {
					body, event = ghReviewCommentReply("alice", "explain"), prforge.EventHeaderReviewComment
				}
				if lane == "github-rerequest" {
					body, event = ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:01:00Z"), prforge.EventHeaderPullRequest
				}
				s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, event, token))
			} else {
				cfg = featurlyConfig()
				cfg.BotIDs = append(cfg.BotIDs, "revi-converse")
				switch lane {
				case "gitlab-command":
					s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteFeaturly))
				case "gitlab-reply":
					s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "explain"`, 1)))
				case "gitlab-rerequest":
					s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glReRequestMR("alice", "iterion-bot", "2026-09-01 10:02:00 UTC", true), gitlab.EventHeaderMergeRequest))
				}
			}
			if w.Code != http.StatusOK || calls != 0 {
				t.Fatalf("status=%d launches=%d body=%s", w.Code, calls, w.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response["status"] != webhooks.StatusLaunchError {
				t.Fatalf("response=%s err=%v", w.Body.String(), err)
			}
			rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 10)
			if err != nil || len(rows) != 1 {
				t.Fatalf("audit=%+v err=%v", rows, err)
			}
			if rows[0].Status != webhooks.StatusLaunchError || !strings.Contains(rows[0].Error, failure.Error()) || rows[0].RunID != "" {
				t.Fatalf("audit=%+v", rows[0])
			}
		})
	}
}
