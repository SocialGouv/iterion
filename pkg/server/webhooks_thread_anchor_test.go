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

// The thread anchor vars (#3283 S5): the webhook lanes hand the converse
// bot the thread ROOT's diff position — path, old path, line, side,
// pre-image hint, commit — as the thread_anchor_* launch vars, so
// converse_scope can focus the answer's reads on the hunk the operator is
// replying to. Pinned here on every lane that reaches the bot:
//   - the GitHub/Forgejo review-thread reply (the payload carries the
//     position; a reply inherits its root's),
//   - the GitLab plain reply in a thread (the ROOT's position, from the
//     discussion fetch the gate already makes),
//   - the GitLab command inside a thread — `/revi <question>` typed as a
//     reply to Revi's inline comment (the same fetch; the arriving note's
//     own position covers a command that OPENED a diff thread).
// The six keys ride as one block, empty where there is no anchor — a
// consumer reads one shape, never an absent var.

const anchorVarsWant = "thread_anchor_path|thread_anchor_old_path|thread_anchor_line|thread_anchor_old_line|thread_anchor_side|thread_anchor_commit"

func assertAnchorBlock(t *testing.T, where string, gotVars map[string]string, want map[string]string) {
	t.Helper()
	for _, name := range strings.Split(anchorVarsWant, "|") {
		if _, present := gotVars[name]; !present {
			t.Errorf("%s: launch vars lack %s — the anchor block must ride whole (empty where absent): %v", where, name, gotVars)
			continue
		}
	}
	for k, wantVal := range want {
		if gotVars[k] != wantVal {
			t.Errorf("%s: %s = %q, want %q", where, k, gotVars[k], wantVal)
		}
	}
}

// A GitHub review-thread reply whose comment carries its thread's position:
// the anchor vars carry it normalized (side RIGHT → new, the commit, the
// pre-image hint).
func TestGitHubWebhookReviewThreadReplyCarriesTheAnchor(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-anchor-1", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		return true, "revi:\nthe SSRF is reachable\n", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"pull_request_review_comment"}
	body := `{
		"action": "created",
		"repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
		"comment": {"id": 9002, "in_reply_to_id": 9001, "body": "why is this reachable?",
			"html_url": "https://github.com/acme/widgets/pull/7#discussion_r9002",
			"path": "pkg/x/y.go", "line": 42, "original_line": 40,
			"side": "RIGHT", "commit_id": "c0ffee1", "user": {"login": "alice"}},
		"pull_request": {"number": 7, "state": "open", "title": "Add X", "body": "desc",
			"html_url": "https://github.com/acme/widgets/pull/7",
			"head": {"sha": "abc123", "ref": "feature/x", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"}},
		"sender": {"login": "alice"}
	}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("reply: code=%d body=%s", w.Code, w.Body.String())
	}
	assertAnchorBlock(t, "github reply", gotVars, map[string]string{
		"thread_anchor_path":     "pkg/x/y.go",
		"thread_anchor_old_path": "",
		"thread_anchor_line":     "42",
		"thread_anchor_old_line": "40",
		"thread_anchor_side":     "new",
		"thread_anchor_commit":   "c0ffee1",
	})
}

// The GitLab reply lane: the gate's discussion fetch names the ROOT's
// position (the reply itself carries none), and the vars carry it.
func TestGitLabWebhookReplyCarriesTheThreadRootAnchor(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-anchor-2", nil
	}
	s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, webhooks.ThreadAnchor, error) {
		return true, true, "the transcript", "reply",
			webhooks.ThreadAnchor{Path: "pkg/x/y.go", OldPath: "pkg/was/y.go", Line: 12, OldLine: 10, Side: "new", Commit: "h1"}, nil
	}
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "Can you expand?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("reply: code=%d body=%s", w.Code, w.Body.String())
	}
	assertAnchorBlock(t, "gitlab reply", gotVars, map[string]string{
		"thread_anchor_path":     "pkg/x/y.go",
		"thread_anchor_old_path": "pkg/was/y.go",
		"thread_anchor_line":     "12",
		"thread_anchor_old_line": "10",
		"thread_anchor_side":     "new",
		"thread_anchor_commit":   "h1",
	})
}

// The GitLab COMMAND lane — the command-in-a-thread entry. A `/revi
// <question>` typed as a reply inside a diff thread reaches this lane; the
// anchor block must ride even when nothing was fetched and the note names
// no position (empty keys), and the note's OWN position covers the
// fresh-inline-comment case.
func TestGitLabWebhookCommandCarriesTheAnchorBlock(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *map[string]string) {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.Bots.Paths = []string{botsDirAbs(t)}
		var gotVars map[string]string
		s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
			gotVars = vars
			return "run-anchor-3", nil
		}
		return s, &gotVars
	}
	commandMap := func() webhooks.Config {
		cfg := glConfig()
		cfg.BotIDs = []string{"review-pr", "revi-converse"}
		cfg.CommandMap = map[string][]webhooks.CommandRoute{
			"revi": {
				{BotID: "review-pr", Scope: "pr", Disambiguator: "when_args_empty"},
				{BotID: "revi-converse", Scope: "pr", Disambiguator: "when_args_present", ArgsVar: "converse_question"},
			},
		}
		return cfg
	}

	t.Run("no position anywhere: the block rides empty, present", func(t *testing.T) {
		s, got := newServer(t)
		body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi why?"`, 1)
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(commandMap()), body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("command: code=%d body=%s", w.Code, w.Body.String())
		}
		assertAnchorBlock(t, "gitlab command", *got, map[string]string{
			"thread_anchor_path":     "",
			"thread_anchor_old_path": "",
			"thread_anchor_line":     "",
			"thread_anchor_old_line": "",
			"thread_anchor_side":     "",
			"thread_anchor_commit":   "",
		})
	})

	t.Run("a command that OPENED a diff thread: the note's own position rides", func(t *testing.T) {
		s, got := newServer(t)
		body := strings.Replace(glNoteRevi, `"note": "/revi"`,
			`"note": "/revi why here?", "position": {"head_sha": "h9", "old_path": "a.go", "new_path": "a.go", "old_line": 4, "new_line": 6, "position_type": "text"}`, 1)
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(commandMap()), body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("command: code=%d body=%s", w.Code, w.Body.String())
		}
		assertAnchorBlock(t, "gitlab command on a fresh inline comment", *got, map[string]string{
			"thread_anchor_path":     "a.go",
			"thread_anchor_line":     "6",
			"thread_anchor_old_line": "4",
			"thread_anchor_side":     "new",
			"thread_anchor_commit":   "h9",
		})
	})
}

// The gate core hands back the ROOT's anchor from the SAME discussion fetch
// that classified the thread — no second call.
func TestGitlabNoteGateReturnsTheThreadRootAnchor(t *testing.T) {
	s := &Server{}
	p := gitlab.ParsedNote{ProjectID: 42, MRIID: 7, DiscussionID: "d-1", AuthorID: 2, AuthorUsername: "alice", NoteBody: "why?"}
	root := &gitlab.NotePosition{NewPath: "pkg/x/y.go", NewLine: int64Ptr(42), HeadSHA: "h1"}
	api := &fakeGitlabNoteAPI{
		who:   gitlab.User{ID: 1, Username: "revi-bot"},
		notes: []gitlab.DiscussionNote{{AuthorID: 1, Body: "the review comment", Position: root}, {AuthorID: 2, Body: "why?"}},
	}
	authorized, _, _, _, anchor, err := s.gitlabNoteGateWithAPI(context.Background(), webhooks.Config{AuthorizedRepliers: []string{"alice"}}, p, api)
	if err != nil || !authorized {
		t.Fatalf("authorized=%v err=%v", authorized, err)
	}
	if !anchor.Set() || anchor.Path != "pkg/x/y.go" || anchor.Line != 42 || anchor.Commit != "h1" {
		t.Errorf("the gate's anchor = %+v, want the root's position from the same fetch", anchor)
	}
	if api.discCalls != 1 {
		t.Errorf("the anchor must come from the classification fetch, not a second call: %d calls", api.discCalls)
	}
}

func int64Ptr(n int64) *int64 { return &n }

// The operator's pinned launch vars win over the anchor block on BOTH
// GitLab entries: the lanes merge the anchor into the vars BEFORE the
// webhook's var layers (cfg.LaunchVars apply last), so an EMPTY anchor
// clobbers nothing and a REAL anchor cannot silently replace an
// operator's explicit choice ("never silently replace an operator's
// explicit choice"). The GitHub/Forgejo lane has merged before the layers
// since it shipped (buildPRForgeReviewReplyVars). Round-1 B-M1.
func TestGitLabWebhookCommandLaneOperatorPinBeatsTheAnchor(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *map[string]string) {
		t.Helper()
		s := newWebhookTestServer(t)
		s.cfg.Bots.Paths = []string{botsDirAbs(t)}
		var gotVars map[string]string
		s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
			gotVars = vars
			return "run-anchor-4", nil
		}
		return s, &gotVars
	}
	commandMap := func() webhooks.Config {
		cfg := glConfig()
		cfg.BotIDs = []string{"review-pr", "revi-converse"}
		cfg.CommandMap = map[string][]webhooks.CommandRoute{
			"revi": {
				{BotID: "review-pr", Scope: "pr", Disambiguator: "when_args_empty"},
				{BotID: "revi-converse", Scope: "pr", Disambiguator: "when_args_present", ArgsVar: "converse_question"},
			},
		}
		return cfg
	}
	pinned := func(cfg webhooks.Config) webhooks.Config {
		cfg.LaunchVars = map[string]string{
			"thread_anchor_path":   "pinned-by-operator.go",
			"thread_anchor_commit": "operatorsha",
		}
		return cfg
	}

	t.Run("an empty anchor must not clobber the pin", func(t *testing.T) {
		s, got := newServer(t)
		body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi why?"`, 1)
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(pinned(commandMap())), body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("command: code=%d body=%s", w.Code, w.Body.String())
		}
		assertAnchorBlock(t, "command lane with an operator pin, no anchor", *got, map[string]string{
			"thread_anchor_path":   "pinned-by-operator.go",
			"thread_anchor_commit": "operatorsha",
		})
	})

	t.Run("a real anchor must not silently replace the pin either", func(t *testing.T) {
		s, got := newServer(t)
		body := strings.Replace(glNoteRevi, `"note": "/revi"`,
			`"note": "/revi why here?", "position": {"head_sha": "h9", "old_path": "a.go", "new_path": "a.go", "new_line": 6, "position_type": "text"}`, 1)
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(pinned(commandMap())), body))
		if w.Code != http.StatusAccepted {
			t.Fatalf("command: code=%d body=%s", w.Code, w.Body.String())
		}
		assertAnchorBlock(t, "command lane with an operator pin and a real anchor", *got, map[string]string{
			"thread_anchor_path":   "pinned-by-operator.go",
			"thread_anchor_commit": "operatorsha",
		})
	})
}

// The prforge lane's precedence is pinned too: the operator's pinned vars
// beat the payload's anchor (the merge sits before the var layers).
func TestPRForgeReviewReplyOperatorPinBeatsTheAnchor(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		gotVars = vars
		return "run-anchor-5", nil
	}
	s.webhookPRForgeReviewReplyGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedReviewComment, string) (bool, string, string, error) {
		return true, "transcript", "allowlist", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	cfg.EventAllowlist = []string{"pull_request_review_comment"}
	cfg.LaunchVars = map[string]string{"thread_anchor_path": "pinned-by-operator.go", "thread_anchor_commit": "operatorsha"}
	body := `{
		"action": "created",
		"repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
		"comment": {"id": 9002, "in_reply_to_id": 9001, "body": "why?",
			"html_url": "https://github.com/acme/widgets/pull/7#discussion_r9002",
			"path": "pkg/x/y.go", "line": 42, "side": "RIGHT", "commit_id": "c0ffee1", "user": {"login": "alice"}},
		"pull_request": {"number": 7, "state": "open", "title": "T", "body": "d",
			"html_url": "https://github.com/acme/widgets/pull/7",
			"head": {"sha": "abc123", "ref": "feature/x", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"}},
		"sender": {"login": "alice"}
	}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderReviewComment, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("reply: code=%d body=%s", w.Code, w.Body.String())
	}
	assertAnchorBlock(t, "prforge lane with an operator pin", gotVars, map[string]string{
		"thread_anchor_path":   "pinned-by-operator.go",
		"thread_anchor_commit": "operatorsha",
	})
}
