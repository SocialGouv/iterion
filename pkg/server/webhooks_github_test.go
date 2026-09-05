package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// ghConfig returns a fresh hmac-mode GitHub Config seeded with a sealed
// plaintext + token mirror. The handler tests use it as a baseline.
func ghConfig(t *testing.T, s *Server) (webhooks.Config, string) {
	t.Helper()
	pt, hash, last4, fp, err := webhooks.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := webhooks.Config{
		ID:           "ghw",
		TenantID:     "t1",
		Provider:     webhooks.ProviderGitHub,
		SignMode:     webhooks.SignModeHMAC,
		Enabled:      true,
		TokenHash:    hash,
		TokenLast4:   last4,
		Fingerprint:  fp,
		BotIDs:       []string{"review-pr"},
		KeyOverrides: map[string]string{},
	}
	sealed, err := webhooks.SealHMACSecret(s.sealer, cfg.ID, pt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HMACSecretSealed = sealed
	return cfg, pt
}

// ghReq builds a request with X-GitHub-Event + X-Hub-Signature-256
// (sha256= prefix matches GitHub's wire format).
func ghReq(ctx context.Context, body, event, pt string) *http.Request {
	r := httptest.NewRequest("POST", "/api/webhooks/github/ghw", strings.NewReader(body)).WithContext(ctx)
	if event != "" {
		r.Header.Set("X-GitHub-Event", event)
	}
	if pt != "" {
		mac := hmac.New(sha256.New, []byte(pt))
		mac.Write([]byte(body))
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	r.SetPathValue("id", "ghw")
	return r
}

func ghCtx(cfg webhooks.Config) context.Context {
	return gitlabCtx(cfg) // same identity-stamping, just a different provider.
}

const ghOpenPR = `{
  "action": "opened",
  "number": 7,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123"}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`

func TestGitHubWebhook_HappyPath(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef = botID, vars, repoURL, repoRef
		return "run-7", nil
	}
	cfg, pt := ghConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-7" {
		t.Fatalf("resp: %v", resp)
	}
	if calls != 1 || gotBot != "review-pr" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["pr_url"] != "https://github.com/acme/widgets/pull/7" || gotVars["base_ref"] != "main" || gotVars["post_to_board"] != "false" {
		t.Fatalf("vars: %v", gotVars)
	}
	if gotURL != "https://github.com/acme/widgets.git" || gotRef != "feature/x" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
}

// ghReviewRequested builds a `review_requested` delivery (the GitHub
// "Request review" / "Re-request review" gesture) targeting `reviewer`.
func ghReviewRequested(sender, reviewer, updatedAt string) string {
	return `{
	  "action": "review_requested", "number": 7,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "requested_reviewer": {"login": "` + reviewer + `"},
	  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
	    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open", "updated_at": "` + updatedAt + `",
	    "head": {"ref": "feature/x", "sha": "abc123"}, "base": {"ref": "main"}},
	  "sender": {"login": "` + sender + `"}
	}`
}

// The forge-native re-review button, GitHub side: a review_requested action
// naming iterion's own account launches the review bot — even on a head the
// PR-open lane already claimed. One naming anyone else stays filtered, and
// the bot re-requesting (its own API write echoing back) never self-triggers.
func TestGitHubWebhook_ReviewRequestedLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotVars = vars
		return "run-7", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-bot"
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "test-gate", nil
	}
	cfg, pt := ghConfig(t, s)

	// The open claims the head under the ordinary key space…
	w0 := httptest.NewRecorder()
	s.handleGitHubWebhook(w0, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w0.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("open: code=%d calls=%d", w0.Code, calls)
	}

	// …and the re-request still relaunches on that same head.
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w1.Code != http.StatusAccepted || calls != 2 {
		t.Fatalf("re-request: code=%d calls=%d body=%s", w1.Code, calls, w1.Body.String())
	}
	if gotVars["re_review"] != "true" || gotVars["head_sha"] != "abc123" {
		t.Fatalf("re-request vars: %v", gotVars)
	}

	// Re-request targeting a human reviewer → filtered.
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghReviewRequested("alice", "bob", "2026-09-01T10:05:00Z"), prforge.EventHeaderPullRequest, pt))
	if w2.Code != http.StatusOK || calls != 2 {
		t.Fatalf("other reviewer: code=%d calls=%d", w2.Code, calls)
	}

	// The bot as ACTOR (its own re-request write echoing back) → filtered.
	w3 := httptest.NewRecorder()
	s.handleGitHubWebhook(w3, ghReq(ghCtx(cfg), ghReviewRequested("iterion-bot", "iterion-bot", "2026-09-01T10:10:00Z"), prforge.EventHeaderPullRequest, pt))
	if w3.Code != http.StatusOK || calls != 2 {
		t.Fatalf("bot actor: code=%d calls=%d body=%s", w3.Code, calls, w3.Body.String())
	}

	// A re-request on a CLOSED or MERGED PR never burns a run — reviewer
	// edits arrive freely on dead PRs.
	for _, state := range []string{"closed", "merged"} {
		closed := strings.Replace(ghReviewRequested("alice", "iterion-bot", "2026-09-01T11:00:00Z"), `"state": "open"`, `"state": "`+state+`"`, 1)
		if closed == ghReviewRequested("alice", "iterion-bot", "2026-09-01T11:00:00Z") {
			t.Fatal("fixture state replacement did not apply")
		}
		wc := httptest.NewRecorder()
		s.handleGitHubWebhook(wc, ghReq(ghCtx(cfg), closed, prforge.EventHeaderPullRequest, pt))
		if wc.Code != http.StatusOK || calls != 2 {
			t.Fatalf("state=%s: code=%d calls=%d body=%s", state, wc.Code, calls, wc.Body.String())
		}
	}
}

// ghTicketPR: a same-repo PR (head.repo == base repo) that closes an issue.
const ghTicketPR = `{
  "action": "opened", "number": 9,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 9, "title": "Add subtract", "body": "Implements subtraction.\n\nFixes #12",
    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
    "head": {"ref": "feat/subtract", "sha": "aaa111", "repo": {"full_name": "acme/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "alice"}
}`

// ghDraftTicketPR: a same-repo ticket PR opened as a DRAFT — must NOT
// auto-launch a bot (the author is still iterating).
const ghDraftTicketPR = `{
  "action": "opened", "number": 9,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 9, "title": "Add subtract", "body": "Implements subtraction.\n\nFixes #12", "draft": true,
    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
    "head": {"ref": "feat/subtract", "sha": "aaa111", "repo": {"full_name": "acme/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "alice"}
}`

// ghReadyForReviewPR: the draft above marked ready-for-review — THE
// auto-trigger (draft flag now false).
const ghReadyForReviewPR = `{
  "action": "ready_for_review", "number": 9,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 9, "title": "Add subtract", "body": "Implements subtraction.\n\nFixes #12", "draft": false,
    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
    "head": {"ref": "feat/subtract", "sha": "aaa111", "repo": {"full_name": "acme/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "alice"}
}`

// ghDequeuedPR: a PR ejected from the merge queue for a conflict → the
// auto-heal path dispatches Billy to rebase+resolve+repush.
const ghDequeuedPR = `{
  "action": "dequeued", "number": 9, "reason": "MERGE_CONFLICT",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 9, "title": "Add subtract", "body": "Implements subtraction.",
    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
    "head": {"ref": "feat/subtract", "sha": "aaa111", "repo": {"full_name": "acme/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "alice"}
}`

// A DRAFT PR never auto-launches a bot — the author is still iterating.
func TestGitHubWebhook_DraftPRNotAutoLaunched(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghDraftTicketPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("draft PR must be filtered, got %q", resp["status"])
	}
	if launched != 0 {
		t.Fatalf("draft PR must NOT auto-launch any bot, launched=%d", launched)
	}
}

// Marking a draft PR ready-for-review IS the auto-trigger — and the auto-lane
// is REVIEW-ONLY (Revi). Even a ticket PR must NOT auto-launch Billy on open:
// Billy runs on a PR only on a deliberate `/billy` command (or the auto-heal
// path). This pins the decoupling (req 1+2).
func TestGitHubWebhook_ReadyForReviewLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotBot string
	s.webhookLaunchBot = func(_ context.Context, botID string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotBot = botID
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReadyForReviewPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotBot != "review-pr" {
		t.Fatalf("ready_for_review PR must auto-REVIEW (Revi), never auto-launch Billy; got %q", gotBot)
	}
}

// TestGitHubWebhook_DequeuedPRAutoHeals: a merge-queue ejection for a conflict
// dispatches Billy to reconcile the branch with the base + re-enter the queue.
func TestGitHubWebhook_DequeuedPRAutoHeals(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotBot, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, repoRef, _ string, _, _ map[string]string) (string, error) {
		gotBot, gotVars, gotRef = botID, vars, repoRef
		return "run-heal", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghDequeuedPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotBot != "branch-improve-loop" {
		t.Fatalf("dequeued PR must auto-heal via Billy, got %q", gotBot)
	}
	if gotVars["open_mr"] != "false" || gotVars["push_branch"] != "feat/subtract" || gotVars["base_ref"] != "main" {
		t.Fatalf("heal vars wrong: %v", gotVars)
	}
	if gotRef != "feat/subtract" {
		t.Fatalf("heal must check out the PR head branch, got %q", gotRef)
	}
	if !strings.Contains(gotVars["scope_notes"], "ejected from the merge queue") || !strings.Contains(gotVars["scope_notes"], "MERGE_CONFLICT") {
		t.Fatalf("heal mission must state the queue-eject reason: %q", gotVars["scope_notes"])
	}
}

// TestGitHubWebhook_DequeuedNonHealableReasonIgnored: a dequeue for a
// non-fixable reason (manual dequeue / queue reset) launches nothing.
func TestGitHubWebhook_DequeuedNonHealableReasonIgnored(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	body := `{"action":"dequeued","number":9,"reason":"DEQUEUED_MANUALLY","repository":{"id":42,"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"pull_request":{"number":9,"title":"x","state":"open","html_url":"u","head":{"ref":"b","sha":"s","repo":{"full_name":"acme/widgets"}},"base":{"ref":"main","repo":{"full_name":"acme/widgets"}}},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if launched != 0 {
		t.Fatalf("a non-healable dequeue must launch nothing, launched=%d", launched)
	}
}

// ghForkTicketPR: same ticket link but head.repo is a FORK → the guard keeps it
// on the reviewer path.
const ghForkTicketPR = `{
  "action": "opened", "number": 10,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 10, "title": "Add subtract", "body": "Fixes #12",
    "html_url": "https://github.com/acme/widgets/pull/10", "state": "open",
    "head": {"ref": "patch-1", "sha": "bbb222", "repo": {"full_name": "mallory/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "mallory"}
}`

// TestGitHubWebhook_TicketPRAutoReviewsNeverBilly: a same-repo PR that closes an
// issue must NOT auto-launch Billy on open (req 1+2). Even with Billy enabled on
// the webhook, the PR-open auto-lane is REVIEW-ONLY — it routes to Revi with the
// review vars, never the mutating branch-improve loop.
func TestGitHubWebhook_TicketPRAutoReviewsNeverBilly(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotBot, gotVars = botID, vars
		return "run-review", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"} // Billy enabled — still must not auto-launch

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghTicketPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotBot != "review-pr" {
		t.Fatalf("ticket PR must auto-REVIEW (Revi), never auto-launch Billy; got %q", gotBot)
	}
	// Review vars, not Billy's push-back vars.
	if gotVars["post_to_board"] != "false" || gotVars["pr_review_mode"] != "inline" {
		t.Fatalf("expected Revi review vars, got %v", gotVars)
	}
	if _, ok := gotVars["push_branch"]; ok {
		t.Fatalf("review lane must NOT carry Billy's push_branch: %v", gotVars)
	}
	if gotVars["pr_author"] != "alice" {
		t.Fatalf("review run must stamp pr_author, got %v", gotVars)
	}
}

// TestGitHubWebhook_IterionBotPRSkipsReview: a PR opened by iterion's OWN forge
// bot (login = <app_slug>[bot], resolved from the provisioned connection) is
// NOT auto-reviewed — it already converged in its own loop (req 5). A generic
// [bot] (Dependabot) is unaffected (covered by the auto-review happy path).
func TestGitHubWebhook_IterionBotPRSkipsReview(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	// Seam: the provisioned connection's bot is "iterion-forge-1234[bot]".
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-forge-1234[bot]"
	}
	cfg, pt := ghConfig(t, s)

	body := `{
	  "action": "opened", "number": 21,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 21, "title": "Docs: align", "body": "aligned by Doki",
	    "html_url": "https://github.com/acme/widgets/pull/21", "state": "open",
	    "head": {"ref": "iterion/run/docs", "sha": "ddd333", "repo": {"full_name": "acme/widgets"}},
	    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
	  "sender": {"login": "iterion-forge-1234[bot]"}
	}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("iterion-bot PR must be filtered, got %q", resp["status"])
	}
	if launched != 0 {
		t.Fatalf("iterion-bot PR must NOT auto-launch any bot, launched=%d", launched)
	}
}

// TestGitHubWebhook_ForkTicketPRStaysOnReviewer: the fork guard — a fork PR that
// closes an issue must NOT route to the mutating bot; it stays on the reviewer.
// A fork PR NEVER auto-launches a bot — not even the reviewer — regardless of
// block_fork_prs. The auto path is untrusted (adversary-controlled code + budget
// exhaustion); a repo collaborator triggers a bot manually via a command
// instead (gated on CollaboratorPermission in handlePRForgeComment).
func TestGitHubWebhook_ForkPRBlockedFromAutoLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	// block_fork_prs deliberately NOT set — the guard is unconditional now.

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghForkTicketPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("fork PR must be filtered on the auto path, got %q", resp["status"])
	}
	if launched != 0 {
		t.Fatalf("fork PR must NOT auto-launch any bot, launched=%d", launched)
	}
}

// TestGitHubWebhook_BlockForkPRs: with block_fork_prs on, a fork PR is filtered
// (NO bot launches) — the opt-in anti budget-exhaustion boundary.
func TestGitHubWebhook_BlockForkPRs(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	cfg.BlockForkPRs = true

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghForkTicketPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("fork PR must be filtered with block_fork_prs, got %v", resp)
	}
	if launched != 0 {
		t.Fatalf("no bot may launch on a blocked fork PR, launched=%d", launched)
	}
}

// TestGitHubWebhook_RequiredSecretUnresolvedRecordsLaunchError: when the launch
// fails because a required workflow secret resolves to nothing, the delivery
// trail records StatusLaunchError with the failure reason — never a silent
// degrade. (The launcher stands in for the SubmitLaunch → resolveAndSeal path
// that produces this error in production.)
func TestGitHubWebhook_RequiredSecretUnresolvedRecordsLaunchError(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "", secrets.RequiredSecretsError([]string{"test_e2e_canary"}, "this team/bot")
	}
	cfg, pt := ghConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("launch failure should be a 502, got code=%d body=%s", w.Code, w.Body.String())
	}
	list, err := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "ghw", 10)
	if err != nil {
		t.Fatalf("ListByWebhook: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 delivery row, got %d", len(list))
	}
	if list[0].Status != webhooks.StatusLaunchError {
		t.Fatalf("delivery status = %q, want %q", list[0].Status, webhooks.StatusLaunchError)
	}
	if !strings.Contains(list[0].Error, "test_e2e_canary") {
		t.Fatalf("delivery error should name the unresolved secret, got %q", list[0].Error)
	}
	if list[0].RunID != "" {
		t.Fatalf("no run should be recorded on the delivery, got run_id=%q", list[0].RunID)
	}
}

func TestGitHubWebhook_BadHMAC(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("launch must not be reached on bad signature")
		return "", nil
	}
	cfg, _ := ghConfig(t, s)
	w := httptest.NewRecorder()
	// Sign with a wrong key.
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, "iwh_wrong_key"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad hmac: code=%d body=%s", w.Code, w.Body.String())
	}
	// No delivery row written either (auth gate is strictly before audit).
	if list, _ := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "ghw", 10); len(list) != 0 {
		t.Fatalf("bad-hmac delivery should not be recorded, got %d rows", len(list))
	}
}

func TestGitHubWebhook_NonPullRequestEventFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("ping must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), `{"zen":"yo"}`, "ping", pt))
	if w.Code != http.StatusOK {
		t.Fatalf("ping: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("ping should be filtered, got %v", resp)
	}
}

func TestGitHubWebhook_SynchronizeFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("synchronize must not launch (auto-review on open only)")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	body := strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "synchronize"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("sync: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("sync should be filtered: %v", resp)
	}
}

func TestGitHubWebhook_ProjectAllowlistMismatch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("mismatched repo must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ProjectAllowlist = []string{"other/*"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("mismatched repo: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("mismatched repo should be filtered: %v", resp)
	}
}

// A webhook scoped to dependency bots must ignore a human PR.
func TestGitHubWebhook_AuthorAllowlistFiltersHuman(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("human-authored PR must not launch when author-allowlisted to bots")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.AuthorAllowlist = []string{"dependabot[bot]", "renovate[bot]"}
	w := httptest.NewRecorder()
	// ghOpenPR's sender is "alice" (a human) → filtered.
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("filtered author: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("human author should be filtered: %v", resp)
	}
}

// The same webhook launches on a Dependabot PR and stamps pr_author.
func TestGitHubWebhook_AuthorAllowlistLaunchesBot(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotVars map[string]string
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotVars = vars
		return "run-dep", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.AuthorAllowlist = []string{"dependabot[bot]"}
	depPR := strings.Replace(ghOpenPR, `"sender": {"login": "alice"}`, `"sender": {"login": "dependabot[bot]"}`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), depPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("dependabot PR should launch: code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("expected one launch, got %d", calls)
	}
	if gotVars["pr_author"] != "dependabot[bot]" {
		t.Fatalf("pr_author not stamped: %v", gotVars)
	}
}

func TestGitHubWebhook_IdempotentReplay(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-7", nil
	}
	cfg, pt := ghConfig(t, s)
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w2.Code != http.StatusOK {
		t.Fatalf("replay code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate || resp["run_id"] != "run-7" {
		t.Fatalf("duplicate resp: %v", resp)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one launch, got %d", calls)
	}
}

// TestGitHubWebhook_RelaunchAfterFailure pins F5: a delivery whose launch
// FAILED (no run created) must be relaunchable by a redelivery of the same
// event — a transient failure (broken bot, LLM 5xx, deploy window) must not
// poison re-review for that (repo, PR#, head sha) until a new commit changes
// the idempotency key. The first launch errors (502); the redelivery relaunches
// (202), it is NOT swallowed as a terminal duplicate.
func TestGitHubWebhook_RelaunchAfterFailure(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("transient launch failure")
		}
		return "run-ok", nil
	}
	cfg, pt := ghConfig(t, s)

	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w1.Code != http.StatusBadGateway {
		t.Fatalf("first (failed launch): code=%d body=%s", w1.Code, w1.Body.String())
	}

	// Redeliver the SAME event: must relaunch, not short-circuit as duplicate.
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w2.Code != http.StatusAccepted {
		t.Fatalf("redelivery must relaunch after a prior failure: code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-ok" {
		t.Fatalf("relaunch resp: %v", resp)
	}
	if calls != 2 {
		t.Fatalf("expected two launch attempts (fail then succeed), got %d", calls)
	}

	// A THIRD delivery, now that the run succeeded, is a terminal duplicate.
	w3 := httptest.NewRecorder()
	s.handleGitHubWebhook(w3, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w3.Code != http.StatusOK {
		t.Fatalf("post-success replay must be duplicate: code=%d", w3.Code)
	}
	if calls != 2 {
		t.Fatalf("post-success replay must not relaunch, got %d calls", calls)
	}
}

func TestGitHubWebhook_BotNotAllowed(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("disallowed bot must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	// SelectBot() returns "" when there are multiple non-default bots
	// → handler falls back to defaultWebhookBotReviewPR. Pin two bots
	// that exclude review-pr so the bot-scope gate fires.
	cfg.BotIDs = []string{"some-other-bot", "and-another"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	// A delivery no enabled bot claims is FILTERED (200), not rejected: a 4xx
	// on a forge hook is what makes GitHub disable it after repeated
	// deliveries, and "nobody wants this PR" is a routing outcome, not a
	// client error. The launcher assertion above is what proves nothing ran.
	if w.Code != http.StatusOK {
		t.Fatalf("bot not allowed: code=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); !strings.Contains(got, webhooks.StatusFiltered) {
		t.Fatalf("bot not allowed: want filtered status, got %s", got)
	}
}

// R6a15fe: the GitHub/Forgejo re-request lane rides the same replier gate as
// its GitLab twin — with no stub the production gate fail-closes on the
// missing forge token, an explicit refusal filters, and an authz ERROR 502s
// only when the click was the delivery's sole reason (R34eb8c).
func TestGitHubWebhook_ReviewRequestedUnauthorizedFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run1", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	cfg, pt := ghConfig(t, s)

	// Production gate, no stub: no forge token → refused, filtered.
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("mallory", "iterion-bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("unauthorized: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}

	// Authz error on a re-request-only delivery → 502 (forge redelivers).
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return false, "", context.DeadlineExceeded
	}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghReviewRequested("mallory", "iterion-bot", "2026-09-01T10:01:00Z"), prforge.EventHeaderPullRequest, pt))
	if w2.Code != http.StatusBadGateway || calls != 0 {
		t.Fatalf("authz error must 502: code=%d calls=%d body=%s", w2.Code, calls, w2.Body.String())
	}
}

// R0c3aab: the replier gate runs AFTER the event/project/author scope filter
// — an out-of-scope delivery must never cost a forge API call nor be able to
// 502 the endpoint. And when the gate demotes the gesture, the hold label it
// had provisionally waived is re-applied.
func TestGitHubWebhook_ReviewRequestScopeFilterBeforeGate(t *testing.T) {
	s := newWebhookTestServer(t)
	var launches, gateCalls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		return "run1", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		gateCalls++
		return false, "", context.DeadlineExceeded // would 502 if ever reached
	}
	cfg, pt := ghConfig(t, s)
	cfg.ProjectAllowlist = []string{"other/repo"} // acme/widgets is out of scope

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T12:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || launches != 0 {
		t.Fatalf("out-of-scope must filter: code=%d launches=%d body=%s", w.Code, launches, w.Body.String())
	}
	if gateCalls != 0 {
		t.Fatalf("out-of-scope delivery reached the forge authz gate (%d calls) — scope filters must run first", gateCalls)
	}
	// (The demote+hold-label re-apply branch is defensive here: on prforge
	// the review_requested / synchronize / opened actions are mutually
	// exclusive, so a demoted gesture never co-rides an admissible lane.
	// The reachable version of that path is the GitLab lane's, covered by
	// TestGitLabWebhook_ReRequestUnauthorizedReplierFiltered.)
}

// The gate-resync lane shares the closed-PR rule with the re-request lane
// (its sibling term): a push to a closed/merged PR's branch still delivers
// `synchronize` and must not burn a review. A payload WITHOUT a state stays
// fail-open — filtering it would strand the required check on the new head.
func TestGitHubWebhook_ResyncOnDeadPRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run1", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	sync := strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "synchronize"`, 1)
	for _, state := range []string{"closed", "merged"} {
		body := strings.Replace(sync, `"state": "open"`, `"state": "`+state+`"`, 1)
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
		if w.Code != http.StatusOK || calls != 0 {
			t.Fatalf("state=%s: code=%d calls=%d body=%s", state, w.Code, calls, w.Body.String())
		}
	}
	// A state-less payload keeps the gate following the head.
	stateless := strings.Replace(sync, ` "state": "open",`, ``, 1)
	if stateless == sync {
		t.Fatal("fixture state removal did not apply")
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), stateless, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("stateless resync must stay reviewable: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// TestGitHubWebhook_GateResyncSurvivesBotGuard: the iterion-bot guard skips a
// PR our own loop produced, keyed on the SENDER. On a merge-gate resync the
// sender is by construction our own forge bot — the fixer that just pushed —
// so the guard would swallow the one delivery the gate depends on: the
// re-review that re-posts the required check on the new head and supersedes
// the fixer's own verdict. A PR the bot OPENS is still skipped.
func TestGitHubWebhook_GateResyncSurvivesBotGuard(t *testing.T) {
	newSrv := func(t *testing.T) (*Server, webhooks.Config, string, *int) {
		t.Helper()
		s := newWebhookTestServer(t)
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run1", nil
		}
		// The forge bot is the sender on everything this webhook receives.
		s.webhookIterionBotAuthor = func(context.Context, webhooks.Config, string) bool { return true }
		cfg, pt := ghConfig(t, s)
		cfg.ReviewOnSync = true
		return s, cfg, pt, &launched
	}
	status := func(w *httptest.ResponseRecorder) string {
		var resp map[string]string
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp["status"]
	}

	s, cfg, pt, launched := newSrv(t)
	body := strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "synchronize"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if got := status(w); got == webhooks.StatusFiltered {
		t.Errorf("a gate resync pushed by the forge bot must still be reviewed, got %q", got)
	}
	if *launched == 0 {
		t.Error("gate resync launched no review — the required check would stay absent on the new head")
	}

	// Same sender, PR opened: still the converged-in-its-own-loop case.
	s2, cfg2, pt2, launched2 := newSrv(t)
	w = httptest.NewRecorder()
	s2.handleGitHubWebhook(w, ghReq(ghCtx(cfg2), ghOpenPR, prforge.EventHeaderPullRequest, pt2))
	if got := status(w); got != webhooks.StatusFiltered {
		t.Errorf("a PR opened by the forge bot must still be skipped, got %q", got)
	}
	if *launched2 != 0 {
		t.Error("a bot-opened PR must not auto-review")
	}
}

// The GitHub arming half of review_request_logins: a configured USER login
// arms the lane where the connection-derived identity cannot (a GitHub App
// is never a requested reviewer), and BOTH halves of the loop guard read the
// same set — the actor half must recognise the configured identity's own
// reviewer-write echo.
func TestGitHubWebhook_ReviewRequestedArmsOnConfiguredLogin(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return fmt.Sprintf("run-%d", calls), nil
	}
	// Replier authorized — this test exercises identity matching, not the gate.
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"@iterion-bot"} // a pasted "@handle" is tolerated

	// Addressed to the configured identity → the reviewer launches.
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "Iterion-Bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("configured identity: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	// Addressed to anyone else → filtered, as this action always was.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "bob", "2026-09-01T10:05:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("other reviewer: code=%d calls=%d", w.Code, calls)
	}
	// The configured identity as ACTOR — its own reviewer write echoing back —
	// must not launch: both halves of the guard read the same set.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("iterion-bot", "iterion-bot", "2026-09-01T10:10:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("self request: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	// Nothing configured → the lane is inert, exactly as before.
	bare, pt2 := ghConfig(t, s)
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(bare), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:15:00Z"), prforge.EventHeaderPullRequest, pt2))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("unarmed webhook: code=%d calls=%d", w.Code, calls)
	}
}

// The hold label freezes EVERY automation on a PR, the re-request included.
// The forge emits the same event for a CODEOWNERS auto-request, which needs no
// permission from the requester and carries nothing to tell it from a click —
// so the lane cannot claim the deliberateness a `/command` has.
func TestGitHubWebhook_ReviewRequestedRespectsHoldLabel(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-1", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}
	cfg.HoldLabels = []string{"iterion:hold"}

	body := strings.Replace(
		ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"),
		`"state": "open",`, `"state": "open", "labels": [{"name": "iterion:hold"}],`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("held PR: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no delivery row recorded (%v)", err)
	}
	if !strings.Contains(rows[0].Error, "hold label") {
		t.Fatalf("audit reason = %q, want the hold-label explanation", rows[0].Error)
	}
}

// R35dde4: with the identity in CODEOWNERS, a single PR open delivers BOTH
// `opened` and an automatic `review_requested` — the second must collapse
// onto the review the first just launched, not double-spend on the same
// head. Once that review FINISHES, the same gesture is the ordinary
// re-review click and relaunches.
func TestGitHubWebhook_ReviewRequestedCollapsesOntoLiveReview(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return fmt.Sprintf("run-%d", calls), nil
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}

	// PR open claims the head and launches run-1…
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("open: code=%d calls=%d", w.Code, calls)
	}

	// …the CODEOWNERS auto-request lands seconds later while run-1 is live:
	// collapsed, with the reason in the audit row.
	s.webhookRunIsLive = func(_ context.Context, runID string) bool { return runID == "run-1" }
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("auto-request on a live review must collapse: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 5)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no delivery row recorded (%v)", err)
	}
	found := false
	for _, row := range rows {
		if strings.Contains(row.Error, "already in flight") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no delivery row carries the collapse reason: %+v", rows)
	}

	// Review finished → the same gesture is a deliberate re-review: relaunch.
	s.webhookRunIsLive = func(context.Context, string) bool { return false }
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:05:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 2 {
		t.Fatalf("re-request after the review finished must relaunch: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// Rf96744: a delivery row stranded at `accepted` (a crash between the insert
// and the post-launch update) must not read as in-flight forever — past the
// launch window the re-request lane treats it as a finished claim and the
// button relaunches instead of collapsing for good.
func TestGitHubWebhook_ReviewRequestedIgnoresStrandedAcceptedRow(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-1", nil
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}

	// Seed the per-head claim as a stranded `accepted` row: no RunID, and
	// received well past the launch window.
	headBase := fmt.Sprintf("gh|%s|%s|acme/widgets|7|abc123", cfg.TenantID, cfg.ID)
	if err := s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
		ID: "stranded", TenantID: cfg.TenantID, WebhookID: cfg.ID, Provider: cfg.Provider,
		IdempotencyKey: forgeIdemKey(headBase, "review-pr", cfg.HasBotRules()),
		Status:         webhooks.StatusAccepted,
		ReceivedAt:     time.Now().UTC().Add(-2 * acceptedLaunchWindow),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("re-request past a stranded accepted row must relaunch: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}

	// A FRESH accepted row is a launch in progress: the same gesture collapses.
	if err := s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
		ID: "in-progress", TenantID: cfg.TenantID, WebhookID: cfg.ID, Provider: cfg.Provider,
		IdempotencyKey: forgeIdemKey(fmt.Sprintf("gh|%s|%s|acme/widgets|8|def456", cfg.TenantID, cfg.ID), "review-pr", cfg.HasBotRules()),
		Status:         webhooks.StatusAccepted,
		ReceivedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	body := strings.Replace(ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:05:00Z"), `"number": 7,`, `"number": 8,`, 2)
	body = strings.Replace(body, `"sha": "abc123"`, `"sha": "def456"`, 1)
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("re-request on a fresh accepted row must collapse: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// R35dde4, order independence: when the auto-request outruns the `opened`
// delivery, it claims the ordinary per-head key — so the late open dedupes
// against it instead of launching a second review of the same head.
func TestGitHubWebhook_ReviewRequestedBeforeOpenClaimsHeadKey(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-1", nil
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}

	// Reordered pair: the auto-request arrives first, on an unclaimed head —
	// it launches the review under the per-head key.
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("re-request on an unclaimed head must launch: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}

	// The late `opened` for the same head dedupes against that claim.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if calls != 1 {
		t.Fatalf("late open must dedupe against the re-request's claim: calls=%d body=%s", calls, w.Body.String())
	}
}

// The collapse DEFERS to an explicit `overlap: supersede` (the operator's
// "newest request wins"): a click during a live review salts as before, the
// launch tail cancels the stale run, and the fresh one replaces it — instead
// of the click being silently dropped.
func TestGitHubWebhook_ReviewRequestedSupersedeSkipsCollapse(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return fmt.Sprintf("run-%d", calls), nil
	}
	var cancelled []string
	s.webhookCancelRun = func(runID string) error { cancelled = append(cancelled, runID); return nil }
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	s.webhookRunIsLive = func(_ context.Context, runID string) bool { return runID == "run-1" }
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}
	cfg.Overlap = "supersede"

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("open: code=%d calls=%d", w.Code, calls)
	}

	// Click while run-1 is live: NOT collapsed — superseded and relaunched.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-02T08:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 2 {
		t.Fatalf("supersede click must relaunch: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	found := false
	for _, id := range cancelled {
		if id == "run-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the stale run must be superseded (cancelled): %v", cancelled)
	}
}

// Rb9e7c9: the collapse is ALL-rules-in-flight. With two bots fanned out, one
// live run must not swallow the click for the bot whose review already
// finished — a single not-live rule declines the collapse and the whole
// delivery salts.
func TestGitHubWebhook_ReviewRequestedMixedFanoutRelaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return fmt.Sprintf("run-%d", calls), nil
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	// Only the first bot's run is still in flight.
	s.webhookRunIsLive = func(_ context.Context, runID string) bool { return runID == "run-1" }
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}
	cfg.BotIDs = []string{"review-pr", "second-bot"}
	cfg.BotRules = []webhooks.BotRule{
		{BotID: "review-pr", Events: []string{"pull_request"}},
		{BotID: "second-bot", Events: []string{"pull_request"}},
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 2 {
		t.Fatalf("open must fan out to both bots: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}

	// run-1 live, run-2 finished → the click must NOT collapse: both relaunch.
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-02T08:05:00Z"), prforge.EventHeaderPullRequest, pt))
	if calls != 4 {
		t.Fatalf("mixed fan-out click must salt and relaunch both: calls=%d body=%s", calls, w.Body.String())
	}
}

// flakyDeliveryStore fails the FIRST GetByIdempotencyKey with a generic (non
// not-found) error, then delegates — the shape of a transient store hiccup.
type flakyDeliveryStore struct {
	webhooks.DeliveryStore
	failedOnce bool
}

func (f *flakyDeliveryStore) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	if !f.failedOnce {
		f.failedOnce = true
		return webhooks.Delivery{}, fmt.Errorf("store hiccup")
	}
	return f.DeliveryStore.GetByIdempotencyKey(ctx, key)
}

// R1545ff: a transient store read error fails TOWARD the salted key (a
// possible duplicate review), never toward the per-head key where the launch
// tail dedupes the click into a silent no-op.
func TestGitHubWebhook_ReviewRequestedStoreErrorSalts(t *testing.T) {
	s := newWebhookTestServer(t)
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-9", nil
	}
	s.webhookPRForgeReviewRequestGate = func(context.Context, webhooks.Config, prforge.Parsed, string) (bool, string, error) {
		return true, "allowlist", nil
	}
	s.webhookRunIsLive = func(context.Context, string) bool { return false }
	cfg, pt := ghConfig(t, s)
	cfg.ReviewRequestLogins = []string{"iterion-bot"}

	// A finished review already claimed the per-head key.
	headBase := fmt.Sprintf("gh|%s|%s|acme/widgets|7|abc123", cfg.TenantID, cfg.ID)
	if err := s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
		ID: "prior", TenantID: cfg.TenantID, WebhookID: cfg.ID, Provider: cfg.Provider,
		IdempotencyKey: forgeIdemKey(headBase, "review-pr", cfg.HasBotRules()),
		Status:         webhooks.StatusLaunched, RunID: "run-0",
		ReceivedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The claim probe's read fails once (transient), then the store recovers.
	s.webhookDeliveries = &flakyDeliveryStore{DeliveryStore: s.webhookDeliveries}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("alice", "iterion-bot", "2026-09-02T08:10:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("store hiccup must salt and launch, not dedupe to a no-op: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// ghDeletedForkPR: a PR opened from a fork that has since been DELETED.
// GitHub keeps the pull request and nulls `head.repo`, so the payload is
// byte-identical to one that simply never carried the field — while the
// head ref is still a name the fork author chose. The unattended lanes
// launch on `<base>.CloneURL + head ref`, so admitting this aims the bot
// at the BASE repo's branch of that name.
const ghDeletedForkPR = `{
  "action": "opened", "number": 9,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 9, "title": "Add subtract", "body": "Implements subtraction.", "draft": false,
    "html_url": "https://github.com/acme/widgets/pull/9", "state": "open",
    "head": {"ref": "main", "sha": "aaa111", "repo": null},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}},
  "sender": {"login": "mallory"}
}`

// The fork guard is fail-CLOSED on the unattended lane: a head repo the
// payload does not name is never treated as same-repo. `head.repo: null`
// is exactly the shape a fork takes once deleted, so reading it as "not a
// fork" admitted the one case that most needed gating — and the refusal
// must say WHICH state it refused, or an operator debugging a filtered
// internal PR goes hunting for a fork that does not exist.
func TestGitHubWebhook_DeletedForkPRIsNotAutoLaunched(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghDeletedForkPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("a PR whose head repo the payload does not name must NOT auto-launch — the launch pair would be <base>.CloneURL + a fork-chosen branch; launched=%d", launched)
	}
	ds, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 10)
	if err != nil || len(ds) == 0 {
		t.Fatalf("no delivery recorded: %v %d", err, len(ds))
	}
	if !strings.Contains(ds[0].Error, "head repo withheld") {
		t.Fatalf("the refusal must name the state it refused (a withheld head, not \"fork PR\"), got %q", ds[0].Error)
	}
}
