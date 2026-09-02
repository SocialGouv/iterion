package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// ghReviewCommentReply builds a pull_request_review_comment payload: `author`
// replies inside the thread rooted at comment 9001 on open PR #7.
func ghReviewCommentReply(author, body string) string {
	return fmt.Sprintf(`{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "comment": {"id": 9002, "in_reply_to_id": 9001, "body": %q,
    "html_url": "https://github.com/acme/widgets/pull/7#discussion_r9002",
    "path": "pkg/x/y.go", "user": {"login": %q}},
  "pull_request": {"number": 7, "state": "open", "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "head": {"sha": "abc123", "ref": "feature/x"}, "base": {"ref": "main"}},
  "sender": {"login": %q}
}`, body, author, author)
}

// The happy path: an authorized human replies in one of the bot's review
// threads → the converse bot launches with the reply as the question, the
// thread transcript, and the thread ROOT id as discussion_id (what GitHub's
// reply endpoint wants).
func TestGitHubWebhook_ReviewThreadReplyLaunchesConverse(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var gotBot string
	var gotVars map[string]string
	calls := 0
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-rt-1", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		return true, "revi (2026-09-02):\nthe SSRF is reachable\n---\n", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"} // post-fix provisioned shape

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "why is this SSRF reachable?"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("reply: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gotBot != "revi-converse" {
		t.Fatalf("expected revi-converse, got %q", gotBot)
	}
	if gotVars["converse_question"] != "why is this SSRF reachable?" {
		t.Fatalf("converse_question: %v", gotVars["converse_question"])
	}
	if gotVars["discussion_id"] != "9001" {
		t.Fatalf("discussion_id must be the THREAD ROOT id: %v", gotVars["discussion_id"])
	}
	if !strings.Contains(gotVars["thread_context"], "the SSRF is reachable") {
		t.Fatalf("thread_context not threaded: %v", gotVars["thread_context"])
	}
	if gotVars["pr_url"] != "https://github.com/acme/widgets/pull/7" || gotVars["head_sha"] != "abc123" || gotVars["base_ref"] != "main" {
		t.Fatalf("PR context vars: %v", gotVars)
	}
	if _, present := gotVars["re_review"]; present {
		t.Fatalf("re_review must not be set on the converse path: %v", gotVars)
	}
}

// The bot's own reply echoes back as the same event — the loop-guard must
// filter it BEFORE any forge I/O, or the conversation ping-pongs forever.
func TestGitHubWebhook_ReviewThreadReplyLoopGuard(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return strings.EqualFold(login, "iterion-bot")
	}
	gateCalled := false
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		gateCalled = true
		return true, "", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("iterion-bot", "here is my answer"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("self reply must filter: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gateCalled {
		t.Fatal("loop-guard must run BEFORE the gate (no forge I/O on the bot's own echo)")
	}
}

// A reply in a human↔human thread (no bot comment in it) must not trigger:
// the gate classifies and refuses, the delivery is filtered.
func TestGitHubWebhook_ReviewThreadReplyNotBotThread(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		return false, "", "not a bot review thread (no iterion comment in it)", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "ping @bob thoughts?"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("human thread must filter: code=%d calls=%d", w.Code, calls)
	}
}

// Without the converse bot in the webhook scope the lane is inert — the
// reply filters instead of falling back to a re-review (a reply is a
// question; answering it with a fresh full review would be wrong).
func TestGitHubWebhook_ReviewThreadReplyConverseNotEnabled(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s) // BotIDs = ["review-pr"] only

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "question?"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("converse-less webhook must filter: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// A reply on a closed PR filters — nothing to converse about.
func TestGitHubWebhook_ReviewThreadReplyClosedPR(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}

	body := strings.Replace(ghReviewCommentReply("alice", "question?"), `"state": "open"`, `"state": "closed"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("closed PR must filter: code=%d calls=%d", w.Code, calls)
	}
}

// `/revi <question>` in a PR comment routes to the converse bot through the
// GENERIC command registry — the manifests' complementary disambiguators
// (review-pr when_args_empty / revi-converse when_args_present), no
// bot-specific branch in the handler. The CommandMap here is exactly what
// the orchestrator derives from those manifests at provision.
func TestGitHubWebhook_ReviQuestionRoutesViaCommandRegistry(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var gotBot string
	var gotVars map[string]string
	calls := 0
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-q-1", nil
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "allowlist", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", SourceBranch: "feature/x", TargetBranch: "main", HeadSHA: "abc123"}, nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"revi": {
			{BotID: "review-pr", Mode: "direct", Scope: "pr", Disambiguator: "when_args_empty"},
			{BotID: "revi-converse", Mode: "direct", Scope: "pr", Disambiguator: "when_args_present", ArgsVar: "converse_question"},
		},
	}

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
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("revi question: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gotBot != "revi-converse" {
		t.Fatalf("expected revi-converse via the registry, got %q", gotBot)
	}
	if gotVars["converse_question"] != "why is the SSRF critical?" {
		t.Fatalf("args_var must carry the question: %v", gotVars["converse_question"])
	}
}

// A config provisioned BEFORE the lane (allowlist without the review-comment
// event) stays inert until re-provisioned — pins the migration semantics.
func TestGitHubWebhook_ReviewThreadReplyPreLaneConfigInert(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	calls := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request"} // pre-lane shape

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewCommentReply("alice", "question?"), prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("pre-lane config must filter until re-provision: code=%d calls=%d", w.Code, calls)
	}
}

// Every inline comment of a bot review echoes back as a thread-OPENING
// pull_request_review_comment (no in_reply_to): nobody can already be in a
// thread this comment creates, so the lane filters it from the payload
// alone — no store read, no forge fetch, the gate is never invoked.
func TestGitHubWebhook_ReviewThreadTopLevelCommentFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	launches, gates := 0, 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		return "run-x", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		gates++
		return true, "", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"}

	body := `{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "comment": {"id": 9001, "body": "inline note on a diff line",
    "html_url": "https://github.com/acme/widgets/pull/7#discussion_r9001",
    "path": "pkg/x/y.go", "user": {"login": "alice"}},
  "pull_request": {"number": 7, "state": "open", "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "head": {"sha": "abc123", "ref": "feature/x"}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || launches != 0 || gates != 0 {
		t.Fatalf("thread-opening comment must filter payload-only: code=%d launches=%d gates=%d", w.Code, launches, gates)
	}
}

// fakeThreadAPI drives reviewReplyGateWithAPI without a forge.
type fakeThreadAPI struct {
	comments  []forge.PRReviewComment
	listErr   error
	perm      string
	permErr   error
	permCalls int
}

func (f *fakeThreadAPI) WhoAmI(context.Context) (forge.Identity, error) {
	return forge.Identity{}, nil
}
func (f *fakeThreadAPI) CollaboratorPermission(context.Context, string, string) (string, error) {
	f.permCalls++
	return f.perm, f.permErr
}
func (f *fakeThreadAPI) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{}, nil
}
func (f *fakeThreadAPI) ListPRReviewComments(context.Context, string, int) ([]forge.PRReviewComment, error) {
	return f.comments, f.listErr
}

// The REAL gate core (not the handler stub): thread-membership filter,
// bot-in-thread classification, allowlist vs role authorization, and error
// propagation, on a fake thread API.
func TestReviewReplyGateWithAPI(t *testing.T) {
	newGateServer := func(t *testing.T) *Server {
		s := newWebhookTestServer(t)
		s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
			return login == "revi-bot"
		}
		return s
	}
	p := prforge.ParsedReviewComment{ProjectPath: "acme/widgets", PRNumber: 7, CommentID: 9002, ThreadRootID: 9001, AuthorLogin: "alice"}
	botThread := []forge.PRReviewComment{
		{ID: 9001, Author: "revi-bot", Body: "the SSRF is reachable", CreatedAt: "2026-09-02T10:00:00Z"},
		{ID: 9002, InReplyTo: 9001, Author: "alice", Body: "why?", CreatedAt: "2026-09-02T10:05:00Z"},
		{ID: 777, Author: "mallory", Body: "unrelated thread", CreatedAt: "2026-09-02T09:00:00Z"},
	}

	t.Run("allowlist authorizes without a role probe", func(t *testing.T) {
		s := newGateServer(t)
		api := &fakeThreadAPI{comments: botThread}
		ok, transcript, reason, err := s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{AuthorizedRepliers: []string{"alice"}}, p, api)
		if err != nil || !ok || reason != "allowlist" {
			t.Fatalf("ok=%v reason=%q err=%v", ok, reason, err)
		}
		if api.permCalls != 0 {
			t.Fatalf("allowlist path must not probe the role: %d calls", api.permCalls)
		}
		if !strings.Contains(transcript, "revi-bot (you, the bot)") || !strings.Contains(transcript, "the SSRF is reachable") {
			t.Fatalf("transcript must label the bot's anchor: %q", transcript)
		}
		if strings.Contains(transcript, "unrelated thread") {
			t.Fatalf("transcript leaked another thread: %q", transcript)
		}
	})

	t.Run("role gate", func(t *testing.T) {
		s := newGateServer(t)
		ok, _, reason, err := s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{MinReplierRole: "developer"}, p, &fakeThreadAPI{comments: botThread, perm: "write"})
		if err != nil || !ok || reason != "role" {
			t.Fatalf("write>=developer must pass: ok=%v reason=%q err=%v", ok, reason, err)
		}
		ok, _, reason, err = s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{MinReplierRole: "developer"}, p, &fakeThreadAPI{comments: botThread, perm: "read"})
		if err != nil || ok || !strings.HasPrefix(reason, "replier not authorized") {
			t.Fatalf("read<developer must refuse: ok=%v reason=%q err=%v", ok, reason, err)
		}
	})

	t.Run("human-only thread never triggers", func(t *testing.T) {
		s := newGateServer(t)
		api := &fakeThreadAPI{comments: []forge.PRReviewComment{
			{ID: 9001, Author: "bob", Body: "top-level human note"},
			{ID: 9002, InReplyTo: 9001, Author: "alice", Body: "why?"},
		}}
		ok, _, reason, err := s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{AuthorizedRepliers: []string{"alice"}}, p, api)
		if err != nil || ok || !strings.HasPrefix(reason, "not a bot review thread") {
			t.Fatalf("ok=%v reason=%q err=%v", ok, reason, err)
		}
		if api.permCalls != 0 {
			t.Fatalf("thread gate must refuse before any authz probe: %d calls", api.permCalls)
		}
	})

	t.Run("infra errors propagate", func(t *testing.T) {
		s := newGateServer(t)
		if _, _, _, err := s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{}, p, &fakeThreadAPI{listErr: fmt.Errorf("boom")}); err == nil {
			t.Fatal("list error must propagate, not filter")
		}
		if _, _, _, err := s.reviewReplyGateWithAPI(context.Background(), webhooks.Config{}, p, &fakeThreadAPI{comments: botThread, permErr: fmt.Errorf("boom")}); err == nil {
			t.Fatal("permission error must propagate, not filter")
		}
	})
}

// classifyReviewThread invariants the gate's security half rests on: the
// trigger comment can never self-certify the thread as the bot's, and the
// transcript obeys the shared anchor+newest cap.
func TestClassifyReviewThread(t *testing.T) {
	isBot := func(login string) bool { return login == "revi-bot" }
	p := prforge.ParsedReviewComment{CommentID: 9002, ThreadRootID: 9001}

	t.Run("trigger comment never counts as the bot", func(t *testing.T) {
		botInThread, _ := classifyReviewThread([]forge.PRReviewComment{
			{ID: 9001, Author: "bob", Body: "human root"},
			{ID: 9002, InReplyTo: 9001, Author: "revi-bot", Body: "trigger itself"},
		}, p, isBot)
		if botInThread {
			t.Fatal("a bot-authored trigger must not certify its own thread")
		}
	})

	t.Run("transcript capped by the shared budget", func(t *testing.T) {
		big := strings.Repeat("x", 4000)
		comments := []forge.PRReviewComment{{ID: 9001, Author: "revi-bot", Body: "anchor: " + big}}
		for i := int64(0); i < 6; i++ {
			comments = append(comments, forge.PRReviewComment{ID: 9100 + i, InReplyTo: 9001, Author: "alice", Body: big})
		}
		botInThread, transcript := classifyReviewThread(comments, p, isBot)
		if !botInThread {
			t.Fatal("bot anchor must certify the thread")
		}
		if len(transcript) > maxThreadContextChars {
			t.Fatalf("transcript exceeds the shared cap: %d > %d", len(transcript), maxThreadContextChars)
		}
		if !strings.Contains(transcript, "earlier notes omitted") {
			t.Fatalf("over-budget transcript must carry the omission marker")
		}
		if !strings.HasPrefix(transcript, "revi-bot (you, the bot)") {
			t.Fatalf("the thread anchor must be kept first: %q", transcript[:80])
		}
	})
}

// A fork PR pairs the BASE repo's clone URL with a HEAD-repo ref — the
// launch would check out missing or wrong code — so the reply lane filters
// cross-repo PRs from the payload alone, before the gate.
func TestGitHubWebhook_ReviewThreadForkPRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	launches, gates := 0, 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		return "run-x", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		gates++
		return true, "", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"issue_comment", "pull_request", "pull_request_review_comment"}

	body := `{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "comment": {"id": 9002, "in_reply_to_id": 9001, "body": "why?", "user": {"login": "alice"}},
  "pull_request": {"number": 7, "state": "open", "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "head": {"sha": "abc123", "ref": "main", "repo": {"full_name": "mallory/widgets"}},
    "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusOK || launches != 0 || gates != 0 {
		t.Fatalf("fork reply must filter payload-only: code=%d launches=%d gates=%d", w.Code, launches, gates)
	}
}
