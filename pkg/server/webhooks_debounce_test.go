package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
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
	if ok, err := st.Upsert(ctx, d); err != nil || !ok {
		t.Fatalf("upsert: ok=%v err=%v", ok, err)
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
	if ok, err := st.Upsert(ctx, d); err != nil || !ok {
		t.Fatalf("re-upsert: ok=%v err=%v", ok, err)
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

// Reschedule is the retry half of the claim contract, and it is
// generation-guarded for the same reason Delete is: a fresh push that
// landed mid-launch must keep its payload. A stale re-arm must also
// never RESURRECT a row a closed PR purged.
func TestMemoryDeferredLaunchStore_RescheduleIsGenerationGuarded(t *testing.T) {
	st := webhooks.NewMemoryDeferredLaunchStore()
	ctx := context.Background()
	now := time.Now().UTC()
	row := func(idem string) webhooks.DeferredLaunch {
		return webhooks.DeferredLaunch{
			SubjectKey: "t1|w1|acme/a|pr:7", FireAt: now.Add(-time.Second), CreatedAt: now,
			Targets: []webhooks.DeferredTarget{{BotID: "review-pr", IdemKey: idem}},
		}
	}
	if ok, err := st.Upsert(ctx, row("k1")); err != nil || !ok {
		t.Fatalf("upsert: ok=%v err=%v", ok, err)
	}
	claimed, _ := st.ClaimDue(ctx, now, 2*time.Minute, 10)
	if len(claimed) != 1 {
		t.Fatalf("claim: %v", claimed)
	}
	// A fresh push lands while the claimer is launching.
	if ok, err := st.Upsert(ctx, row("k2")); err != nil || !ok {
		t.Fatalf("re-upsert: ok=%v err=%v", ok, err)
	}
	// The claimer's launch failed and re-arms its OWN generation: the
	// fresh payload must survive, unchanged and immediately due.
	if err := st.Reschedule(ctx, claimed[0].SubjectKey, claimed[0].Generation, now.Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ClaimDue(ctx, now, 2*time.Minute, 10)
	if len(fresh) != 1 || fresh[0].Targets[0].IdemKey != "k2" {
		t.Fatalf("a stale re-arm clobbered the fresh push: %+v", fresh)
	}
	if fresh[0].Attempts != 0 {
		t.Fatalf("the fresh payload must keep its full retry budget, attempts=%d", fresh[0].Attempts)
	}
	// Re-arming the CURRENT generation does take effect.
	if err := st.Reschedule(ctx, fresh[0].SubjectKey, fresh[0].Generation, now.Add(time.Hour), 3); err != nil {
		t.Fatal(err)
	}
	if due, _ := st.ClaimDue(ctx, now.Add(time.Minute), 2*time.Minute, 10); len(due) != 0 {
		t.Fatalf("a re-armed row must not be due before its new FireAt: %v", due)
	}
	due, _ := st.ClaimDue(ctx, now.Add(2*time.Hour), 2*time.Minute, 10)
	if len(due) != 1 || due[0].Attempts != 3 {
		t.Fatalf("re-armed row = %+v, want one due row carrying attempts=3", due)
	}
	// A purged subject (closed PR) must stay purged.
	if err := st.DeleteBySubject(ctx, due[0].SubjectKey); err != nil {
		t.Fatal(err)
	}
	if err := st.Reschedule(ctx, due[0].SubjectKey, due[0].Generation, now, 4); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.ClaimDue(ctx, now.Add(3*time.Hour), 2*time.Minute, 10); len(left) != 0 {
		t.Fatalf("a re-arm resurrected a purged subject: %v", left)
	}
}

// A lapsed lease re-offers the row (the claimer died mid-launch): the
// at-least-once half of the contract — the launch tail's idempotency key
// is what turns the replay of an already-launched target into a no-op.
func TestMemoryDeferredLaunchStore_LeaseLapsesAndRefires(t *testing.T) {
	st := webhooks.NewMemoryDeferredLaunchStore()
	ctx := context.Background()
	now := time.Now().UTC()

	if ok, err := st.Upsert(ctx, webhooks.DeferredLaunch{
		SubjectKey: "t1|w1|pr:8", FireAt: now.Add(-time.Second), CreatedAt: now,
	}); err != nil || !ok {
		t.Fatalf("upsert: ok=%v err=%v", ok, err)
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

// A REDELIVERY of a synchronize whose parked launch already fired must
// not cancel the run it itself launched — the R0cca8b regression. The
// immediate lane orders replay-check BEFORE supersede for exactly this
// reason; the defer lane used to supersede first, so a redelivery
// cancelled the live review, re-parked under the same idempotency key,
// and the sweep answered `duplicate` — the review dead, the required
// check absent forever, nothing left to retry it.
func TestSyncDebounce_RedeliveryDoesNotKillItsOwnRun(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	cfg.Overlap = schedgate.OverlapSupersede
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	deliver := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))
		return w
	}

	deliver()
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("the parked launch must fire once, got %v", botsOf(*got))
	}

	// The redelivery: same event, same idempotency key, the run it
	// launched still live.
	w := deliver()
	if w.Code != 200 {
		t.Fatalf("a redelivery of an already-launched head must answer 200 duplicate, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("response = %s, want status=duplicate", w.Body.String())
	}
	if len(cancelled) != 0 {
		t.Fatalf("a redelivery must not supersede the run it itself launched, cancelled %v", cancelled)
	}
	// And it parked nothing: no second launch, ever.
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(20*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("the redelivery re-parked and re-fired: %v", botsOf(*got))
	}
}

// The config as it stands AT FIRE TIME governs the parked launch. The
// quiet window is real time an operator acts in, and the sweep re-enters
// none of the admission the inbound request passed — the `!Enabled →
// 410` guard lives in the auth middleware. A webhook switched off (or a
// review_on_sync cleared) inside the window used to still get its run.
func TestSyncDebounce_ConfigTurnedOffInsideWindowDropsParkedLaunch(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  func(*webhooks.Config)
	}{
		{"webhook disabled", func(c *webhooks.Config) { c.Enabled = false }},
		{"review_on_sync cleared", func(c *webhooks.Config) { c.ReviewOnSync = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if w.Code != 202 {
				t.Fatalf("synchronize must defer (202), got %d: %s", w.Code, w.Body.String())
			}

			// The operator acts inside the quiet window.
			tc.off(&cfg)
			if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}

			s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
			if len(*got) != 0 {
				t.Fatalf("a parked review must not outlive the switch that turned it off, got %v", botsOf(*got))
			}
			// And the row is gone, not re-claimed every 20s until the TTL.
			s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(30*time.Minute))
			if len(*got) != 0 {
				t.Fatalf("dropped row re-fired: %v", botsOf(*got))
			}
		})
	}
}

// Bot scope is re-read at fire time too: a bot removed from BotIDs while
// the row sat parked must not launch.
func TestSyncDebounce_BotOutOfScopeAtFireTimeIsSkipped(t *testing.T) {
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

	cfg.BotIDs = []string{"some-other-bot"}
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("a bot removed from the webhook's scope must not launch, got %v", botsOf(*got))
	}
}

// A parked launch that FAILS must be re-armed, not deleted — the
// Rb58c20 regression. The forge was answered `202 deferred` minutes ago,
// so the immediate lane's recovery (hand back the denial's 4xx, let the
// forge redeliver) does not exist here: deleting the row drops the
// promised review with no retry and no visible failure.
func TestSyncDebounce_FailedLaunchIsRetriedNotDropped(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	var attempts int
	fail := true
	s.webhookLaunchBot = func(_ context.Context, bot string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		attempts++
		if fail {
			return "", errors.New("bot temporarily broken")
		}
		return "run-" + bot, nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	base := time.Now().UTC()
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(4*time.Minute))
	if attempts != 1 {
		t.Fatalf("the parked launch must be attempted once, got %d", attempts)
	}

	// Re-armed: the next sweep past the backoff retries it, and this time
	// the launch succeeds.
	fail = false
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(20*time.Minute))
	if attempts != 2 {
		t.Fatalf("a failed parked launch must be retried, attempts=%d", attempts)
	}
	// Launched: the row is acknowledged and never fires again.
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(2*time.Hour))
	if attempts != 2 {
		t.Fatalf("a launched row re-fired, attempts=%d", attempts)
	}
}

// The retry chain is BOUNDED, and its end is audited: a review that can
// never launch must be abandoned loudly (a launch_error delivery naming
// the loss), never deleted in silence.
func TestSyncDebounce_RetryChainIsBoundedAndAudited(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	var attempts int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		attempts++
		return "", errors.New("bot permanently broken")
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	base := time.Now().UTC()
	for i := 1; i <= webhookDeferMaxAttempts+4; i++ {
		s.sweepDeferredWebhookLaunches(context.Background(), base.Add(time.Duration(i)*time.Hour))
	}
	if attempts != webhookDeferMaxAttempts {
		t.Fatalf("retries = %d, want the chain capped at %d", attempts, webhookDeferMaxAttempts)
	}
	ds, err := s.webhookDeliveries.ListByWebhook(context.Background(), cfg.TenantID, cfg.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var abandoned bool
	for _, d := range ds {
		if d.Status == webhooks.StatusLaunchError && strings.Contains(d.Error, "abandoned") {
			abandoned = true
		}
	}
	if !abandoned {
		t.Fatal("the abandoned review must leave an audited launch_error delivery, not vanish")
	}
}

// The retry horizon: a transient denial (concurrency, launch rate) is
// waited out on its own Retry-After; a monthly quota/cost denial resets
// next MONTH — parking the row that long buys nothing (the head is
// stale long before) and hides the loss, so it is terminal.
func TestDeferRetryWait(t *testing.T) {
	if wait, ok := deferRetryWait(nil, 0); !ok || wait != webhookDeferRetryBase {
		t.Fatalf("a plain launch failure must retry at the base delay, got %s ok=%v", wait, ok)
	}
	if wait, ok := deferRetryWait(&launchDenial{retryAfter: 90 * time.Second}, 0); !ok || wait != 90*time.Second {
		t.Fatalf("the denial's own Retry-After must floor the wait, got %s ok=%v", wait, ok)
	}
	if wait, ok := deferRetryWait(&launchDenial{retryAfter: time.Hour}, 0); !ok || wait != webhookDeferRetryMax {
		t.Fatalf("the wait must stay capped, got %s ok=%v", wait, ok)
	}
	if _, ok := deferRetryWait(nil, webhookDeferMaxAttempts-1); ok {
		t.Fatal("the chain must be bounded by the attempt cap")
	}
	monthly := &launchDenial{resetAt: nextMonthStart(time.Now().UTC())}
	if _, ok := deferRetryWait(monthly, 0); ok {
		t.Fatal("a monthly-cap denial must be terminal, not parked for weeks")
	}
}

// ghSyncPayloadAt is a synchronize delivery carrying the forge's own
// event timestamp — the only ordering signal a webhook delivery has.
func ghSyncPayloadAt(sha, updatedAt string) string {
	return `{
	  "action": "synchronize", "number": 7,
	  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 7, "title": "T", "body": "b",
	    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
	    "updated_at": "` + updatedAt + `",
	    "head": {"ref": "feature/x", "sha": "` + sha + `"}, "base": {"ref": "main"}},
	  "sender": {"login": "alice"}
	}`
}

// The parked payload must be the NEWEST head, not the last-arrived one —
// the R4f7eab regression. Forges do not guarantee delivery order, and a
// retried or slow delivery landing after a later one is exactly this
// feature's regime. Replacing by arrival parked the STALE head: the
// sweep reviewed it and posted `revi/review` on a commit that was no
// longer the head, leaving the real head with no status.
func TestSyncDebounce_OutOfOrderDeliveryDoesNotParkTheStaleHead(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	var cancelled []string
	s.webhookCancelRun = func(runID string) error {
		cancelled = append(cancelled, runID)
		return nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	cfg.Overlap = schedgate.OverlapSupersede
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	deliver := func(sha, at string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayloadAt(sha, at), prforge.EventHeaderPullRequest, pt))
		return w
	}

	deliver("sha-new", "2026-09-03T10:15:30Z")
	// The OLDER push's delivery arrives late (a forge retry, a slow
	// dispatch): it must neither replace the parked payload nor supersede.
	w := deliver("sha-old", "2026-09-03T10:15:00Z")
	if w.Code != 200 {
		t.Fatalf("an out-of-order delivery must be filtered (200), got %d: %s", w.Code, w.Body.String())
	}
	if len(cancelled) != 0 {
		t.Fatalf("a stale delivery must not supersede a newer head's run, cancelled %v", cancelled)
	}

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("want exactly one launch, got %v", botsOf(*got))
	}
	if sha := (*got)[0].vars["head_sha"]; sha != "sha-new" {
		t.Fatalf("launched head = %q, want the NEWEST head sha-new", sha)
	}
}

// The ordering rule itself: only two present keys order anything, and
// equal keys are not stale (a forge stamping two events in the same
// second must keep the arrival-order outcome, never drop the second).
func TestDeferredPayloadIsStale(t *testing.T) {
	for _, tc := range []struct {
		incoming, stored string
		want             bool
	}{
		{"2026-09-03T10:15:00Z", "2026-09-03T10:15:30Z", true},
		{"2026-09-03T10:15:30Z", "2026-09-03T10:15:00Z", false},
		{"2026-09-03T10:15:30Z", "2026-09-03T10:15:30Z", false},
		{"", "2026-09-03T10:15:30Z", false},
		{"2026-09-03T10:15:00Z", "", false},
		{"2026-09-03 10:15:00 UTC", "2026-09-03 10:15:30 UTC", true}, // GitLab's shape
	} {
		if got := webhooks.DeferredPayloadIsStale(tc.incoming, tc.stored); got != tc.want {
			t.Errorf("IsStale(%q, %q) = %v, want %v", tc.incoming, tc.stored, got, tc.want)
		}
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

// The GitLab twin of the closed-PR purge (Rd26f8f): a push parks a
// review, the MR merges inside the quiet window — the parked launch must
// purge, not fire a full review of a dead merge request at T+3m.
func TestSyncDebounce_GitLabMergedMRPurgesParkedLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg := glConfig()
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	push := `{
	  "object_kind": "merge_request",
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "update", "state": "opened", "oldrev": "sha0", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "last_commit": {"id": "sha1"}}
	}`
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), push, gitlab.EventHeaderMergeRequest))
	if w.Code != 202 {
		t.Fatalf("synchronize must defer (202), got %d: %s", w.Code, w.Body.String())
	}

	merged := `{
	  "object_kind": "merge_request",
	  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
	  "object_attributes": {"iid": 7, "action": "merge", "state": "merged", "source_branch": "feature/x", "target_branch": "main",
	    "title": "Add X", "description": "desc", "url": "https://gitlab.com/acme/widgets/-/merge_requests/7",
	    "last_commit": {"id": "sha1"}}
	}`
	w = httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(cfg), merged, gitlab.EventHeaderMergeRequest))

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(10*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("a merged MR's parked review must never fire, got %v", botsOf(*got))
	}
}
