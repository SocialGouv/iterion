package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// ghSyncPayload is a GitHub `synchronize` delivery (a push to the PR head).
func ghSyncPayload(sha string) string {
	return ghSyncPayloadRepo("acme/widgets", sha)
}

// ghSyncPayloadRepo is the same delivery for an arbitrary repository —
// one webhook config routinely serves many (ProjectAllowlist, an
// org-level hook), and PR numbers collide across them.
func ghSyncPayloadRepo(repo, sha string) string {
	return `{
	  "action": "synchronize", "number": 7,
	  "repository": {"id": 42, "full_name": "` + repo + `", "clone_url": "https://github.com/` + repo + `.git"},
	  "pull_request": {"number": 7, "title": "T", "body": "b",
	    "html_url": "https://github.com/` + repo + `/pull/7", "state": "open",
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

// One webhook serves MANY projects, and a subject id carries no repo
// ("pr:7" on both). Keyed on the subject alone, a push to acme/b#7
// replaces the parked review of acme/a#7 — which then never launches and
// never retries (the defer lane writes no delivery row, so nothing
// notices). Both reviews must survive and both must fire.
func TestSyncDebounce_SamePRNumberInTwoProjectsDoNotCollide(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	for _, repo := range []string{"acme/a", "acme/b"} {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayloadRepo(repo, "sha-"+repo), prforge.EventHeaderPullRequest, pt))
		if w.Code != 202 {
			t.Fatalf("%s: deferred delivery must answer 202, got %d: %s", repo, w.Code, w.Body.String())
		}
	}

	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 2 {
		t.Fatalf("both projects' #7 must be reviewed, got %d launch(es): %v", len(*got), *got)
	}
	seen := map[string]bool{}
	for _, rec := range *got {
		seen[rec.vars["head_sha"]] = true
	}
	if !seen["sha-acme/a"] || !seen["sha-acme/b"] {
		t.Fatalf("one project's review was dropped by a key collision; launched heads = %v", seen)
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

// failingDeferredStore refuses every park, driving deferSyncLaunch onto
// its "launch immediately instead" fallback.
type failingDeferredStore struct{ webhooks.DeferredLaunchStore }

func (failingDeferredStore) Upsert(context.Context, webhooks.DeferredLaunch) error {
	return errors.New("store down")
}
func (failingDeferredStore) ClaimDue(context.Context, time.Time, time.Duration, int) ([]webhooks.DeferredLaunch, error) {
	return nil, nil
}
func (failingDeferredStore) Delete(context.Context, string, int64) error { return nil }

// When the park fails, the defer lane falls back to launching inline —
// and that fallback WRITES AN HTTP RESPONSE. A launch-gate denial
// (monthly quota, cost cap, suspended team — routine on a busy tenant)
// runs writeLaunchDenial → reflectAllowedOrigin, which dereferences the
// request: handed nil, the forge got a dropped connection instead of an
// answer and the delivery was lost with no delivery row to retry from.
func TestSyncDebounce_ParkFailureDenialAnswersInsteadOfPanicking(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = failingDeferredStore{}
	s.syncDebounce = 3 * time.Minute
	fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// A suspended team is the cheapest real denial gateLaunch produces.
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "t1", Name: "t1", Slug: "t1", Status: identity.TeamStatusSuspended, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusForbidden {
		t.Fatalf("a denied fallback launch must answer 403, got %d: %s", w.Code, w.Body.String())
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
