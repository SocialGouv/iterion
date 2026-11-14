package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// A webhook pinned to a forge instance (cfg.ForgeBaseURL) hands that base to
// the run it launches as forge_base_url, on every lane that can launch the
// converse bot: revi-converse's publish_reply derives its API from it, the
// only way to reach a GitLab or a Forgejo served under a sub-path, where the
// PR URL's origin names the wrong path. An unpinned webhook stamps nothing —
// the bot derives the base from the PR URL as before.
func TestConverseLanesCarryThePinnedForgeBase(t *testing.T) {
	for _, lane := range []struct {
		name string
		base string
		run  func(t *testing.T, s *Server, base string) map[string]string
	}{
		{name: "gitlab /revi <question>", base: "https://gitlab.com/gitlab", run: gitlabQuestionLane},
		{name: "gitlab reply in a Revi thread", base: "https://gitlab.com/gitlab", run: gitlabReplyLane},
		{name: "github /revi <question>", base: "https://github.com", run: githubQuestionLane},
		{name: "github review-thread reply", base: "https://github.com", run: githubReviewReplyLane},
	} {
		t.Run(lane.name, func(t *testing.T) {
			for _, base := range []string{lane.base, ""} {
				s := newWebhookTestServer(t)
				s.cfg.Bots.Paths = []string{botsDirAbs(t)}
				vars := lane.run(t, s, base)
				got, present := vars["forge_base_url"]
				switch {
				case base != "" && got != base:
					t.Errorf("pinned to %q: forge_base_url = %q (present %v), want the pin", base, got, present)
				case base == "" && present:
					t.Errorf("unpinned: forge_base_url = %q, want it absent", got)
				}
			}
		})
	}
}

// captureLaunch records the vars of the one launch a delivery makes.
func captureLaunch(t *testing.T, s *Server) func() map[string]string {
	t.Helper()
	var calls int
	var got map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		got = vars
		if botID != "revi-converse" {
			t.Errorf("launched %q, want revi-converse", botID)
		}
		return "run-base-1", nil
	}
	return func() map[string]string {
		if calls != 1 {
			t.Fatalf("%d launches, want 1", calls)
		}
		return got
	}
}

func reviCommandMap() map[string][]webhooks.CommandRoute {
	return map[string][]webhooks.CommandRoute{
		"revi": {
			{BotID: "review-pr", Mode: "direct", Scope: "pr", Disambiguator: "when_args_empty"},
			{BotID: "revi-converse", Mode: "direct", Scope: "pr", Disambiguator: "when_args_present", ArgsVar: "converse_question"},
		},
	}
}

func gitlabQuestionLane(t *testing.T, s *Server, base string) map[string]string {
	launched := captureLaunch(t, s)
	cfg := glConfig()
	cfg.ForgeBaseURL = base
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.CommandMap = reviCommandMap()
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi why is the SSRF critical?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	return launched()
}

func gitlabReplyLane(t *testing.T, s *Server, base string) map[string]string {
	s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, error) {
		return true, true, "@revi (you, the bot):\nThe SSRF fix pins the host.", "reply", nil
	}
	launched := captureLaunch(t, s)
	cfg := glConfig()
	cfg.ForgeBaseURL = base
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "Can you expand on the SSRF fix?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	return launched()
}

func githubQuestionLane(t *testing.T, s *Server, base string) map[string]string {
	launched := captureLaunch(t, s)
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "allowlist", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feature/x", TargetBranch: "main", HeadSHA: "abc123"}, nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = base
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.CommandMap = reviCommandMap()
	body := `{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "issue": {"number": 7, "title": "Add X", "body": "desc", "state": "open",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "pull_request": {"html_url": "https://github.com/acme/widgets/pull/7"}},
  "comment": {"id": 555, "body": "/revi why is the SSRF critical?",
    "html_url": "https://github.com/acme/widgets/pull/7#issuecomment-555"},
  "sender": {"login": "alice"}
}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	return launched()
}

func githubReviewReplyLane(t *testing.T, s *Server, base string) map[string]string {
	launched := captureLaunch(t, s)
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		return true, "revi (2026-09-02):\nthe SSRF is reachable\n---\n", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ForgeBaseURL = base
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "why is this SSRF reachable?"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	return launched()
}
