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

// dropHeadRepo strips `head.repo` from a prforge fixture — the shape GitHub
// and Forgejo actually emit once a PR's head repository is deleted or blocked,
// which only a FORK head can be. Fails loudly if the fixture never carried one,
// so a fixture edit cannot turn these tests into no-ops.
func dropHeadRepo(t *testing.T, body string) string {
	t.Helper()
	const headRepo = `, "repo": {"full_name": "acme/widgets"}`
	if !strings.Contains(body, headRepo) {
		t.Fatalf("fixture carries no head.repo to drop:\n%s", body)
	}
	return strings.Replace(body, headRepo, "", 1)
}

// filteredReason returns the audit reason recorded for the most recent
// delivery on this webhook.
func filteredReason(t *testing.T, s *Server, cfg webhooks.Config) string {
	t.Helper()
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no delivery row recorded (%v)", err)
	}
	return rows[0].Error
}

// TestGitHubWebhook_UnverifiableHeadRepoBlocksEveryAutoLane pins the fork
// guard's fail-CLOSED contract on the auto path. The guard sits ahead of the
// re-request and gate-resync resolution, so ONE omitted `head.repo` must stop
// all four modes it covers — and each launch below would otherwise hand the
// runner the base repo's clone URL paired with a head-repo ref:
//
//   - opened          → a review grounded in whatever that branch name
//     resolves to on the BASE repo,
//   - dequeued        → the merge-queue auto-heal fixer PUSHING LLM commits
//     to the base repo's branch of that name — the sharpest one,
//   - synchronize     → same, on the review-on-sync lane,
//   - review_requested→ same, on the manual re-review button.
//
// The audit reason must say "not verifiable", not "fork PR": an operator
// debugging silent reviews has to tell a policy refusal from a payload the
// handler could not read.
func TestGitHubWebhook_UnverifiableHeadRepoBlocksEveryAutoLane(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T) string
		tune func(cfg *webhooks.Config)
	}{
		{
			name: "opened",
			body: func(t *testing.T) string { return dropHeadRepo(t, ghOpenPR) },
		},
		{
			name: "dequeued auto-heal",
			body: func(t *testing.T) string {
				return dropHeadRepo(t, strings.Replace(
					strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "dequeued"`, 1),
					`"number": 7,`, `"number": 7, "reason": "MERGE_CONFLICT",`, 1))
			},
			tune: func(cfg *webhooks.Config) { cfg.BotIDs = []string{"review-pr", "branch-improve-loop"} },
		},
		{
			name: "synchronize gate resync",
			body: func(t *testing.T) string {
				return dropHeadRepo(t, strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "synchronize"`, 1))
			},
			tune: func(cfg *webhooks.Config) { cfg.ReviewOnSync = true },
		},
		{
			name: "review_requested",
			body: func(t *testing.T) string {
				return dropHeadRepo(t, ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"))
			},
			tune: func(cfg *webhooks.Config) { cfg.ReviewRequestLogins = []string{"iterion-bot"} },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			launched := 0
			s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
				launched++
				return "run-x", nil
			}
			// The replier gate is wide open: only the fork guard may refuse here.
			s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
				return true, "allowlist", nil
			}
			cfg, pt := ghConfig(t, s)
			if tc.tune != nil {
				tc.tune(&cfg)
			}

			w := httptest.NewRecorder()
			s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), tc.body(t), prforge.EventHeaderPullRequest, pt))
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["status"] != webhooks.StatusFiltered {
				t.Fatalf("status=%q, want filtered", resp["status"])
			}
			if launched != 0 {
				t.Fatalf("an unverifiable head repo must launch nothing, launched=%d", launched)
			}
			if reason := filteredReason(t, s, cfg); !strings.Contains(reason, "not verifiable") {
				t.Fatalf("audit reason = %q, want it to name the unverifiable head (not a fork refusal)", reason)
			}
		})
	}
}

// A proven fork still reports as a fork, so the two refusals stay
// distinguishable in the delivery audit.
func TestGitHubWebhook_ProvenForkKeepsItsOwnAuditReason(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("fork PR must not launch any bot")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	body := strings.Replace(ghOpenPR, `"full_name": "acme/widgets"}}, "base"`, `"full_name": "mallory/widgets"}}, "base"`, 1)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	reason := filteredReason(t, s, cfg)
	if !strings.Contains(reason, "fork PR") {
		t.Fatalf("audit reason = %q, want the fork explanation", reason)
	}
	// The reason must not name an escape hatch that also refuses: the
	// `/command` lane is fork-guarded too (webhooks_prforge.go), and
	// docs/merge-gate.md says there is no manual path for fork code. An
	// operator reads this line on the delivery audit before the docs.
	if strings.Contains(reason, "can trigger a bot manually") {
		t.Fatalf("audit reason = %q advertises the /command escape hatch, which the command lane refuses on a fork", reason)
	}
}

// The review-thread reply lane launches the same base-clone + head-ref pair,
// so it fails closed on an unverifiable head too — the converse bot would
// otherwise answer a reviewer grounded in the wrong code.
func TestGitHubWebhook_ReviewThreadReplyRefusesUnverifiableHeadRepo(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("an unverifiable head repo must not launch the converse bot")
		return "", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		t.Fatal("the fork guard must refuse before any forge I/O")
		return false, "", "", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"}

	body := dropHeadRepo(t, ghReviewCommentReply("alice", "why is this SSRF reachable?"))
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if reason := filteredReason(t, s, cfg); !strings.Contains(reason, "not verifiable") {
		t.Fatalf("audit reason = %q, want it to name the unverifiable head", reason)
	}
}
