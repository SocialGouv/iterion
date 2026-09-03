package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
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

// PublicURL is optional in this codebase — several paths deliberately
// degrade without it — and on such a deployment the immediate lane derives
// its publish base from the inbound delivery's own Host + TLS state. The
// sweep has no request at all, so a parked review would launch with no
// forge_publish_token: no PR comments, no revi/review commit status, a
// required check absent on every synchronize. The base is resolved at park
// time and CARRIED, scheme included.
func TestSyncDebounce_ParkedLaunchKeepsThePublishGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tls      bool
		wantBase string
	}{
		{"plain HTTP behind a proxy", false, "http://hooks.example"},
		{"TLS terminated in-process", true, "https://hooks.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			s.cfg.PublicURL = "" // the deployment that must degrade gracefully
			s.forgeConnections = forge.NewMemoryConnectionStore()
			s.forgePublishTokens = NewForgePublishTokenRegistry()
			if err := s.forgeConnections.Create(context.Background(), forge.Connection{
				ID: "conn1", TenantID: "t1", Provider: forge.ProviderGitHub,
			}); err != nil {
				t.Fatal(err)
			}
			s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
			s.syncDebounce = 3 * time.Minute
			got := fanoutLauncher(s)
			cfg, pt := ghConfig(t, s)
			cfg.ReviewOnSync = true
			if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}

			req := ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt)
			req.Host = "hooks.example"
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			s.handleGitHubWebhook(httptest.NewRecorder(), req)

			s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
			if len(*got) != 1 {
				t.Fatalf("want one parked launch, got %v", botsOf(*got))
			}
			vars := (*got)[0].vars
			if vars[forgePublishVarToken] == "" {
				t.Fatal("the parked review launched with NO publish grant — it would post no comments and no commit status")
			}
			if want := tc.wantBase + "/api/v1/forge/publish-review"; vars[forgePublishVarURL] != want {
				t.Fatalf("publish url = %q, want %q (the base the inbound delivery resolved, scheme included)", vars[forgePublishVarURL], want)
			}
		})
	}
}

// flakyConfigStore makes Get fail with a NON-not-found error (a decode
// failure, a server-selection timeout) for the first n reads.
type flakyConfigStore struct {
	webhooks.ConfigStore
	fails int
}

func (f *flakyConfigStore) Get(ctx context.Context, id string) (webhooks.Config, error) {
	if f.fails > 0 {
		f.fails--
		return webhooks.Config{}, errors.New("server selection timeout")
	}
	return f.ConfigStore.Get(ctx, id)
}

// A TRANSIENT config-store error is not "the webhook is gone". Deleting on
// it destroys the parked review with no delivery row and no retry — the
// review silently lost, the required check never landing. The row must
// survive and the next sweep must launch it. (Only webhooks.ErrNotFound —
// a genuinely deleted config — is terminal.)
func TestSyncDebounce_TransientConfigErrorKeepsTheParkedLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	s.handleGitHubWebhook(httptest.NewRecorder(), ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	s.webhookConfigs = &flakyConfigStore{ConfigStore: s.webhookConfigs, fails: 1}
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("nothing may launch while the config is unreadable: %v", botsOf(*got))
	}

	// The lease lapses; the next sweep reads the config and launches.
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(10*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("the parked review was destroyed by a transient error — got %d launch(es)", len(*got))
	}
}

// The operator's kill switch must hold RETROACTIVELY: inbound deliveries
// are refused with 410 once a webhook is disabled, so a row parked just
// before must not launch minutes later. That is a control-plane bypass,
// not merely wasted spend.
func TestSyncDebounce_DisabledWebhookDropsTheParkedLaunch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	got := fanoutLauncher(s)
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.handleGitHubWebhook(httptest.NewRecorder(), ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	cfg.Enabled = false
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(4*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("a disabled webhook must not launch its parked review: %v", botsOf(*got))
	}
	// And the row is gone, not left to be re-claimed every 20s forever.
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.sweepDeferredWebhookLaunches(context.Background(), time.Now().UTC().Add(10*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("the dropped row came back: %v", botsOf(*got))
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
func (failingDeferredStore) RescheduleFailed(context.Context, string, int64, time.Time) error {
	return nil
}

// The deferred lane answered the forge 202 at park time and wrote no
// delivery row, so NO redelivery is coming: it owns the only retry this
// review will ever get. Acknowledging a transient launch failure (a Mongo
// blip, a bot momentarily unlaunchable) therefore destroys the review —
// the ordinary "StatusLaunchError is retryable on redelivery" contract
// has nothing to retry it with. It must re-arm, and give up loudly.
func TestSyncDebounce_TransientLaunchFailureIsRetriedThenGivesUp(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	var attempts int
	var launched []string
	s.webhookLaunchBot = func(_ context.Context, bot string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		attempts++
		if attempts <= 2 {
			return "", errors.New("transient: queue unavailable")
		}
		launched = append(launched, bot)
		return "run-" + bot, nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.handleGitHubWebhook(httptest.NewRecorder(), ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	base := time.Now().UTC()
	// Attempt 1 fails → re-armed one backoff out, not acknowledged.
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(4*time.Minute))
	// Attempt 2 fails → re-armed again.
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(10*time.Minute))
	// Attempt 3 succeeds.
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(20*time.Minute))
	if attempts != 3 || len(launched) != 1 {
		t.Fatalf("want 3 attempts ending in one launch, got attempts=%d launched=%v", attempts, launched)
	}
	// Acknowledged: no fourth attempt.
	s.sweepDeferredWebhookLaunches(context.Background(), base.Add(40*time.Minute))
	if attempts != 3 {
		t.Fatalf("the acknowledged row re-fired: attempts=%d", attempts)
	}
}

// The bound: a permanently-broken launch must not re-fire forever.
func TestSyncDebounce_LaunchFailuresAreBounded(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookDeferred = webhooks.NewMemoryDeferredLaunchStore()
	s.syncDebounce = 3 * time.Minute
	var attempts int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		attempts++
		return "", errors.New("permanently broken")
	}
	cfg, pt := ghConfig(t, s)
	cfg.ReviewOnSync = true
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.handleGitHubWebhook(httptest.NewRecorder(), ghReq(ghCtx(cfg), ghSyncPayload("sha-1"), prforge.EventHeaderPullRequest, pt))

	base := time.Now().UTC()
	for i := 1; i <= 6; i++ {
		s.sweepDeferredWebhookLaunches(context.Background(), base.Add(time.Duration(i)*10*time.Minute))
	}
	if attempts != webhookDeferMaxAttempts {
		t.Fatalf("attempts = %d, want the bound %d", attempts, webhookDeferMaxAttempts)
	}
}

// A push arriving mid-retry must WIN: the fresher payload keeps its own
// window instead of being pushed back by the loser's backoff, and its
// retry budget starts clean.
func TestDeferredStore_RescheduleLosesToAFreshPush(t *testing.T) {
	st := webhooks.NewMemoryDeferredLaunchStore()
	ctx := context.Background()
	now := time.Now().UTC()

	d := webhooks.DeferredLaunch{SubjectKey: "t1|w1|acme/a|pr:7", FireAt: now.Add(-time.Second), CreatedAt: now}
	if err := st.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	due, _ := st.ClaimDue(ctx, now, time.Minute, 10)
	if len(due) != 1 {
		t.Fatalf("claim: %v", due)
	}
	// A fresh push lands while the claimer is launching.
	d.FireAt = now.Add(3 * time.Minute)
	if err := st.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	// The loser's reschedule must not touch it.
	if err := st.RescheduleFailed(ctx, due[0].SubjectKey, due[0].Generation, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.ClaimDue(ctx, now.Add(4*time.Minute), time.Minute, 10)
	if len(fresh) != 1 {
		t.Fatalf("the fresh push must fire on its OWN window, got %v", fresh)
	}
	if fresh[0].Attempts != 0 {
		t.Fatalf("a new payload starts with a clean retry budget, got attempts=%d", fresh[0].Attempts)
	}
}

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
