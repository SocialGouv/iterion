package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

func glConfig() webhooks.Config {
	return webhooks.Config{ID: "w1", TenantID: "t1", Provider: webhooks.ProviderGitLab, Enabled: true, BotIDs: []string{"review-pr"}}
}

// gitlabCtx simulates what webhookAuth stamps before the handler runs.
func gitlabCtx(cfg webhooks.Config) context.Context {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "webhook:" + cfg.ID, TeamID: cfg.TenantID, Role: identity.RoleMember})
	ctx = store.WithIdentity(ctx, cfg.TenantID, "webhook:"+cfg.ID)
	return context.WithValue(ctx, webhookCtxKey{}, cfg)
}

func glReq(ctx context.Context, body, event string) *http.Request {
	r := httptest.NewRequest("POST", "/api/webhooks/gitlab/w1", strings.NewReader(body)).WithContext(ctx)
	if event != "" {
		r.Header.Set("X-Gitlab-Event", event)
	}
	r.SetPathValue("id", "w1")
	return r
}

const glOpenMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 7, "action": "open", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "sha1"}}
}`

func TestGitLabWebhook_HappyPath(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef, gotProjectPath string
	var gotVars, gotKeyOverrides, gotSecretOverrides map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, keyOverrides, secretOverrides map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef, gotKeyOverrides, gotSecretOverrides = botID, vars, repoURL, repoRef, keyOverrides, secretOverrides
		gotProjectPath = projectPath
		return "run-123", nil
	}
	cfg := glConfig()
	cfg.KeyOverrides = map[string]string{"anthropic": "key-abc"}
	cfg.SecretOverrides = map[string]string{"forge_token": "sec-xyz"}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-123" {
		t.Fatalf("resp: %v", resp)
	}
	if calls != 1 || gotBot != "review-pr" {
		t.Fatalf("launch calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["pr_url"] != "https://gitlab.com/acme/widgets/-/merge_requests/7" || gotVars["base_ref"] != "main" || gotVars["post_to_board"] != "false" {
		t.Fatalf("vars: %v", gotVars)
	}
	if gotURL != "https://gitlab.com/acme/widgets.git" || gotRef != "feature/x" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
	// project_path (the stable forge slug) must reach the launch so the
	// run is filterable by repository in the studio.
	if gotProjectPath != "acme/widgets" {
		t.Fatalf("project path not threaded to launch: %q", gotProjectPath)
	}
	if gotKeyOverrides["anthropic"] != "key-abc" {
		t.Fatalf("key overrides not threaded to launch: %v", gotKeyOverrides)
	}
	if gotSecretOverrides["forge_token"] != "sec-xyz" {
		t.Fatalf("secret overrides not threaded to launch: %v", gotSecretOverrides)
	}
	// delivery recorded as launched
	if list, _ := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "w1", 10); len(list) != 1 || list[0].Status != webhooks.StatusLaunched || list[0].RunID != "run-123" {
		t.Fatalf("delivery: %+v", list)
	}
}

// glDraftMR is a draft MR opened — must be filtered (never auto-launched)
// even though the action is "open".
const glDraftMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 9, "action": "open", "source_branch": "wip/x", "target_branch": "main",
    "title": "Draft: Add X", "description": "wip", "url": "https://gitlab.com/acme/widgets/-/merge_requests/9",
    "draft": true, "work_in_progress": true, "last_commit": {"id": "sha9"}}
}`

// glReadyMR is the draft→ready transition (update clearing changes.draft):
// GitLab's stand-in for a ready-for-review action — this MUST launch.
const glReadyMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 9, "action": "update", "source_branch": "wip/x", "target_branch": "main",
    "title": "Add X", "description": "ready", "url": "https://gitlab.com/acme/widgets/-/merge_requests/9",
    "draft": false, "work_in_progress": false, "last_commit": {"id": "sha9"}},
  "changes": {"draft": {"previous": true, "current": false}}
}`

func TestGitLabWebhook_DraftMRNotAutoLaunched(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glConfig()), glDraftMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusOK {
		t.Fatalf("draft MR should be a filtered 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("draft MR status=%q want filtered", resp["status"])
	}
	if calls != 0 {
		t.Fatalf("draft MR must NOT launch a bot (calls=%d)", calls)
	}
}

func TestGitLabWebhook_ReadyForReviewLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot string
	s.webhookLaunchBot = func(_ context.Context, botID string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot = botID
		return "run-ready", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glConfig()), glReadyMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusAccepted {
		t.Fatalf("ready MR should launch (202), got %d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotBot != "review-pr" {
		t.Fatalf("ready MR must launch review bot once: calls=%d bot=%q", calls, gotBot)
	}
}

func TestGitLabWebhook_Idempotent(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	cfg := glConfig()
	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w1.Code)
	}
	// replay same payload (same head sha) → duplicate, no second launch
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w2.Code != http.StatusOK {
		t.Fatalf("replay code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate || resp["run_id"] != "run-123" {
		t.Fatalf("duplicate resp: %v", resp)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one launch, got %d", calls)
	}
}

// glReRequestMR builds the "Re-request review" delivery: an `update` whose
// changes.reviewers stamps re_requested on one reviewer. updatedAt salts the
// idempotency key (one delivery per click); state exercises the open-MR gate.
func glReRequestMRState(actor, reviewer, updatedAt, state string, reRequested bool) string {
	return `{
	  "object_kind": "merge_request",
	  "user": {"username": "` + actor + `"},
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "update", "state": "` + state + `", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "updated_at": "` + updatedAt + `", "last_commit": {"id": "sha1"}},
	  "changes": {"reviewers": {"previous": [], "current": [{"id": 575, "username": "` + reviewer + `", "re_requested": ` + map[bool]string{true: "true", false: "false"}[reRequested] + `}]}}
	}`
}

func glReRequestMR(actor, reviewer, updatedAt string, reRequested bool) string {
	return glReRequestMRState(actor, reviewer, updatedAt, "opened", reRequested)
}

// The forge-native re-review button: a reviewers change that (re-)requests a
// review from iterion's own bot account launches the review bot — even on a
// head the MR-open lane already claimed — and each click launches again.
func TestGitLabWebhook_ReRequestReviewLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotVars = vars
		return "run-123", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	s.webhookReviewRequestGate = func(context.Context, webhooks.Config, gitlab.Parsed, string) (bool, string, error) {
		return true, "test-gate", nil
	}
	cfg := glConfig()

	// The open already claimed this head under the "mr|" key space…
	w0 := httptest.NewRecorder()
	s.handleGitLabWebhook(w0, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w0.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("open: code=%d calls=%d", w0.Code, calls)
	}

	// …and the re-request still relaunches on that same head.
	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glReq(gitlabCtx(cfg), glReRequestMR("alice", "iterion-bot", "2026-09-01 10:00:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if w1.Code != http.StatusAccepted || calls != 2 {
		t.Fatalf("re-request: code=%d calls=%d body=%s", w1.Code, calls, w1.Body.String())
	}
	if gotVars["re_review"] != "true" || gotVars["head_sha"] != "sha1" {
		t.Fatalf("re-request vars: %v", gotVars)
	}

	// The forge redelivering the SAME click (same updated_at) is a replay…
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), glReRequestMR("alice", "iterion-bot", "2026-09-01 10:00:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if calls != 2 {
		t.Fatalf("redelivery must not double-launch: calls=%d", calls)
	}
	// …but a SECOND click (new updated_at) on the same head reviews again.
	w3 := httptest.NewRecorder()
	s.handleGitLabWebhook(w3, glReq(gitlabCtx(cfg), glReRequestMR("alice", "iterion-bot", "2026-09-01 11:22:33 UTC", true), gitlab.EventHeaderMergeRequest))
	if w3.Code != http.StatusAccepted || calls != 3 {
		t.Fatalf("second click: code=%d calls=%d", w3.Code, calls)
	}
}

// The publish tail self-assigns the bot as reviewer after each review, and
// GitLab echoes that PUT back as a reviewers change. The actor of that echo
// is the bot itself — it must never trigger a review (the self-launch loop
// this guard exists for). A change naming some OTHER reviewer is ordinary
// MR housekeeping and stays filtered too.
func TestGitLabWebhook_ReviewerChangeNotForBotFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	s.webhookIterionBotAuthor = func(_ context.Context, _ webhooks.Config, login string) bool {
		return login == "iterion-bot"
	}
	cfg := glConfig()

	// Self-assign echo: the bot added ITSELF (actor = bot) → filtered.
	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glReq(gitlabCtx(cfg), glReRequestMR("iterion-bot", "iterion-bot", "2026-09-01 10:00:00 UTC", false), gitlab.EventHeaderMergeRequest))
	if w1.Code != http.StatusOK || calls != 0 {
		t.Fatalf("self-assign echo: code=%d calls=%d body=%s", w1.Code, calls, w1.Body.String())
	}

	// A human requesting a review from ANOTHER human → filtered.
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), glReRequestMR("alice", "bob", "2026-09-01 10:05:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if w2.Code != http.StatusOK || calls != 0 {
		t.Fatalf("other reviewer: code=%d calls=%d body=%s", w2.Code, calls, w2.Body.String())
	}
}

// Reviewer edits arrive freely on closed and merged MRs — a re-request there
// must never burn a review run (mirror of the Note lane's closed-MR filter).
func TestGitLabWebhook_ReRequestOnClosedMRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	cfg := glConfig()
	for _, state := range []string{"closed", "merged"} {
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glReRequestMRState("alice", "iterion-bot", "2026-09-01 10:00:00 UTC", state, true), gitlab.EventHeaderMergeRequest))
		if w.Code != http.StatusOK || calls != 0 {
			t.Fatalf("state=%s: code=%d calls=%d body=%s", state, w.Code, calls, w.Body.String())
		}
	}
}

// One GitLab `update` can be BOTH a push (oldrev) and a reviewers change.
// With ReviewOnSync on, that event must ride the per-head resync key — else
// the following pure resync on the same head reviews it a second time.
func TestGitLabWebhook_PushWithReviewersDiffDoesNotDoubleLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	s.webhookReviewRequestGate = func(context.Context, webhooks.Config, gitlab.Parsed, string) (bool, string, error) {
		return true, "test-gate", nil
	}
	cfg := glConfig()
	cfg.ReviewOnSync = true

	pushWithReviewers := `{
	  "object_kind": "merge_request",
	  "user": {"username": "alice"},
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "update", "state": "opened", "oldrev": "sha0", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "updated_at": "2026-09-01 10:00:00 UTC", "last_commit": {"id": "sha1"}},
	  "changes": {"reviewers": {"previous": [], "current": [{"id": 575, "username": "iterion-bot", "re_requested": true}]}}
	}`
	pureResync := `{
	  "object_kind": "merge_request",
	  "user": {"username": "alice"},
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "update", "state": "opened", "oldrev": "sha0", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "updated_at": "2026-09-01 10:00:05 UTC", "last_commit": {"id": "sha1"}}
	}`
	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glReq(gitlabCtx(cfg), pushWithReviewers, gitlab.EventHeaderMergeRequest))
	if w1.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("push+reviewers: code=%d calls=%d", w1.Code, calls)
	}
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), pureResync, gitlab.EventHeaderMergeRequest))
	if calls != 1 {
		t.Fatalf("pure resync on the same head must dedupe against the first launch: calls=%d", calls)
	}
}

// R7e050f: the button is a MANUAL trigger like /revi, and must ride the same
// replier controls (AuthorizedRepliers / MinReplierRole). An unauthorized
// click never launches — and never earns the hold-label exemption: with no
// stub the production gate fail-closes on the missing forge token, which is
// exactly the state this harness runs in.
func TestGitLabWebhook_ReRequestUnauthorizedReplierFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	s.webhookIterionBotReviewRequest = func(_ context.Context, _ webhooks.Config, requested func(string) bool) bool {
		return requested("iterion-bot")
	}
	cfg := glConfig()

	// Production gate, no stub: no forge token resolvable → refused, filtered.
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glReRequestMR("mallory", "iterion-bot", "2026-09-01 10:00:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("unauthorized re-request must filter: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}

	// An explicit refusal from the gate seam behaves identically.
	s.webhookReviewRequestGate = func(context.Context, webhooks.Config, gitlab.Parsed, string) (bool, string, error) {
		return false, "re-request review by unauthorized replier: mallory", nil
	}
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), glReRequestMR("mallory", "iterion-bot", "2026-09-01 10:01:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if w2.Code != http.StatusOK || calls != 0 {
		t.Fatalf("refused re-request must filter: code=%d calls=%d body=%s", w2.Code, calls, w2.Body.String())
	}

	// An authz ERROR on a re-request-only delivery is a 502 so the forge
	// redelivers (mirror of the note gate).
	s.webhookReviewRequestGate = func(context.Context, webhooks.Config, gitlab.Parsed, string) (bool, string, error) {
		return false, "", context.DeadlineExceeded
	}
	w3 := httptest.NewRecorder()
	s.handleGitLabWebhook(w3, glReq(gitlabCtx(cfg), glReRequestMR("mallory", "iterion-bot", "2026-09-01 10:02:00 UTC", true), gitlab.EventHeaderMergeRequest))
	if w3.Code != http.StatusBadGateway || calls != 0 {
		t.Fatalf("authz error must 502: code=%d calls=%d body=%s", w3.Code, calls, w3.Body.String())
	}

	// R34eb8c: when an automatic lane co-rides the event (push + reviewers
	// diff, ReviewOnSync on), the same authz error must NOT strand it — the
	// gesture is demoted and the resync launches.
	cfg2 := glConfig()
	cfg2.ReviewOnSync = true
	coRiding := `{
	  "object_kind": "merge_request",
	  "user": {"username": "mallory"},
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "update", "state": "opened", "oldrev": "sha0", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "updated_at": "2026-09-01 10:03:00 UTC", "last_commit": {"id": "sha-co"}},
	  "changes": {"reviewers": {"previous": [], "current": [{"id": 575, "username": "iterion-bot", "re_requested": true}]}}
	}`
	w4 := httptest.NewRecorder()
	s.handleGitLabWebhook(w4, glReq(gitlabCtx(cfg2), coRiding, gitlab.EventHeaderMergeRequest))
	if w4.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("authz error must not strand the co-riding resync: code=%d calls=%d body=%s", w4.Code, calls, w4.Body.String())
	}
}

// The merge-gate resync lane shares the closed-MR rule with the re-request
// lane (the round-1 guard's SIBLING term): a push to a closed/merged MR's
// source branch still delivers the update hook and must not burn a review.
// A payload WITHOUT a state stays fail-open — filtering it would strand the
// required check on the new head.
func TestGitLabWebhook_ResyncOnDeadMRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-123", nil
	}
	cfg := glConfig()
	cfg.ReviewOnSync = true

	resync := func(state, sha string) string {
		st := ""
		if state != "" {
			st = `"state": "` + state + `", `
		}
		return `{
		  "object_kind": "merge_request",
		  "user": {"username": "alice"},
		  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
		  "object_attributes": {"iid": 7, "action": "update", ` + st + `"oldrev": "prev", "source_branch": "feature/x", "target_branch": "main",
		    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
		    "last_commit": {"id": "` + sha + `"}}
		}`
	}
	for _, state := range []string{"closed", "merged", "locked"} {
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), resync(state, "sha-"+state), gitlab.EventHeaderMergeRequest))
		if w.Code != http.StatusOK || calls != 0 {
			t.Fatalf("state=%s: code=%d calls=%d body=%s", state, w.Code, calls, w.Body.String())
		}
	}
	// Open and state-less payloads keep the gate following the head.
	for i, state := range []string{"opened", ""} {
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), resync(state, fmt.Sprintf("sha-live-%d", i)), gitlab.EventHeaderMergeRequest))
		if w.Code != http.StatusAccepted || calls != i+1 {
			t.Fatalf("state=%q: code=%d calls=%d body=%s", state, w.Code, calls, w.Body.String())
		}
	}
}

// glNoteReq builds a request carrying the Note Hook event header.
func glNoteReq(ctx context.Context, body string) *http.Request {
	return glReq(ctx, body, gitlab.EventHeaderNote)
}

const glNoteRevi = `{
  "object_kind": "note",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "user": {"username": "alice"},
  "object_attributes": {"id": 99, "note": "/revi", "noteable_type": "MergeRequest", "discussion_id": "d-1", "author_id": 1},
  "merge_request": {"iid": 7, "state": "opened", "source_branch": "feature/x", "target_branch": "main",
    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
    "last_commit": {"id": "headsha"}}
}`

// TestGitLabNoteHook_ReviCommandLaunches pins the happy path for the
// /revi re-review command: bare `/revi` triggers a fresh review with
// re_review=true and scope_notes = the note body.
func TestGitLabNoteHook_ReviCommandLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef = botID, vars, repoURL, repoRef
		return "run-note-1", nil
	}
	cfg := glConfig()
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteRevi))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotBot != "review-pr" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["re_review"] != "true" {
		t.Fatalf("re_review flag missing: %v", gotVars)
	}
	// Conversational contract (forge-conversations A2): scope_notes
	// carries the MR context; the triggering note rides separately.
	if !strings.Contains(gotVars["scope_notes"], "Add X") {
		t.Fatalf("scope_notes should carry the MR title/desc: %v", gotVars["scope_notes"])
	}
	if gotVars["trigger_note"] != "/revi" || gotVars["trigger_command"] != "revi" {
		t.Fatalf("trigger vars: %v", gotVars)
	}
	if gotVars["conversation_mode"] != "reply" || gotVars["discussion_id"] != "d-1" || gotVars["replier"] != "alice" {
		t.Fatalf("conversation vars: %v", gotVars)
	}
	if gotURL != "https://gitlab.com/acme/widgets.git" || gotRef != "feature/x" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
}

// TestGitLabNoteHook_FocusArgTolerated pins that args after /revi are
// tolerated on a webhook whose scope does NOT include the converse bot:
// the handler falls back to today's review-pr re-review path with the
// args ignored. This is the back-compat half of the A5 routing.
func TestGitLabNoteHook_FocusArgTolerated(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-note-2", nil
	}
	cfg := glConfig()
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi focus=security"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("focus arg: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

// botsDirAbs returns the absolute path to the repo's bots/ directory
// from a test in pkg/server/. Used by the conversational-routing test
// to wire the bot registry so revi-converse resolves on disk.
func botsDirAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "bots"))
	if err != nil {
		t.Fatalf("resolve bots dir: %v", err)
	}
	return abs
}

// TestGitLabNoteHook_ConverseRoutesQuestionToConverseBot pins the A5
// conversational route: when an authorized user asks `/revi <question>`
// AND the webhook scope includes revi-converse AND the bot is
// resolvable on disk, the handler launches revi-converse (NOT
// review-pr) with the question threaded as `converse_question`. The
// re_review flag is dropped (it's a question, not a re-review).
func TestGitLabNoteHook_ConverseRoutesQuestionToConverseBot(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	var calls int
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-converse-1", nil
	}
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi why is the SSRF critical?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("converse route: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gotBot != "revi-converse" {
		t.Fatalf("expected revi-converse routing, got bot=%q", gotBot)
	}
	if gotVars["converse_question"] != "why is the SSRF critical?" {
		t.Fatalf("converse_question not threaded: %v", gotVars["converse_question"])
	}
	// Conversation context is carried for both routing paths.
	if gotVars["discussion_id"] != "d-1" || gotVars["replier"] != "alice" {
		t.Fatalf("conversation vars: %v", gotVars)
	}
	if gotVars["trigger_args"] != "why is the SSRF critical?" || gotVars["trigger_command"] != "revi" {
		t.Fatalf("trigger vars: %v", gotVars)
	}
	// re_review must be dropped on the converse path — it's a question,
	// not a fresh review.
	if _, present := gotVars["re_review"]; present {
		t.Fatalf("re_review flag must be dropped on the converse path: %v", gotVars)
	}
}

// TestGitLabNoteHook_ConverseFallsBackWhenBotMissing pins that even
// when the webhook scope ALLOWS revi-converse, if the bot bundle is
// NOT resolvable on disk (older deploy without the bundle) the handler
// gracefully falls back to the review-pr re-review path with the args
// ignored — same outcome as a webhook that doesn't scope the converse
// bot. The fallback keeps a /revi <question> note useful instead of
// erroring out the inbound webhook.
func TestGitLabNoteHook_ConverseFallsBackWhenBotMissing(t *testing.T) {
	s := newWebhookTestServer(t)
	// Point at an empty bot dir so revi-converse does not resolve.
	s.cfg.Bots.Paths = []string{t.TempDir()}
	var calls int
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-fallback-1", nil
	}
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "/revi why is the SSRF critical?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("fallback: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gotBot != "review-pr" {
		t.Fatalf("expected review-pr fallback, got bot=%q", gotBot)
	}
	if gotVars["re_review"] != "true" {
		t.Fatalf("re_review flag must be set on the fallback re-review path: %v", gotVars)
	}
	if _, present := gotVars["converse_question"]; present {
		t.Fatalf("converse_question must NOT be set on the fallback path: %v", gotVars)
	}
}

// TestGitLabNoteHook_ReplyInThreadRoutesToConverse pins the reply-in-thread
// trigger: a plain reply (NO /revi command) in a thread Revi is part of
// launches revi-converse with the reply body as the question. The real gate
// confirms "Revi is in this thread" via the GitLab discussions API; here the
// seam reports replyInThread=true so the handler routing is exercised.
func TestGitLabNoteHook_ReplyInThreadRoutesToConverse(t *testing.T) {
	s := newWebhookTestServer(t)
	s.cfg.Bots.Paths = []string{botsDirAbs(t)}
	s.webhookNoteGate = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (bool, bool, string, string, error) {
		// authorized + a reply in a Revi thread, with the fetched transcript.
		return true, true, "@revi (you, the bot):\nThe SSRF fix pins the host.\n\n---\n\n@alice:\nCan you expand on the SSRF fix?", "reply", nil
	}
	var calls int
	var gotBot string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars = botID, vars
		return "run-reply-1", nil
	}
	cfg := glConfig()
	cfg.BotIDs = []string{"review-pr", "revi-converse"}
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "Can you expand on the SSRF fix?"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("reply route: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	if gotBot != "revi-converse" {
		t.Fatalf("a reply in a Revi thread should route to revi-converse, got %q", gotBot)
	}
	if gotVars["converse_question"] != "Can you expand on the SSRF fix?" {
		t.Fatalf("converse_question should be the reply body: %q", gotVars["converse_question"])
	}
	// The discussion transcript the gate fetched must reach the bot as
	// thread_context — it grounds the answer in Revi's earlier note.
	if !strings.Contains(gotVars["thread_context"], "The SSRF fix pins the host.") {
		t.Fatalf("thread_context not threaded to launch vars: %q", gotVars["thread_context"])
	}
	if _, present := gotVars["re_review"]; present {
		t.Fatalf("re_review must be dropped on the converse path: %v", gotVars)
	}
}

// TestGitLabNoteHook_PlainCommentWithoutConverseBotFiltered pins that a
// non-/revi note on a webhook WITHOUT the converse bot enabled triggers
// nothing (the reply-in-thread feature is off → early-filtered, no gate
// call, no launch).
func TestGitLabNoteHook_PlainCommentWithoutConverseBotFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a plain comment without the converse bot must not launch")
		return "", nil
	}
	cfg := glConfig() // BotIDs = ["review-pr"] only → converse disabled
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "just a normal comment"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusOK {
		t.Fatalf("plain comment should be filtered (200): code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestGitLabNoteHook_QuotedMidTextDoesNotTrigger pins the anti-loop
// guardrail: "please run /revi" in the middle of a comment must NOT
// launch a review, otherwise reviewers casually quoting the command
// would re-trigger the bot forever.
func TestGitLabNoteHook_QuotedMidTextDoesNotTrigger(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("mid-text /revi must not trigger")
		return "", nil
	}
	cfg := glConfig()
	body := strings.Replace(glNoteRevi, `"note": "/revi"`, `"note": "please run /revi when you can"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusOK {
		t.Fatalf("mid-text /revi: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("should be filtered: %v", resp)
	}
}

// TestGitLabNoteHook_ClosedMRFiltered pins that /revi on a closed MR is
// filtered (no re-review on a merged/closed MR).
func TestGitLabNoteHook_ClosedMRFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("closed MR must not re-review")
		return "", nil
	}
	cfg := glConfig()
	body := strings.Replace(glNoteRevi, `"state": "opened"`, `"state": "closed"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body))
	if w.Code != http.StatusOK {
		t.Fatalf("closed: code=%d", w.Code)
	}
}

// TestGitLabNoteHook_IdempotencyByNoteID pins both halves of the note
// idempotency contract: replay of the same delivery (same note id)
// dedupes, but a SECOND /revi on the same MR (new note id) launches
// fresh.
func TestGitLabNoteHook_IdempotencyByNoteID(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-note-N", nil
	}
	cfg := glConfig()
	// First /revi
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteRevi))
	if w.Code != http.StatusAccepted {
		t.Fatalf("first: %d body=%s", w.Code, w.Body.String())
	}
	// Same delivery replayed → duplicate (200), no new launch
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteRevi))
	if w.Code != http.StatusOK {
		t.Fatalf("replay: %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("expected duplicate: %v", resp)
	}
	// Fresh note (different id) on same MR → new launch (calls bumps).
	body2 := strings.Replace(glNoteRevi, `"id": 99`, `"id": 100`, 1)
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), body2))
	if w.Code != http.StatusAccepted {
		t.Fatalf("second /revi: %d body=%s", w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Fatalf("expected two launches across two distinct notes, got %d", calls)
	}
}

// TestGitLabWebhook_MROpenAndNoteCoexist pins that the MR open key
// (mr|…) and the note key (note|…) do NOT collide for the same tenant
// + webhook + MR — both can land back-to-back without one looking like
// a replay of the other.
func TestGitLabWebhook_MROpenAndNoteCoexist(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "ok", nil
	}
	cfg := glConfig()
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusAccepted {
		t.Fatalf("mr open: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(cfg), glNoteRevi))
	if w.Code != http.StatusAccepted {
		t.Fatalf("note coexisting w/ mr-open: %d body=%s", w.Code, w.Body.String())
	}
	if calls != 2 {
		t.Fatalf("mr-open + note must each launch independently, got %d", calls)
	}
}

func TestGitLabWebhook_FiltersAndRejects(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "r", nil
	}
	cfg := glConfig()

	// label-only update (no oldrev) → 200 filtered, no launch
	labelEdit := strings.Replace(glOpenMR, `"action": "open"`, `"action": "update"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), labelEdit, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusOK {
		t.Fatalf("label edit code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("expected filtered, got %v", resp)
	}

	// project allowlist mismatch → filtered
	cfg2 := glConfig()
	cfg2.ProjectAllowlist = []string{"other/*"}
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg2), glOpenMR, gitlab.EventHeaderMergeRequest))
	json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("project mismatch: code=%d resp=%v", w.Code, resp)
	}

	// wrong event header → 400
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), glOpenMR, "Push Hook"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("wrong header code=%d", w.Code)
	}

	// malformed payload → 400
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), `{bad`, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed code=%d", w.Code)
	}

	if calls != 0 {
		t.Fatalf("no launch should have happened, got %d", calls)
	}
}

// raceMissDeliveries simulates the concurrent-duplicate race: the
// step-1 replay check misses for the first `forcedMisses` lookups (as
// it does when two deliveries of the same event are in flight at
// once), then delegates to the real store.
type raceMissDeliveries struct {
	webhooks.DeliveryStore
	forcedMisses int
}

func (r *raceMissDeliveries) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	if r.forcedMisses > 0 {
		r.forcedMisses--
		return webhooks.Delivery{}, webhooks.ErrNotFound
	}
	return r.DeliveryStore.GetByIdempotencyKey(ctx, key)
}

// TestGitLabWebhook_ConcurrentDuplicateReleasesQuota reproduces the
// double-metering race: two deliveries of the same event both pass the
// step-1 replay check and both charge the monthly run counter, but only
// the idempotency-insert winner launches. The loser must release its
// quota unit — the counter ends at exactly one metered run.
func TestGitLabWebhook_ConcurrentDuplicateReleasesQuota(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeliveries = &raceMissDeliveries{DeliveryStore: webhooks.NewMemoryDeliveryStore(), forcedMisses: 2}
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	now := time.Now().UTC()
	if _, err := s.authStore().CreateOrg(context.Background(), identity.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{ID: "t1", OrgID: "o1", Name: "t1", Slug: "t1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		return "run-1", nil
	}
	cfg := glConfig()

	w1 := httptest.NewRecorder()
	s.handleGitLabWebhook(w1, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first delivery: code=%d body=%s", w1.Code, w1.Body.String())
	}
	// Second delivery of the SAME event; its replay check misses (forced),
	// so it meters, then loses the idempotency insert.
	w2 := httptest.NewRecorder()
	s.handleGitLabWebhook(w2, glReq(gitlabCtx(cfg), glOpenMR, gitlab.EventHeaderMergeRequest))
	if w2.Code != http.StatusOK {
		t.Fatalf("duplicate delivery: code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate || resp["run_id"] != "run-1" {
		t.Fatalf("duplicate resp: %v", resp)
	}
	// The org's monthly counter must reflect ONE launch, not two.
	u, err := counter.Usage(context.Background(), "o1", now)
	if err != nil {
		t.Fatal(err)
	}
	if u.Runs != 1 {
		t.Fatalf("org monthly runs = %d, want 1 (loser must release its quota unit)", u.Runs)
	}
}
