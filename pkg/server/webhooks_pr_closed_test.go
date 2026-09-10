package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

const ghClosedPR = `{
  "action": "closed",
  "number": 7,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7", "state": "closed", "merged": true,
    "head": {"ref": "feature/x", "sha": "abc123", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`

// A merged or closed pull request ends every review still bound to it: a
// run in flight burns provider quota on a diff nobody will merge, and a
// PARKED run would wake hours later to comment on a dead PR.
func TestGitHubWebhook_ClosedPRStopsItsRuns(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg, pt := ghConfig(t, s)

	// Two launched runs on this PR (two different bots) plus one on
	// another PR, which must be left alone.
	for _, d := range []webhooks.Delivery{
		{ID: "d1", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "review-pr", RunID: "run-a", Status: webhooks.StatusLaunched},
		{ID: "d2", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "branch-improve-loop", RunID: "run-b", Status: webhooks.StatusLaunched},
		{ID: "d3", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:9", BotID: "review-pr", RunID: "run-c", Status: webhooks.StatusLaunched},
		// A filtered delivery launched nothing, so it carries no run id —
		// which is exactly what makes it invisible to the by-subject query
		// (the store defines "launched" as having one, like CountLaunched).
		{ID: "d4", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "pr:7", BotID: "review-pr", Status: webhooks.StatusFiltered},
		// A `/billy` fixer launched from a COMMENT: its own subject is the
		// comment id, and only the parent link ties it to pr:7. Before that
		// link the stop could not reach it — the exact run whose quota it
		// keeps burning on a merged PR.
		{ID: "d6", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/widgets", SubjectID: "comment:99", ParentSubjectID: "pr:7", BotID: "branch-improve-loop", RunID: "run-f", Status: webhooks.StatusLaunched},
		// SAME subject id, ANOTHER repo on the same multi-project webhook:
		// PR numbers collide freely across repos, and cancelling this one
		// would block an unrelated pull request.
		{ID: "d5", TenantID: cfg.TenantID, WebhookID: cfg.ID, ProjectPath: "acme/other", SubjectID: "pr:7", BotID: "review-pr", RunID: "run-e", Status: webhooks.StatusLaunched},
	} {
		if err := s.webhookDeliveries.Insert(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghClosedPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("a closed PR must answer 200/filtered (a 4xx gets the hook disabled): code=%d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("a closed PR must launch nothing, launched=%d", launched)
	}
	if len(cancelled) != 3 {
		t.Fatalf("every live run of pr:7 must stop — including the comment-launched fixer, got %v", cancelled)
	}
	var sawComment bool
	for _, id := range cancelled {
		if id == "run-f" {
			sawComment = true
		}
	}
	if !sawComment {
		t.Fatal("a /command run reaches the PR only through ParentSubjectID — the stop missed it")
	}
	for _, id := range cancelled {
		if id == "run-c" {
			t.Fatal("another PR's run was cancelled — the stop must be scoped to the subject")
		}
		if id == "run-d" {
			t.Fatal("a delivery that never launched carries no run to cancel")
		}
		if id == "run-e" {
			t.Fatal("a same-numbered PR of ANOTHER repo was cancelled — the stop must be project-scoped")
		}
	}
}

// The action is what says "this PR is over": a synchronize whose payload
// happens to carry a closed state is a race, and must not end a review.
func TestParsedIsClosed(t *testing.T) {
	closed, err := prforge.ParsePullRequest([]byte(ghClosedPR))
	if err != nil {
		t.Fatal(err)
	}
	if !closed.IsClosed() {
		t.Fatalf("merged PR must read as closed: %+v", closed)
	}
	open, err := prforge.ParsePullRequest([]byte(ghOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if open.IsClosed() {
		t.Fatal("an opened PR must not read as closed")
	}
}

const glMergedMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 7, "action": "merge", "state": "merged", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "sha1"}}
}`

const glNoteBilly = `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "user": {"username": "alice"},
  "object_attributes": {"id": 99, "note": "/billy fix the findings", "noteable_type": "MergeRequest", "discussion_id": "d-1", "author_id": 1},
  "merge_request": {"iid": 7, "state": "opened", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "headsha"}}
}`

// A GitLab command run (`/billy`, a converse reply) records the NOTE as its
// own subject, so the closed-MR stop reaches it only through the parent link.
// Without it a fixer keeps working — and pushing — on a merge request that
// already merged. The delivery row is written by the real note lane here:
// asserting on a hand-inserted row would only exercise the store's reader.
func TestGitLabWebhook_ClosedMRStopsItsNoteLaunchedRuns(t *testing.T) {
	s := newWebhookTestServer(t)
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "branch-improve-loop"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"billy": {{BotID: "branch-improve-loop", Scope: "pr", ArgsVar: "scope_notes"}},
	}
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	s.webhookGitLabPRResolver = func(_ context.Context, _ webhooks.Config, p gitlab.ParsedNote, _ string) (forge.PullRef, error) {
		return forge.PullRef{State: "open", SourceBranch: p.SourceBranch, TargetBranch: p.TargetBranch, HeadRepoFullName: p.ProjectPath}, nil
	}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "run-billy", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteBilly))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the /billy note must launch: code=%d body=%s", w.Code, w.Body.String())
	}

	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glMergedMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusOK {
		t.Fatalf("a merged MR must answer 200/filtered: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(cancelled) != 1 || cancelled[0] != "run-billy" {
		t.Fatalf("the note-launched fixer must stop when its merge request merges, cancelled=%v", cancelled)
	}
}

// The parent link is what makes a note-launched run findable from its merge
// request. An issue note hangs off no MR and must name none.
func TestGitLabNoteMetaCarriesTheParentSubject(t *testing.T) {
	mrNote := gitlab.ParsedNote{ProjectPath: "acme/widgets", NoteID: 99, MRIID: 7, MRURL: "https://gitlab.com/acme/widgets/-/merge_requests/7"}
	if got := gitlabNoteMeta(mrNote).ParentSubjectID; got != "mr:7" {
		t.Fatalf("an MR note must name mr:7 as its parent, got %q", got)
	}
	issueNote := gitlab.ParsedNote{ProjectPath: "acme/widgets", NoteID: 99, IssueIID: 4, IssueURL: "https://gitlab.com/acme/widgets/-/issues/4"}
	if got := gitlabNoteMeta(issueNote).ParentSubjectID; got != "" {
		t.Fatalf("an issue note hangs off no merge request, got parent %q", got)
	}
}
