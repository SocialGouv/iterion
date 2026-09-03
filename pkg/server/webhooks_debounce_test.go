package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// ghSyncPayload is a GitHub `synchronize` delivery (a push to the PR head).
func ghSyncPayload(sha string) string {
	return `{
	  "action": "synchronize", "number": 7,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 7, "title": "T", "body": "b",
	    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
	    "head": {"ref": "feature/x", "sha": "` + sha + `"}, "base": {"ref": "main"}},
	  "sender": {"login": "alice"}
	}`
}

// A push volley must cost ONE review of the FINAL head: each synchronize
// parks the launch for the quiet window, a newer push replaces the parked
// payload, and only the sweep — after the window — launches. This is the
// debounce that replaces "launch immediately, cancel mid-flight on the
// next push" (~18% of a production morning's review runs died superseded
// with their tokens already spent).
func TestSyncDebounce_VolleyLaunchesOnlyFinalHead(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	deliver := func(sha string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload(sha), prforge.EventHeaderPullRequest, pt))
		return w
	}

	w := deliver("sha-1")
	if w.Code != 202 {
		t.Fatalf("deferred delivery must answer 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["status"] != webhooks.StatusDeferred {
		t.Fatalf("response = %s, want status=deferred", w.Body.String())
	}
	deliver("sha-2")
	if len(*got) != 0 {
		t.Fatalf("nothing may launch during the quiet window, got %v", botsOf(*got))
	}

	// Before the window elapses the sweep must not fire.
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(1*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("sweep fired inside the quiet window: %v", botsOf(*got))
	}

	// After the window: exactly one launch, of the FINAL head.
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("want exactly one launch of the final head, got %v", botsOf(*got))
	}
	if sha := (*got)[0].vars["head_sha"]; sha != "sha-2" {
		t.Fatalf("launched head = %q, want the volley's final sha-2", sha)
	}

	// The row is acknowledged: a second sweep launches nothing more.
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(10*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("acknowledged row re-fired: %v", botsOf(*got))
	}
}

// A PR OPEN is a human waiting for an answer — never debounced, even with
// the window configured.
func TestSyncDebounce_OpenLaunchesImmediately(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true

	open := `{
	  "action": "opened", "number": 7,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 7, "title": "T", "body": "b",
	    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
	    "head": {"ref": "feature/x", "sha": "sha-1"}, "base": {"ref": "main"}},
	  "sender": {"login": "alice"}
	}`
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), open, prforge.EventHeaderPullRequest, pt))
	if len(*got) != 1 {
		t.Fatalf("a PR open must launch immediately, got %v", botsOf(*got))
	}
}

// The store's debounce contract: a re-upsert replaces the payload, bumps
// the generation and clears any sweep lease; Delete only acknowledges the
// generation it launched, so a subject that re-armed mid-claim survives.
func TestMemoryDeferredLaunchStore_Semantics(t *testing.T) {
	st := webhooks.NewMemoryDeferredLaunchStore()
	ctx := context.Background()
	now := time.Now().UTC()

	d := webhooks.DeferredLaunch{
		SubjectKey: "t1|w1|pr:7", TenantID: "t1", WebhookID: "w1",
		FireAt: now.Add(-time.Second), CreatedAt: now,
		Targets: []webhooks.DeferredTarget{{BotID: "review-pr", IdemKey: "k1"}},
	}
	if err := st.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}

	// Claim leases the row: a concurrent claim sees nothing.
	due, err := st.ClaimDue(ctx, now, 2*time.Minute, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim: %v %v", due, err)
	}
	if again, _ := st.ClaimDue(ctx, now, 2*time.Minute, 10); len(again) != 0 {
		t.Fatalf("leased row re-claimed: %v", again)
	}

	// Re-arm mid-claim: fresh payload, cleared lease, bumped generation.
	d.FireAt = now.Add(-time.Millisecond)
	d.Targets = []webhooks.DeferredTarget{{BotID: "review-pr", IdemKey: "k2"}}
	if err := st.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	// Acknowledging the OLD generation must not drop the re-armed row.
	if err := st.Delete(ctx, due[0].SubjectKey, due[0].Generation); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ClaimDue(ctx, now, 2*time.Minute, 10)
	if len(fresh) != 1 || fresh[0].Targets[0].IdemKey != "k2" {
		t.Fatalf("re-armed subject must survive the old ack and carry the fresh payload, got %+v", fresh)
	}
	if fresh[0].Generation <= due[0].Generation {
		t.Fatalf("generation must bump on re-arm: %d then %d", due[0].Generation, fresh[0].Generation)
	}

	// Acknowledging the LAUNCHED generation removes the row for good.
	if err := st.Delete(ctx, fresh[0].SubjectKey, fresh[0].Generation); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ClaimDue(ctx, now.Add(10*time.Minute), 2*time.Minute, 10); len(left) != 0 {
		t.Fatalf("acknowledged row still claimable: %v", left)
	}
}

// A lapsed lease re-offers the row (the claimer died mid-launch): the
// at-least-once half of the contract — the launch tail's idempotency key
// is what turns the replay of an already-launched target into a no-op.
func TestMemoryDeferredLaunchStore_LeaseLapsesAndRefires(t *testing.T) {
	st := webhooks.NewMemoryDeferredLaunchStore()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := st.Upsert(ctx, webhooks.DeferredLaunch{
		SubjectKey: "t1|w1|pr:8", FireAt: now.Add(-time.Second), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.ClaimDue(ctx, now, time.Minute, 10); len(due) != 1 {
		t.Fatalf("first claim: %v", due)
	}
	if due, _ := st.ClaimDue(ctx, now.Add(2*time.Minute), time.Minute, 10); len(due) != 1 {
		t.Fatalf("lapsed lease must re-offer the row, got %v", due)
	}
}

// Two same-numbered PRs on two repos of ONE webhook must park under two
// distinct keys and both fire — the R46e1fb regression: a repo-less
// subject id ("pr:7") let a push to acme/b#7 replace acme/a#7's parked
// review, which then never launched and never retried.
func TestSyncDebounce_CrossRepoSamePRNumberIsolated(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	syncOn := func(repo, sha string) string {
		return `{
		  "action": "synchronize", "number": 7,
		  "repository": {"id": 42, "full_name": "` + repo + `", "clone_url": "https://github.com/` + repo + `.git"},
		  "pull_request": {"number": 7, "title": "T", "body": "b",
		    "html_url": "https://github.com/` + repo + `/pull/7", "state": "open",
		    "head": {"ref": "feature/x", "sha": "` + sha + `"}, "base": {"ref": "main"}},
		  "sender": {"login": "alice"}
		}`
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), syncOn("acme/a", "sha-a"), prforge.EventHeaderPullRequest, pt))
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), syncOn("acme/b", "sha-b"), prforge.EventHeaderPullRequest, pt))

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 2 {
		t.Fatalf("both repos' PRs must launch, got %d: %v", len(*got), botsOf(*got))
	}
	shas := map[string]bool{}
	for _, rec := range *got {
		shas[rec.vars["head_sha"]] = true
	}
	if !shas["sha-a"] || !shas["sha-b"] {
		t.Fatalf("each repo must launch its own head, got %v", shas)
	}
}

// A PR closed (merged) inside its quiet window must purge the parked
// review — the R59ff42 regression: the sweep otherwise fires a full
// review of a dead pull request minutes after it merged.
func TestSyncDebounce_ClosedPRPurgesParkedLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	closed := `{
	  "action": "closed", "number": 7,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 7, "title": "T", "body": "b", "merged": true,
	    "html_url": "https://github.com/acme/widgets/pull/7", "state": "closed",
	    "head": {"ref": "feature/x", "sha": "sha-1"}, "base": {"ref": "main"}},
	  "sender": {"login": "alice"}
	}`
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), closed, prforge.EventHeaderPullRequest, pt))

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(10*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("a closed PR's parked review must never fire, got %v", botsOf(*got))
	}
}

// The supersede pass must be scoped by project too: a push to acme/b#7
// must not cancel the live review of acme/a#7 riding the same webhook —
// the pre-existing sibling of the R46e1fb key collision.
func TestSupersede_ScopedByProject(t *testing.T) {
	s := newWebhookTestServer(t)
	got := fanoutLauncher(s)
	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.Overlap = schedgate.OverlapSupersede

	openOn := func(repo, sha string) string {
		return `{
		  "action": "opened", "number": 7,
		  "repository": {"id": 42, "full_name": "` + repo + `", "clone_url": "https://github.com/` + repo + `.git"},
		  "pull_request": {"number": 7, "title": "T", "body": "b",
		    "html_url": "https://github.com/` + repo + `/pull/7", "state": "open",
		    "head": {"ref": "feature/x", "sha": "` + sha + `"}, "base": {"ref": "main"}},
		  "sender": {"login": "alice"}
		}`
	}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), openOn("acme/a", "sha-a"), prforge.EventHeaderPullRequest, pt))
	w = httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), openOn("acme/b", "sha-b"), prforge.EventHeaderPullRequest, pt))

	if len(*got) != 2 {
		t.Fatalf("both PRs must launch, got %v", botsOf(*got))
	}
	if len(cancelled) != 0 {
		t.Fatalf("a same-numbered PR of ANOTHER repo must not be superseded, cancelled %v", cancelled)
	}
}

// A nil request through the denial-response path (the defer-failure
// fallback fires with the inbound request now, but the guard is the
// belt) must answer, never panic.
func TestReflectAllowedOrigin_NilRequestIsNoop(t *testing.T) {
	s := newWebhookTestServer(t)
	w := httptest.NewRecorder()
	s.reflectAllowedOrigin(w, nil)
	if h := w.Header().Get("Access-Control-Allow-Origin"); h != "" {
		t.Fatalf("nil request must reflect nothing, got %q", h)
	}
}
