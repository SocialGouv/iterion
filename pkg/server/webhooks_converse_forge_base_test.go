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

// A webhook pinned to a forge instance (cfg.ForgeBaseURL) hands the base
// it STORES to the run it launches as forge_base_url, on every lane that can
// launch the converse bot; revi-converse's publish_reply derives its API from
// it. The stored value depends on where the pin came from, and the test reads
// it from the real code of each source:
//   - a webhook provisioned from a forge connection stores the connection's
//     base URL, path included (forge.CanonicalBaseURL, Connection.BaseURL) —
//     the source that reaches a GitLab or Forgejo under a sub-path;
//   - a webhook pinned through the webhook API stores scheme://host only
//     (validateForgeBaseURL drops the path), so a sub-path pinned there
//     reaches the run as its origin.
//
// An unpinned webhook stamps nothing — the bot derives the base from the PR
// URL.
func TestConverseLanesCarryThePinnedForgeBase(t *testing.T) {
	apiStored := func(t *testing.T, raw string) string {
		t.Helper()
		v, err := validateForgeBaseURL(raw)
		if err != nil {
			t.Fatalf("validateForgeBaseURL(%q): %v", raw, err)
		}
		return v
	}
	connStored := func(p forge.Provider, raw string) string {
		c := forge.Connection{Provider: p, ForgeBaseURL: forge.CanonicalBaseURL(p, raw)}
		return c.BaseURL()
	}
	if got := connStored(forge.ProviderGitLab, "https://gitlab.com/gitlab/"); got != "https://gitlab.com/gitlab" {
		t.Fatalf("a connection under a sub-path stores %q, want the path kept — the premise of this test moved", got)
	}
	if got := apiStored(t, "https://gitlab.com/gitlab"); got != "https://gitlab.com" {
		t.Fatalf("the webhook API stores %q for a sub-path pin, want the origin only — the documented caveat moved", got)
	}
	for _, lane := range []struct {
		name     string
		provider forge.Provider
		pin      string
		run      func(t *testing.T, s *Server, base string) map[string]string
	}{
		{name: "gitlab /revi <question>", provider: forge.ProviderGitLab, pin: "https://gitlab.com/gitlab", run: gitlabQuestionLane},
		{name: "gitlab reply in a Revi thread", provider: forge.ProviderGitLab, pin: "https://gitlab.com/gitlab", run: gitlabReplyLane},
		{name: "github /revi <question>", provider: forge.ProviderGitHub, pin: "https://github.com", run: githubQuestionLane},
		{name: "github review-thread reply", provider: forge.ProviderGitHub, pin: "https://github.com", run: githubReviewReplyLane},
	} {
		t.Run(lane.name, func(t *testing.T) {
			for source, stored := range map[string]string{
				"connection-provisioned": connStored(lane.provider, lane.pin),
				"pinned through the API": apiStored(t, lane.pin),
				"unpinned":               "",
			} {
				s := newWebhookTestServer(t)
				s.cfg.Bots.Paths = []string{botsDirAbs(t)}
				vars := lane.run(t, s, stored)
				got, present := vars["forge_base_url"]
				switch {
				case stored != "" && got != stored:
					t.Errorf("%s (stored %q): forge_base_url = %q (present %v), want the stored base", source, stored, got, present)
				case stored == "" && present:
					t.Errorf("%s: forge_base_url = %q, want it absent", source, got)
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
