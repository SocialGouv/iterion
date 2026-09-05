package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// glNoteApprove builds an MR note carrying `/revi approve <reason>` by the
// given username, on the same MR glNoteRevi targets (gitlab.com/acme/widgets!7).
func glNoteApprove(username, reason string) string {
	note := "/revi approve"
	if reason != "" {
		note += " " + reason
	}
	return `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "user": {"username": "` + username + `", "id": 9},
  "object_attributes": {"id": 556, "note": "` + note + `", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7#note_556", "noteable_type": "MergeRequest", "discussion_id": "d-9", "author_id": 9},
  "merge_request": {"iid": 7, "state": "opened", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "headsha"}}
}`
}

// A `/revi approve` MR note must be intercepted as an override — it must NOT
// launch a re-review bot. Parity with TestGitHubComment_ReviewApproveDoesNotLaunch.
func TestGitLabComment_ReviewApproveDoesNotLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	// No connection, no forge_token binding: the gate resolves nothing —
	// the point of this test is that the approve branch ran (filtered, not
	// launch_error and not a launch), not that it succeeded end to end.
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(glConfig()), glNoteApprove("alice", "dispute")))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if launched != 0 {
		t.Fatalf("/revi approve must NOT launch a bot (launched=%d)", launched)
	}
}

// /revi approve authorizes through the SAME command gate as every other
// /command, keyed on the review-pr route with the floor RAISED to
// approveFloor — not the webhook's own MinReplierRole/AuthorizedRepliers,
// which govern who may TALK to the bot, not who may force-green a gate.
func TestGitLabComment_ReviewApprove_UsesCommandGateWithRaisedFloor(t *testing.T) {
	s := newWebhookTestServer(t)
	var gateCalls int
	var gotRoute webhooks.CommandRoute
	var gotAllowlist []string
	s.webhookCommandGate = func(_ context.Context, cfg webhooks.Config, _ gitlab.ParsedNote, route webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		gateCalls++
		gotRoute = route
		gotAllowlist = cfg.AuthorizedRepliers
		return gateRefused, "replier not authorized", nil
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	cfg := glConfig()
	cfg.AuthorizedRepliers = []string{"mallory"} // must NOT bypass the approve floor
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteApprove("mallory", "dispute")))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gateCalls != 1 {
		t.Fatalf("approve must consult the command gate exactly once (calls=%d)", gateCalls)
	}
	if gotRoute.BotID != "review-pr" {
		t.Fatalf("gate must be keyed on the review-pr route, got %q", gotRoute.BotID)
	}
	if gotRoute.MinReplierRole != "maintainer" {
		t.Fatalf("gate must raise the floor to maintainer, got %q", gotRoute.MinReplierRole)
	}
	if gotAllowlist != nil {
		t.Fatalf("the webhook's talk-back allowlist must NOT bypass the approve floor, got %v", gotAllowlist)
	}
	if launched != 0 {
		t.Fatalf("denied approve must not launch anything (launched=%d)", launched)
	}
}

// TestGitLabReviewApprove_ThroughConnectionAdmin pins the happy path through
// a team connection's admin client — parity with
// TestReviewApprove_ThroughConnectionAdmin_NotForgeTokenBinding: the status
// write and reply both ride s.forgeGateClientFor / s.forgeGitlabPullCommenterFor
// (the SAME seams the publish/pause-notice paths use), never the webhook's
// forge_token binding, which is intentionally unset here.
func TestGitLabReviewApprove_ThroughConnectionAdmin(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "conn-gl", TenantID: "t1", Provider: forge.ProviderGitLab}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "deadbeef1234"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	var commented int
	var lastBody string
	s.forgeGitlabPullCommenterFor = func(context.Context, forge.Connection) (gitlabPullCommenter, error) {
		return &fakeGitlabPullCommenter{onComment: func(body string) { commented++; lastBody = body }}, nil
	}
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	cfg := glConfig()
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteApprove("maintainer-jane", "false positive")))

	if w.Code != http.StatusOK {
		t.Fatalf("approve must answer 200, got %d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 1 {
		t.Fatalf("approve must post the status through the connection's gate client exactly once, got setCalls=%d", gc.setCalls)
	}
	if gc.last.Context != "revi/review" || gc.last.State != forge.CommitStateSuccess {
		t.Fatalf("approve must post success under the pinned gate context, got %+v", gc.last)
	}
	if gc.lastSHA != "deadbeef1234" {
		t.Fatalf("approve must post on the resolved MR head SHA, got %q", gc.lastSHA)
	}
	if commented != 0 {
		t.Fatalf("a successful approve posts no reply comment (the status IS the answer): commented=%d body=%q", commented, lastBody)
	}
}

// TestGitLabReviewApprove_TokenBindingWritesTheStatusWithoutAConnection pins
// the forge_token-binding fallback: no team connection covers the repo, but
// the webhook's own forge_token binding (seedForgeToken — the SAME helper
// the GitHub/Forgejo approve tests use, provider-agnostic) still lets the
// status land, against a fake (but real-HTTP) GitLab API.
func TestGitLabReviewApprove_TokenBindingWritesTheStatusWithoutAConnection(t *testing.T) {
	s := newWebhookTestServer(t)
	var setCalls int
	var gotContext, gotState, gotSHA string
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "state": "opened", "sha": "cafe1234", "source_project_id": 42, "target_project_id": 42})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/statuses/"):
			setCalls++
			gotSHA = strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if st, ok := body["state"].(string); ok {
				gotState = st
			}
			if c, ok := body["context"].(string); ok {
				gotContext = c
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer glSrv.Close()
	cfg := glConfig()
	cfg.ForgeBaseURL = glSrv.URL
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	seedForgeToken(t, s, &cfg, "gl-tok")
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	body := strings.Replace(glNoteApprove("maintainer-jane", ""), `"url": "https://gitlab.com/acme/widgets/-/merge_requests/7"`, `"url": "`+glSrv.URL+`/acme/widgets/-/merge_requests/7"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if setCalls != 1 {
		t.Fatalf("expected exactly one status write via the forge_token binding, got %d", setCalls)
	}
	if gotState != "success" || gotContext != "revi/review" || gotSHA != "cafe1234" {
		t.Fatalf("state=%q context=%q sha=%q", gotState, gotContext, gotSHA)
	}
}

// TestGitLabReviewApprove_AuthorCannotApproveOwnMR pins the self-approve
// refusal: the commenter cleared the maintainer floor, but the MR's own
// author cannot force-green their own gate.
func TestGitLabReviewApprove_AuthorCannotApproveOwnMR(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "conn-gl", TenantID: "t1", Provider: forge.ProviderGitLab}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &authoredGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}, author: "maintainer-jane"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(glConfig()), glNoteApprove("maintainer-jane", "")))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gc.setCalls != 0 {
		t.Fatalf("self-approve must never write the status, setCalls=%d", gc.setCalls)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("self-approve is a configuration-shaped refusal (filtered), got %v", resp)
	}
}

// TestGitLabReviewApprove_UnauthorizedCommenterIsRefusedInSilence: a
// commenter the gate REFUSES gets no reply — the delivery audit is the only
// record. Reachable by anyone who can comment; a reply there would be a bot
// comment any drive-by could drive.
func TestGitLabReviewApprove_UnauthorizedCommenterIsRefusedInSilence(t *testing.T) {
	s := newWebhookTestServer(t)
	var commentCalls int
	s.forgeGitlabPullCommenterFor = func(context.Context, forge.Connection) (gitlabPullCommenter, error) {
		return &fakeGitlabPullCommenter{onComment: func(string) { commentCalls++ }}, nil
	}
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateRefused, "replier not authorized: mallory", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(glConfig()), glNoteApprove("mallory", "")))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("expected filtered, got %v", resp)
	}
	if commentCalls != 0 {
		t.Fatalf("a REFUSED commenter must get no reply (commentCalls=%d)", commentCalls)
	}
}

// TestGitLabReviewApprove_AuthzForgeErrorIs200LaunchError pins the explicit
// ticket requirement: a forge error on the authz read is 200 + launch_error,
// NEVER a 5xx (which GitLab answers by disabling the hook).
func TestGitLabReviewApprove_AuthzForgeErrorIs200LaunchError(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateRefused, "", errors.New("member access level: gitlab: http 503")
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-x", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(glConfig()), glNoteApprove("maintainer-jane", "flaky finding")))

	if w.Code != http.StatusOK {
		t.Fatalf("a forge error on the authz read must never be a 5xx (the forge disables the hook), got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusLaunchError {
		t.Fatalf("status must be launch_error, got %v", resp)
	}
	if launched != 0 {
		t.Fatalf("nothing may launch on an unverified commenter (launched=%d)", launched)
	}
}

// TestGitLabReviewApprove_ConfigurationRefusalRepliesOnMR pins the
// "configuration refusal ⇒ reply" half of parity: past the gate, a
// maintainer-shaped commenter who cannot be served (no connection, no
// forge_token binding) is TOLD what to fix on the MR — unlike the silent
// gateRefused case above.
func TestGitLabReviewApprove_ConfigurationRefusalRepliesOnMR(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	w := httptest.NewRecorder()
	// No forgeConnections, no forge_token binding: resolveGitLabApproveWritePath
	// must refuse with nothing to write through — but the commenter already
	// cleared the maintainer floor, so this is a configuration miss told to
	// them, not a silent drop.
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(glConfig()), glNoteApprove("maintainer-jane", "")))

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("expected filtered (configuration refusal), got %v", resp)
	}
}

// TestGitLabReviewApprove_WriteFailureRepliesAnd200 pins the fail-with-reply
// path: a forge write error must not 502 the webhook — record launch_error,
// best-effort reply on the MR, answer 200.
func TestGitLabReviewApprove_WriteFailureRepliesAnd200(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "conn-gl", TenantID: "t1", Provider: forge.ProviderGitLab}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "deadbeef", setErr: errors.New("403 insufficient scope")}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	var commented int
	var lastBody string
	s.forgeGitlabPullCommenterFor = func(context.Context, forge.Connection) (gitlabPullCommenter, error) {
		return &fakeGitlabPullCommenter{onComment: func(body string) { commented++; lastBody = body }}, nil
	}
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	cfg := glConfig()
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteApprove("maintainer-jane", "")))

	if w.Code != http.StatusOK {
		t.Fatalf("write failure must answer 200 (never 502), got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusLaunchError {
		t.Fatalf("expected launch_error, got %v", resp)
	}
	if commented != 1 || !strings.Contains(lastBody, "statuses permission") {
		t.Fatalf("write failure must reply on the MR explaining what to fix: commented=%d body=%q", commented, lastBody)
	}
}

// TestGitLabReviewApprove_IdempotentOnRedelivery pins the replay guard: a
// redelivery of an already-launched approve answers `duplicate`, never a
// second status write.
func TestGitLabReviewApprove_IdempotentOnRedelivery(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "conn-gl", TenantID: "t1", Provider: forge.ProviderGitLab}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	gc := &fakeGateClient{headSHA: "deadbeef"}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.webhookCommandGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "role", nil
	}
	cfg := glConfig()
	cfg.LaunchVars = map[string]string{gateContextVar: "revi/review"}
	body := glNoteApprove("maintainer-jane", "")

	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glNoteReq(gitlabCtx(cfg), body))
	if w1.Code != http.StatusOK || gc.setCalls != 1 {
		t.Fatalf("first delivery: code=%d setCalls=%d body=%s", w1.Code, gc.setCalls, w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glNoteReq(gitlabCtx(cfg), body))
	if w2.Code != http.StatusOK || gc.setCalls != 1 {
		t.Fatalf("redelivery must not write a second status: code=%d setCalls=%d body=%s", w2.Code, gc.setCalls, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp) //nolint:errcheck
	if resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("redelivery must answer duplicate, got %v", resp)
	}
}

// fakeGitlabPullCommenter records CommentPullRequest calls — the gitlabPullCommenter
// test double, mirroring stubCommenter (the GitHub/Forgejo twin).
type fakeGitlabPullCommenter struct {
	onComment func(body string)
	err       error
}

func (f *fakeGitlabPullCommenter) CommentPullRequest(_ context.Context, _ string, _ int, body string) (forge.CommentRef, error) {
	if f.onComment != nil {
		f.onComment(body)
	}
	return forge.CommentRef{}, f.err
}
