package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The fork lane's pool, server side (ADR-162 Q3 and decision 10): the
// fork-lane config names the runner pool its runs execute on — set at
// creation, required there, immutable — and every launch the lane produces
// carries it to the publisher (LaunchSpec.ForkLanePool), which stamps the
// run's RunnerPool. A config stored without one is refused at the launch
// tail before either budget is metered.

// createWebhook posts a creation body as a super-admin and decodes the row.
func createWebhook(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, webhooks.Config) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(superAdminCtx(), "POST", "/api/teams/t1/webhooks", body, "t1", ""))
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
	}
	return w, created.Config
}

// Mutations, each red on its own row: forkLanePoolCreationRefusal's three
// arms dropped one at a time (each refusal row then answers 201), and the
// creation no longer copying the pool onto the row (the accepted row reads
// back without it).
func TestForkLanePool_IsRequiredAndCheckedAtCreation(t *testing.T) {
	for _, row := range []struct {
		name, body, want string
	}{
		{"a fork lane without a pool", `{"name":"f","provider":"github","bot_ids":["review-pr"],"fork_lane":true}`, "fork_lane requires fork_lane_pool"},
		{"a pool without a fork lane", `{"name":"o","provider":"github","bot_ids":["review-pr"],"fork_lane_pool":"forks"}`, "this config is not one"},
		{"a pool that is not a pool name", `{"name":"f","provider":"github","bot_ids":["review-pr"],"fork_lane":true,"fork_lane_pool":"Forks_Pool"}`, "is not a runner pool name"},
	} {
		t.Run(row.name, func(t *testing.T) {
			w, _ := createWebhook(t, newWebhookTestServer(t), row.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), row.want) {
				t.Fatalf("code=%d body=%s, want 400 naming %q", w.Code, w.Body.String(), row.want)
			}
		})
	}
	t.Run("a fork lane naming its pool is created with it", func(t *testing.T) {
		s := newWebhookTestServer(t)
		w, cfg := createWebhook(t, s, `{"name":"f","provider":"github","bot_ids":["review-pr"],"fork_lane":true,"fork_lane_pool":" forks "}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		stored, err := s.webhookConfigs.Get(context.Background(), cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.ForkLane || stored.ForkLanePool != "forks" {
			t.Fatalf("stored row ForkLane=%v ForkLanePool=%q, want the lane and its trimmed pool", stored.ForkLane, stored.ForkLanePool)
		}
	})
}

// The pool is immutable like the lane kind: a change is refused 409, a
// clear too, and a restatement is a no-op that lets the rest of the patch
// apply. Mutation: the PATCH arm removed → the change answers 200 and the
// stored row moves pools (red on the 409 assertion and on the read-back).
func TestForkLanePool_IsImmutableOnPatch(t *testing.T) {
	s := newWebhookTestServer(t)
	w, cfg := createWebhook(t, s, `{"name":"f","provider":"github","bot_ids":["review-pr"],"fork_lane":true,"fork_lane_pool":"forks"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	patch := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+cfg.ID, body, "t1", cfg.ID))
		return w
	}
	for _, body := range []string{`{"fork_lane_pool":"other-pool"}`, `{"fork_lane_pool":""}`} {
		if w := patch(body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "fork_lane_pool is immutable") {
			t.Fatalf("PATCH %s: code=%d body=%s, want 409 naming the immutability", body, w.Code, w.Body.String())
		}
	}
	if w := patch(`{"fork_lane_pool":"forks","name":"renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("restatement: code=%d body=%s, want the no-op patch to apply", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ForkLanePool != "forks" || stored.Name != "renamed" {
		t.Fatalf("stored row ForkLanePool=%q Name=%q, want the pool kept and the rest of the patch applied", stored.ForkLanePool, stored.Name)
	}
}

// A fork-lane config stored without a pool — created before the field — is
// refused at the launch tail, typed, BEFORE either budget is metered: the
// gesture is admitted row by row, then the provenance precondition the
// delivery tail, the durable retry and the board branch all cross filters
// it. Mutation: the precondition's pool clause removed → the gesture launches
// and meters both subjects (red on the filtered status, the launch count and
// both counters).
func TestForkGesture_AConfigWithoutAPoolIsRefusedBeforeMetering(t *testing.T) {
	fx := newForkGestureFixture(t)
	fx.cfg.ForkLanePool = ""
	w := fx.replay(t, 4101, "/revi")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), webhooks.StatusFiltered) {
		t.Fatalf("code=%d body=%s, want 200/filtered", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fork lane names no runner pool") {
		t.Fatalf("body=%s, want the typed reason naming the missing pool", w.Body.String())
	}
	if n := fx.launchedCount(); n != 0 {
		t.Fatalf("launches=%d, want 0", n)
	}
	now := time.Now().UTC()
	for _, subj := range []orgusage.Subject{orgusage.OrgSubject("t1"), orgusage.ForkAuthorSubject("t1", "github", "4242")} {
		u, err := fx.counter.Usage(context.Background(), subj, now)
		if err != nil {
			t.Fatal(err)
		}
		if u.Runs != 0 {
			t.Fatalf("subject %s metered %d run(s) — the refusal must precede the bound", subj, u.Runs)
		}
	}
	rows, err := fx.s.webhookDeliveries.ListByWebhook(context.Background(), "t1", fx.cfg.ID, 10)
	if err != nil || len(rows) != 1 || rows[0].Status != webhooks.StatusFiltered || !strings.Contains(rows[0].Error, "fork lane names no runner pool") {
		t.Fatalf("delivery rows = %+v (err %v), want one filtered row with the typed reason", rows, err)
	}
}

// realForkLauncher puts the REAL launcher behind the fixture: the lane's
// default bot from the catalog, the run service, and a publisher recording
// the LaunchSpec it is handed.
func realForkLauncher(t *testing.T, fx *forkGestureFixture) *forkSpecPublisher {
	t.Helper()
	fx.s.webhookLaunchBot = nil
	if err := os.WriteFile(filepath.Join(fx.s.cfg.WorkDir, "bots", "review-pr.bot"), []byte(forkReviewProbeBot), 0o644); err != nil {
		t.Fatal(err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := &forkSpecPublisher{}
	fx.s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	return pub
}

// The durable retry rebuilds the target from the delivery row and re-reads
// the config, so the relaunch carries the lane's pool as the gesture did —
// and the same row on a config without a pool is refused for good.
//
// Mutation: forkLanePoolFor returning "" → the relaunch's spec names no pool
// (red on the spec assertion; the publisher would refuse it after the gate).
func TestForkLanePool_TheDurableRetryRelaunchesOnTheLanesPool(t *testing.T) {
	seed := func(t *testing.T, fx *forkGestureFixture) webhooks.Delivery {
		t.Helper()
		if err := fx.s.webhookConfigs.Create(context.Background(), fx.cfg); err != nil {
			t.Fatal(err)
		}
		due := time.Now().Add(-time.Hour)
		d := webhooks.Delivery{
			ID: "fork-retry", WebhookID: fx.cfg.ID, TenantID: fx.cfg.TenantID, Provider: fx.cfg.Provider,
			Status: webhooks.StatusLaunchError, IdempotencyKey: "fork-retry-idem", BotID: "review-pr",
			ProjectPath: "acme/widgets", Attempts: 1, NextAttemptAt: &due,
			RetryTarget: &webhooks.DeferredTarget{
				BotID: "review-pr", IdemKey: "fork-retry-idem",
				Vars:    map[string]string{"pr_url": "https://github.com/acme/widgets/pull/7", "head_sha": forkForkPR().HeadSHA},
				RepoURL: "https://github.com/acme/widgets.git", RepoRef: "refs/pull/7/head",
				Trust: store.RunTrustFork, ExpectedSHA: forkForkPR().HeadSHA,
				ForkAuthor: &webhooks.ForkAuthorRef{Provider: "github", AuthorID: "4242"},
			},
		}
		if err := fx.s.webhookDeliveries.Insert(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	t.Run("the relaunch names the lane's pool", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		pub := realForkLauncher(t, fx)
		d := seed(t, fx)
		out, launch := fx.s.prepareWebhookRetry(context.Background(), d)
		if launch == nil {
			t.Fatalf("the retry was not prepared: %+v", out)
		}
		if res := launch(context.Background()); res.Status != webhooks.StatusLaunched {
			t.Fatalf("relaunch = %+v, want launched", res)
		}
		if spec := pub.only(t); spec.ForkLanePool != "forks" || spec.Trust != store.RunTrustFork {
			t.Fatalf("relaunch spec ForkLanePool=%q Trust=%q, want the lane's pool on the fork run", spec.ForkLanePool, spec.Trust)
		}
	})
	t.Run("a config without a pool exhausts the row", func(t *testing.T) {
		fx := newForkGestureFixture(t)
		pub := realForkLauncher(t, fx)
		fx.cfg.ForkLanePool = ""
		d := seed(t, fx)
		if _, launch := fx.s.prepareWebhookRetry(context.Background(), d); launch != nil {
			t.Fatal("a retry on a fork-lane config without a pool was prepared for launch")
		}
		got, err := fx.s.webhookDeliveries.Get(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != webhooks.StatusLaunchExhausted {
			t.Fatalf("row status = %s, want %s — no retry cures a config that names no pool", got.Status, webhooks.StatusLaunchExhausted)
		}
		if len(pub.specs) != 0 {
			t.Fatalf("published %d spec(s)", len(pub.specs))
		}
	})
}
