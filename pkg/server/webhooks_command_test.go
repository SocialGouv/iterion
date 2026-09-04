package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// glNoteCmd is an MR note carrying a generic slash-command with args.
const glNoteFeaturly = `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "user": {"username": "alice"},
  "object_attributes": {"id": 99, "note": "/featurly add an export endpoint", "noteable_type": "MergeRequest", "discussion_id": "d-1", "author_id": 1},
  "merge_request": {"iid": 7, "state": "opened", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "headsha"}}
}`

func featurlyConfig() webhooks.Config {
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	return cfg
}

// TestGitLabNoteHook_GenericCommandLaunches pins the universal slash-command
// path: /featurly <spec> on an MR note resolves through the CommandMap to
// feature-dev, the args land in the route's args_var, and the bot launches.
func TestGitLabNoteHook_GenericCommandLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot string
	var gotVars map[string]string
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-feat-1", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(featurlyConfig()), glNoteFeaturly))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotBot != "feature-dev" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["feature_prompt"] != "add an export endpoint" {
		t.Fatalf("args should land in feature_prompt: %v", gotVars["feature_prompt"])
	}
	if gotVars["scope_notes"] == "" || gotVars["pr_url"] == "" {
		t.Fatalf("PR context vars missing: %v", gotVars)
	}
}

// TestGitLabNoteHook_UnknownCommandFiltered: a command no bot claims (on a
// non-wildcard webhook) is filtered with 200, never launched, never 4xx.
func TestGitLabNoteHook_UnknownCommandFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "ok", nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "x", nil
	}
	body := `{"object_kind":"note","project":{"id":42,"path_with_namespace":"acme/widgets"},"user":{"username":"alice"},"object_attributes":{"id":99,"note":"/bogus do something","noteable_type":"MergeRequest","discussion_id":"d-1","author_id":1},"merge_request":{"iid":7,"state":"opened","target_branch":"main","title":"X","url":"https://gitlab.com/acme/widgets/-/merge_requests/7"}}`
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(featurlyConfig()), body))
	if w.Code != http.StatusOK {
		t.Fatalf("unknown command should be filtered 200, got %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("unknown command must not launch, calls=%d", calls)
	}
}

// TestGitLabNoteHook_CommandUnauthorizedFiltered: a denied replier filters
// (200) without launching.
func TestGitLabNoteHook_CommandUnauthorizedFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return false, "replier not authorized", nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "x", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(featurlyConfig()), glNoteFeaturly))
	if w.Code != http.StatusOK {
		t.Fatalf("unauthorized should be filtered 200, got %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("unauthorized must not launch, calls=%d", calls)
	}
}

const ghIssueCommentFeaturly = `{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "issue": {"number": 7, "title": "Add X", "body": "desc", "state": "open",
    "pull_request": {"html_url": "https://github.com/acme/widgets/pull/7"}},
  "comment": {"id": 555, "body": "/featurly add export endpoint"},
  "sender": {"login": "alice"}
}`

// TestGitHubIssueComment_GenericCommandLaunches pins the universal command
// path on GitHub: /featurly <spec> in a PR comment routes through the
// CommandMap to feature-dev with the args in its args_var.
func TestGitHubIssueComment_GenericCommandLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	var calls int
	var gotBot, gotRef string
	var gotVars map[string]string
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feat/export", TargetBranch: "main", Author: "alice"}, nil
	}
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, repoRef, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotRef = botID, vars, repoRef
		return "run-gh-1", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotBot != "feature-dev" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["feature_prompt"] != "add export endpoint" {
		t.Fatalf("args should land in feature_prompt: %v", gotVars["feature_prompt"])
	}
	// The resolved PR threads the checkout ref + branch vars into the launch.
	if gotRef != "feat/export" {
		t.Fatalf("repoRef should be the PR head branch, got %q", gotRef)
	}
	for k, want := range map[string]string{"base_ref": "main", "target_branch": "main", "source_branch": "feat/export", "pr_author": "alice"} {
		if gotVars[k] != want {
			t.Fatalf("vars[%s]=%q want %q", k, gotVars[k], want)
		}
	}
}

// TestGitHubIssueComment_BillyPushBackStamped: /billy on a PR comment must
// get the SAME push-back semantics as the pull_request-event path — without
// open_mr/push_branch the bot banks its commits on the storage branch and
// the PR never receives them.
func TestGitHubIssueComment_BillyPushBackStamped(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"billy": {{BotID: "branch-improve-loop", ArgsVar: "scope_notes", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "dependabot/go_modules/bump", TargetBranch: "main"}, nil
	}
	var gotVars map[string]string
	var gotRef string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, repoRef, _ string, _, _ map[string]string) (string, error) {
		gotVars, gotRef = vars, repoRef
		return "run-billy-1", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"bump deps","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":556,"body":"/billy fix the drift"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotRef != "dependabot/go_modules/bump" {
		t.Fatalf("repoRef=%q want the PR head branch", gotRef)
	}
	if gotVars["open_mr"] != "false" || gotVars["push_branch"] != "dependabot/go_modules/bump" {
		t.Fatalf("push-back vars not stamped: open_mr=%q push_branch=%q", gotVars["open_mr"], gotVars["push_branch"])
	}
	if gotVars["scope_notes"] != "fix the drift" {
		t.Fatalf("args should land in scope_notes: %q", gotVars["scope_notes"])
	}
}

// TestGitHubIssueComment_BillyPushBackAsPR: with BranchImproveAsPR the same
// command opens a separate hardening PR instead of pushing in-place.
func TestGitHubIssueComment_BillyPushBackAsPR(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"branch-improve-loop"}
	cfg.BranchImproveAsPR = true
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"billy": {{BotID: "branch-improve-loop", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feat/x", TargetBranch: "main"}, nil
	}
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-billy-2", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":557,"body":"/billy"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotVars["open_mr"] != "true" || gotVars["mr_base"] != "feat/x" {
		t.Fatalf("as-PR vars not stamped: open_mr=%q mr_base=%q", gotVars["open_mr"], gotVars["mr_base"])
	}
	if _, ok := gotVars["push_branch"]; ok {
		t.Fatalf("push_branch must not be set in as-PR mode")
	}
}

// TestGitHubIssueComment_BillyBoardCardCarriesPRContext: a board-mode /billy
// launches from the card's BotArgs ONLY (cloud coordinator), so the resolved
// PR context — push_branch, pr_url, base_ref — must ride BotArgs or the run
// falls back to DSL defaults and strands its commits off-PR.
func TestGitHubIssueComment_BillyBoardCardCarriesPRContext(t *testing.T) {
	s := newWebhookTestServer(t)
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"billy": {{BotID: "branch-improve-loop", Mode: "board", ArgsVar: "scope_notes", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "dependabot/go_modules/bump", TargetBranch: "main", Author: "dependabot[bot]"}, nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "run-board-billy", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"bump deps","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":558,"body":"/billy fix it"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	cards, err := boardStore.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	got := cards[0].BotArgs
	for k, want := range map[string]string{
		"push_branch": "dependabot/go_modules/bump",
		"open_mr":     "false",
		"base_ref":    "main",
		"pr_url":      "https://github.com/acme/widgets/pull/7",
		"pr_author":   "dependabot[bot]",
		"scope_notes": "fix it",
	} {
		if got[k] != want {
			t.Fatalf("card BotArgs[%s]=%q want %q (all: %v)", k, got[k], want, got)
		}
	}
}

// TestGitHubIssueComment_PRResolutionFailureIsVisible: when the PR head
// cannot be resolved, the command must NOT silently launch on the default
// branch (the run would diff nothing and no-op) — it fails loudly as a
// launch error.
func TestGitHubIssueComment_PRResolutionFailureIsVisible(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{}, fmt.Errorf("forge unreachable")
	}
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "x", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("resolution failure must be a visible 502, got %d body=%s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("must not launch on the default branch, calls=%d", calls)
	}
}

// TestGitHubIssueComment_ClosedPRFiltered: a command on a PR that is no
// longer open (merged/closed since the payload snapshot) is filtered — a
// launch would churn on a stale or deleted branch.
func TestGitHubIssueComment_ClosedPRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "merged", SourceBranch: "feat/export", TargetBranch: "main"}, nil
	}
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "x", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("closed PR should be filtered 200, got %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("closed PR must not launch, calls=%d", calls)
	}
}

// TestGitHubIssueComment_UnknownCommandFiltered: a non-command comment is
// filtered 200 (so GitHub does not disable the hook) and never launches.
func TestGitHubIssueComment_PlainCommentFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "x", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets"},"issue":{"number":7,"state":"open","pull_request":{"html_url":"x"}},"comment":{"id":1,"body":"lgtm, thanks!"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("plain comment should be filtered 200, got %d", w.Code)
	}
	if calls != 0 {
		t.Fatalf("plain comment must not launch, calls=%d", calls)
	}
}

// TestGitLabNoteHook_BoardModeCreatesCard: a board-mode command on a cloud
// board materialises exactly one tracked card (idempotent across retries),
// assigned to the bot with the args in bot_args, and still launches the run.
func TestGitLabNoteHook_BoardModeCreatesCard(t *testing.T) {
	s := newWebhookTestServer(t)
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	var launches int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		return "run-board-1", nil
	}

	s.handleGitLabWebhook(httptest.NewRecorder(), glNoteReq(gitlabCtx(featurlyConfig()), glNoteFeaturly))

	cards, err := boardStore.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("want exactly 1 board card, got %d", len(cards))
	}
	c := cards[0]
	if c.Bot != "feature-dev" || c.Assignee != "feature-dev" {
		t.Errorf("card should be assigned to the bot: %+v", c)
	}
	if c.BotArgs["feature_prompt"] != "add an export endpoint" {
		t.Errorf("card bot_args should carry the command args: %+v", c.BotArgs)
	}
	if launches != 1 {
		t.Errorf("the run should still launch, launches=%d", launches)
	}

	// Retry the same comment → no duplicate card (idempotent on the comment id).
	s.handleGitLabWebhook(httptest.NewRecorder(), glNoteReq(gitlabCtx(featurlyConfig()), glNoteFeaturly))
	cards2, _ := boardStore.List(native.ListFilter{})
	if len(cards2) != 1 {
		t.Errorf("retry must not duplicate the card, got %d", len(cards2))
	}
}

// glNoteFeaturlyIssue is a /featurly command on an OPEN ISSUE note (no
// merge_request block) — the open-MR-and-back-link surface. The project carries
// a default_branch (the MR base) and the issue its own url (the back-link).
const glNoteFeaturlyIssue = `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git", "default_branch": "main"},
  "user": {"username": "alice"},
  "object_attributes": {"id": 99, "note": "/featurly add an export endpoint", "noteable_type": "Issue", "author_id": 1, "url": "https://gitlab.com/acme/widgets/-/notes/99"},
  "issue": {"iid": 12, "title": "Add X", "description": "desc", "state": "opened", "url": "https://gitlab.com/acme/widgets/-/issues/12"}
}`

func featurlyIssueConfig() webhooks.Config {
	cfg := glConfig()
	cfg.BotIDs = []string{"feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any", OpensMR: true}},
	}
	return cfg
}

// TestGitLabIssueNote_BoardCardStampsOpenMR pins the central new path: a
// /featurly command on an ISSUE note routes (surface="issue"), materialises a
// board card, and — because the command declares opens_mr — stamps open_mr +
// source_issue_ref=<the issue's own URL> into the card's bot_args, so the bot
// opens an MR and back-links the very issue the human commented on.
func TestGitLabIssueNote_BoardCardStampsOpenMR(t *testing.T) {
	s := newWebhookTestServer(t)
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	var launches int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launches++
		return "run-issue-1", nil
	}

	s.handleGitLabWebhook(httptest.NewRecorder(), glNoteReq(gitlabCtx(featurlyIssueConfig()), glNoteFeaturlyIssue))

	cards, err := boardStore.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("want exactly 1 board card, got %d", len(cards))
	}
	c := cards[0]
	if c.Bot != "feature-dev" {
		t.Errorf("card should be assigned to feature-dev: %+v", c)
	}
	if c.BotArgs["feature_prompt"] != "add an export endpoint" {
		t.Errorf("command args should land in feature_prompt: %+v", c.BotArgs)
	}
	if c.BotArgs["open_mr"] != "true" {
		t.Errorf("opens_mr command must stamp open_mr=true: %+v", c.BotArgs)
	}
	if got := c.BotArgs["source_issue_ref"]; got != "https://gitlab.com/acme/widgets/-/issues/12" {
		t.Errorf("source_issue_ref should be the issue URL (back-link target), got %q", got)
	}
	if launches != 1 {
		t.Errorf("the run should still launch, launches=%d", launches)
	}
}

// TestGitLabIssueNote_NonOpensMRNoStamp: a board command WITHOUT opens_mr (e.g.
// a read-only reviewer) must NOT receive the open_mr / source_issue_ref stamp,
// so unrelated board commands are unaffected.
func TestGitLabIssueNote_NonOpensMRNoStamp(t *testing.T) {
	s := newWebhookTestServer(t)
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "run-x", nil
	}
	cfg := featurlyIssueConfig()
	cfg.CommandMap["featurly"][0].OpensMR = false // read-only command
	s.handleGitLabWebhook(httptest.NewRecorder(), glNoteReq(gitlabCtx(cfg), glNoteFeaturlyIssue))

	cards, _ := boardStore.List(native.ListFilter{})
	if len(cards) != 1 {
		t.Fatalf("want 1 card, got %d", len(cards))
	}
	if _, ok := cards[0].BotArgs["open_mr"]; ok {
		t.Errorf("non-opens_mr command must not stamp open_mr: %+v", cards[0].BotArgs)
	}
}

// TestGitHubIssueComment_BillyUnauthorizedRejected: `/billy` from a commenter
// who fails the authorization gate (no repo write) is filtered (200) and NEVER
// launches the mutating branch-improve loop (req 3 — the authz gate is
// mandatory, reused verbatim from the /revi/command path).
func TestGitHubIssueComment_BillyUnauthorizedRejected(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"billy": {{BotID: "branch-improve-loop", ArgsVar: "scope_notes", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return false, "replier not authorized: mallory", nil
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "x", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":561,"body":"/billy fix it"},"sender":{"login":"mallory"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("unauthorized /billy must be filtered 200, got %d", w.Code)
	}
	if launched != 0 {
		t.Fatalf("unauthorized /billy must NOT launch Billy, launched=%d", launched)
	}
}

// TestGitHubIssueComment_SeedsDeclaredReviewConsumer: an authorized command on
// a PR that was already reviewed carries that review into the run.
//
// Which bot receives it, and into which var, comes from the LAUNCHED BOT'S OWN
// MANIFEST (`consumes: kind: review`) — so the fixture bot here is deliberately
// not any shipped bot: nothing in the engine may know that a particular bot is
// the one that fixes reviews. The counterpart assertion, that the shipped
// reviewer/fixer pair really declares the two halves, lives in bots/.
func TestGitHubIssueComment_SeedsDeclaredReviewConsumer(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"fixer-bot"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"fixit": {{BotID: "fixer-bot", ArgsVar: "scope_notes", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feat/x", TargetBranch: "main", HeadSHA: "cafe1234cafe1234"}, nil
	}
	var gotPRURL string
	s.webhookHandoff = func(_ context.Context, _ webhooks.Config, _ bundle.HandoffKind, q handoffQuery) string {
		gotPRURL = q.PRURL
		// The PR's live head must reach the lookup: a review of an older head is
		// still worth handing over, but only labelled as such.
		if q.HeadSHA != "cafe1234cafe1234" {
			t.Errorf("prior-review lookup head sha = %q, want the resolved PR head", q.HeadSHA)
		}
		return "Prior review of this PR: 1 finding\n- [high/security] SQLi (db.go:42)"
	}
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-seeded", nil
	}
	body := `{"action":"created","repository":{"full_name":"acme/widgets","clone_url":"https://github.com/acme/widgets.git"},"issue":{"number":7,"title":"t","body":"","state":"open","pull_request":{"html_url":"https://github.com/acme/widgets/pull/7"}},"comment":{"id":562,"body":"/fixit"},"sender":{"login":"alice"}}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if gotPRURL != "https://github.com/acme/widgets/pull/7" {
		t.Fatalf("prior-review lookup used pr_url=%q", gotPRURL)
	}
	if !strings.Contains(gotVars["prior_review"], "SQLi") {
		t.Fatalf("the declared var must carry the review, got %q", gotVars["prior_review"])
	}
}

// TestPriorReviewSkipsABotThatDidNotAskForIt: a bot with no `consumes` block is
// launched untouched. The seed is a declared appetite, never something the
// engine decides a bot ought to want.
func TestPriorReviewSkipsABotThatDidNotAskForIt(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = t.TempDir() // no bundles at all
	called := false
	s.webhookHandoff = func(context.Context, webhooks.Config, bundle.HandoffKind, handoffQuery) string {
		called = true
		return "a review"
	}
	vars := map[string]string{}
	s.stampHandoffs(context.Background(), webhooks.Config{}, "some-bot", vars, handoffQuery{PRURL: "https://f/o/r/pull/1"})
	if called {
		t.Error("looked up a review for a bot that never declared it consumes one")
	}
	if len(vars) != 0 {
		t.Errorf("stamped %v onto a bot that asked for nothing", vars)
	}
}

// writeConsumerBotFixture creates a minimal bundle declaring that it consumes a
// review into varName, and returns the workdir holding it.
func writeConsumerBotFixture(t *testing.T, name, varName string) string {
	t.Helper()
	workDir := t.TempDir()
	dir := filepath.Join(workDir, "bots", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + name + "\nschema_version: 1\nconsumes:\n  - kind: review\n    var: " + varName + "\n    scope: pr\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	bot := "vars:\n  " + varName + ": string = \"\"\n\nagent noop:\n  system: \"noop\"\n\nworkflow w:\n  entry: noop\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(bot), 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir
}

// TestGitHubIssueComment_ForkPRFiltered pins the fork guard on the /command
// lane (#642). Class surfaced by Revi on #626 (finding R0b5042, fixed there
// for the reply-in-thread lane only). The gap: handlePRForgeComment resolves
// the PR via GetPullRequest → forge.PullRef, and the launch pair (base repo
// CloneURL + PR head ref) does NOT name one repository on a fork PR — the
// head ref lives in the head repo, so the checkout misses (or, worse, hits
// a same-named branch on the base and the bot answers grounded in the wrong
// code, under the bot identity). Reachable by any repo collaborator
// commenting `/revi` or `/billy` on a fork PR.
//
// The PR auto-review lane has an unconditional fork guard (webhooks_github.go
// IsCrossRepo on the parsed payload), the review-thread reply lane got one in
// #626 — this test locks the command lane down the same way.
func TestGitHubIssueComment_ForkPRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	// The resolver returns a fork PR: HeadRepoFullName differs from the
	// base repo. The command handler must filter it as a fork (200) and
	// launch nothing.
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{
			Number: 7, State: "open",
			SourceBranch: "feat/x", TargetBranch: "main",
			HeadRepoFullName: "mallory/widgets", // fork of acme/widgets
		}, nil
	}
	var launched int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-forbidden", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("fork PR command must filter 200, got %d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("fork PR must NOT launch (launched=%d — a bot answering fork code under the bot identity)", launched)
	}
}

// A same-repo PR must STILL launch (regression guard: the fork check must not
// filter internal-branch PRs whose head lives in the base repo).
func TestGitHubIssueComment_SameRepoStillLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{
			Number: 7, State: "open",
			SourceBranch: "feat/x", TargetBranch: "main",
			HeadRepoFullName: "acme/widgets", // same repo (owner/repo matches base)
		}, nil
	}
	var launched int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-ok", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("same-repo command must launch (202), got %d body=%s", w.Code, w.Body.String())
	}
	if launched != 1 {
		t.Fatalf("same-repo command must launch exactly once, got %d", launched)
	}
}

// TestGitHubIssueComment_EmptyHeadRepoFailsClosed pins the deleted-fork /
// legacy-payload fail-CLOSED semantics (B1): both GitHub and Forgejo emit
// `head.repo: null` when the head repo no longer exists, which is ALWAYS a
// fork. An earlier IsCrossRepo-only guard treated empty HeadRepoFullName as
// safe, letting the fixer launch with repoURL=<base> repoRef=main and push
// LLM commits to the BASE repo's main. SameRepoAs is false on empty head,
// so the command lane now refuses.
func TestGitHubIssueComment_EmptyHeadRepoFailsClosed(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (bool, string, error) {
		return true, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", SourceBranch: "feat/x", TargetBranch: "main"}, nil
	}
	var launched int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-forbidden", nil
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghIssueCommentFeaturly, prforge.EventHeaderIssueComment, pt))
	if launched != 0 {
		t.Fatalf("empty HeadRepoFullName is a deleted-fork signal — MUST NOT launch (launched=%d)", launched)
	}
}
