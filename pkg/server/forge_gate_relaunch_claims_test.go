package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// raceLoserDeliveries makes the calling offer the LOSER of a claim race whose
// winner inserted, launched and FAILED TO START between this offer's replay
// check and its own insert.
type raceLoserDeliveries struct {
	webhooks.DeliveryStore
	mu     sync.Mutex
	done   bool
	winner webhooks.Delivery
}

func (r *raceLoserDeliveries) Insert(ctx context.Context, d webhooks.Delivery) error {
	r.mu.Lock()
	inject := !r.done && d.IdempotencyKey == r.winner.IdempotencyKey
	r.done = r.done || inject
	r.mu.Unlock()
	if inject {
		if err := r.DeliveryStore.Insert(ctx, r.winner); err != nil {
			return err
		}
	}
	return r.DeliveryStore.Insert(ctx, d)
}

// A duplicate that names no run is a relaunch still launching, or one that
// died on the way: each is escalated when — and only when — it is a death.
func TestGateRelaunchClaims(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "revi/review"
		botID  = "dep-update-guard"
		base   = "https://iterion.test"
	)
	ctx := context.Background()
	type world struct {
		s     *Server
		gc    *listingGateClient
		board native.BoardStore
	}
	build := func(t *testing.T) world {
		t.Helper()
		s := newWebhookTestServer(t)
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs
		s.cfg.PublicURL = base
		conns := forge.NewMemoryConnectionStore()
		if err := conns.Create(ctx, forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
		ints := forge.NewMemoryRepoIntegrationStore()
		if err := ints.Create(ctx, forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{botID}, WebhookID: "w1",
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(ctx, webhooks.Config{ID: "w1", TenantID: team, BotIDs: []string{botID}}); err != nil {
			t.Fatal(err)
		}
		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: head}}
		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
		board, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			return "run-relaunched", nil
		}
		return world{s: s, gc: gc, board: board}
	}
	seedDeadRun := func(t *testing.T, s *Server) string {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(ctx, id, "dep_update_guard", map[string]any{
			"pr_url": prURL, "gate_context": gateNm, "head_sha": head,
			forgePublishVarToken: "run-token",
			forgePublishVarURL:   base + "/api/v1/forge/publish-review",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = botID
		run.Status = store.RunStatusFailedResumable
		run.Error = "provider error: 529 overloaded"
		if err := s.cfg.Store.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	relaunchIdem := knowledge.ChecksumHex([]byte("gaterelaunch|" + team + "|" + repo + "|7|" + head + "|" + botID))
	cards := func(t *testing.T, w world) []*native.Issue {
		t.Helper()
		got, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	ownMarker := func(runID string) []forge.CommitStatus {
		return []forge.CommitStatus{{Context: gateNm, State: forge.CommitStateFailure,
			Description: gateInterruptedDescription, TargetURL: deeplink.Run(base, runID)}}
	}
	ownClaim := func(runID string) []forge.CommitStatus {
		return []forge.CommitStatus{{Context: gateNm, State: forge.CommitStatePending,
			Description: gateInFlightDescription, TargetURL: deeplink.Run(base, runID)}}
	}
	blockingLauncher := func(w world, runID string) (entered, release chan struct{}) {
		entered, release = make(chan struct{}), make(chan struct{})
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			close(entered)
			<-release
			return runID, nil
		}
		return entered, release
	}
	wait := func(t *testing.T, ch chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	// Q1c (control): the FIRST attempt. Two offers of one dead run race —
	// both read the head before either posted, both post the synthetic
	// failure, the second meets the first's fresh claim row.
	t.Run("a racing offer meets the first attempt launching: no card", func(t *testing.T) {
		w := build(t)
		entered, release := blockingLauncher(w, "run-relaunch-1")
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		done := make(chan struct{})
		go func() { defer close(done); _ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent) }()
		wait(t, entered, "the winner's launch")
		row, _ := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		t.Logf("claim row while attempt 1 launches: status=%s attempts=%d run=%q received %s ago failed_at=%v",
			row.Status, row.Attempts, row.RunID, time.Since(row.ReceivedAt).Round(time.Second), row.FailedAt)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep) // the racing offer
		got := cards(t, w)
		close(release)
		wait(t, done, "the winner")
		if len(got) != 0 {
			t.Errorf("FALSE ESCALATION on attempt 1: %d card(s) while the relaunch was launching:\n%s", len(got), got[0].Body)
		}
	})

	// Q1: the THIRD attempt (ClaimFailedRetry). The reclaimed row keeps the
	// FIRST attempt's ReceivedAt and loses FailedAt, so its "age" is 20 min
	// although it was claimed a moment ago.
	t.Run("a racing offer meets a retry launching: no card", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		failedAt := now.Add(-11 * time.Minute) // attempt 2 failed 11 min ago; its 10 min backoff is over
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-retry", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunchError, Attempts: 2, ReceivedAt: now.Add(-20 * time.Minute),
			FailedAt: &failedAt, Error: "launch failed: queue blip",
		}); err != nil {
			t.Fatal(err)
		}
		entered, release := blockingLauncher(w, "run-relaunch-3")
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownMarker(r1) // the first death's own marker, as the retries find it
		done := make(chan struct{})
		go func() { defer close(done); _ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep) }()
		wait(t, entered, "the retry's launch")
		row, _ := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		t.Logf("claim row while attempt 3 launches: status=%s attempts=%d run=%q received %s ago failed_at=%v",
			row.Status, row.Attempts, row.RunID, time.Since(row.ReceivedAt).Round(time.Second), row.FailedAt)
		// A racing offer that reaches the tail while attempt 3 launches:
		// another dead run of the same bot on the same head.
		r1b := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRunID(ctx, r1b, gateTriggerSweep)
		got := cards(t, w)
		close(release)
		wait(t, done, "the retry")
		final, _ := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		t.Logf("after the retry returned: status=%s run=%q", final.Status, final.RunID)
		if len(got) != 0 {
			t.Errorf("FALSE ESCALATION on attempt 3: %d card(s) filed while the retry was launching (it then launched %q):\n%s",
				len(got), final.RunID, got[0].Body)
		}
	})

	// Q1d: the same, the racer being a second offer of the SAME dead run (two
	// sweeps across a lease hand-over) that passed gateRelaunchRetryPending
	// before the winner's claim — from relaunchDeadGateRun on, its path.
	t.Run("the same run's second offer meets its retry launching: no card", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		failedAt := now.Add(-11 * time.Minute)
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-retry", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunchError, Attempts: 2, ReceivedAt: now.Add(-20 * time.Minute),
			FailedAt: &failedAt, Error: "launch failed: queue blip",
		}); err != nil {
			t.Fatal(err)
		}
		entered, release := blockingLauncher(w, "run-relaunch-3")
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownMarker(r1)
		done := make(chan struct{})
		go func() { defer close(done); _ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep) }()
		wait(t, entered, "the retry's launch")
		run, err := w.s.cfg.Store.LoadRun(ctx, r1)
		if err != nil {
			t.Fatal(err)
		}
		conn, _ := w.s.forgeConnections.Get(ctx, "c1")
		grant, _ := w.s.forgePublishTokens.lookup("run-token")
		w.s.relaunchDeadGateRun(ctx, deadGateRun{
			run: run, grant: grant, conn: conn, gc: w.gc,
			repo: repo, number: 7, pr: forge.PullRef{HeadSHA: head, HeadRepoFullName: repo},
			gateCtx: gateNm, prURL: prURL,
		})
		got := cards(t, w)
		close(release)
		wait(t, done, "the retry")
		if len(got) != 0 {
			t.Errorf("FALSE ESCALATION (same run) on attempt 3: %d card(s) while the retry was launching", len(got))
		}
	})

	// Q2: the launching process dies mid-launch (simulated by Goexit in the
	// launcher: nothing after it runs, the row stays Accepted with no run).
	// The docs: "escalated only once the row is older than 10 minutes, a
	// launch that died on the way". The sweep re-offers the dead run for an
	// hour.
	t.Run("a launch that died on the way is escalated within the hour", func(t *testing.T) {
		w := build(t)
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			runtime.Goexit() // the replica dies mid-launch
			return "", nil
		}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		done := make(chan struct{})
		go func() { defer close(done); _ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent) }()
		wait(t, done, "the dying offer")
		row, _ := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		t.Logf("stuck claim row: status=%s run=%q; statuses posted=%d (last %s %q)", row.Status, row.RunID, w.gc.setCalls, w.gc.last.State, w.gc.last.TargetURL)
		w.gc.statuses = ownMarker(r1) // what the forge now shows: the first offer's synthetic failure
		start := time.Now().UTC()
		for m := 1; m <= 60; m++ {
			at := start.Add(time.Duration(m) * time.Minute)
			w.s.gateClock = func() time.Time { return at }
			_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep)
		}
		got := cards(t, w)
		t.Logf("cards after an hour of sweep offers: %d", len(got))
		if len(got) != 1 {
			t.Errorf("a launch that died on the way 60 min ago was never escalated (%d cards) — the docs say it is escalated once older than 10 minutes", len(got))
		}
	})

	// Q3: the relaunched run's claim row never learned its run (the launch
	// returned, the row update did not land), and the relaunch dies two
	// minutes after its claim, holding its own in-flight claim on the head.
	t.Run("a relaunch that dies before its row learns its run is escalated", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-rel", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusAccepted, Attempts: 1, ReceivedAt: now.Add(-2 * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		rel := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(rel)
		_ = w.s.reconcileGateForRunID(ctx, rel, gateTriggerEvent)
		first := len(cards(t, w))
		w.gc.statuses = ownMarker(rel)
		for m := 1; m <= 60; m++ {
			at := now.Add(time.Duration(m) * time.Minute)
			w.s.gateClock = func() time.Time { return at }
			_ = w.s.reconcileGateForRunID(ctx, rel, gateTriggerSweep)
		}
		got := cards(t, w)
		t.Logf("cards after the death event: %d; after an hour of sweep offers: %d", first, len(got))
		if len(got) != 1 {
			t.Errorf("the relaunch died and is never escalated (%d cards)", len(got))
		}
	})

	// The same read-back once the budget is spent IS a death: only a row a
	// launch claimed reads as one still launching, never a failed start's.
	t.Run("a failed start read back with its budget spent is escalated", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		w.s.webhookDeliveries = &raceLoserDeliveries{DeliveryStore: w.s.webhookDeliveries, winner: webhooks.Delivery{
			ID: "d-winner", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunchError, Attempts: maxUnattendedLaunchAttempts, ReceivedAt: now, FailedAt: &now,
			Error: "launch failed: queue blip",
		}}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent)
		if got := cards(t, w); len(got) != 1 {
			t.Errorf("a relaunch whose budget is spent, read back by a racing loser, filed %d card(s), want 1", len(got))
		}
	})

	// A launch is booked even when its caller is gone: the claim row learns its
	// run on a context the caller's cancellation does not reach. The store
	// honours cancellation, as Mongo does — a store that ignored it would pass
	// whatever context the booking used.
	t.Run("a launch is booked even when its caller is gone", func(t *testing.T) {
		w := build(t)
		w.s.webhookDeliveries = ctxHonoringDeliveries{DeliveryStore: w.s.webhookDeliveries}
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			cancel() // the sweep term ends while the launch returns
			return "run-relaunched", nil
		}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(callCtx, r1, gateTriggerEvent)
		row, err := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		if err != nil || row.RunID != "run-relaunched" || row.Status != webhooks.StatusLaunched {
			t.Errorf("the claim row after a launch whose caller left: %s run %q (err %v), want launched run-relaunched", row.Status, row.RunID, err)
		}
	})

	// A row the store refuses to update is reported, not swallowed: left
	// without its run, it would read as a launch still in flight.
	t.Run("a claim row the store refuses to update is reported", func(t *testing.T) {
		w := build(t)
		out := &lockedBuffer{}
		w.s.logger = iterlog.New(iterlog.LevelWarn, out)
		w.s.webhookDeliveries = refusingUpdateDeliveries{DeliveryStore: w.s.webhookDeliveries}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent)
		if !strings.Contains(out.String(), "was not recorded") {
			t.Errorf("a refused claim-row update was not reported; log:\n%s", out.String())
		}
	})

	// Q4 (class sibling): a racing loser reads back the winner's row after
	// the winner FAILED TO START (attempt 1 of 3, budget unspent).
	t.Run("a failed start read back by a racing loser is left to the budget", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		w.s.webhookDeliveries = &raceLoserDeliveries{DeliveryStore: w.s.webhookDeliveries, winner: webhooks.Delivery{
			ID: "d-winner", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunchError, Attempts: 1, ReceivedAt: now, FailedAt: &now,
			Error: "launch failed: queue blip",
		}}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent)
		got := cards(t, w)
		row, _ := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		t.Logf("row: status=%s attempts=%d run=%q; cards=%d", row.Status, row.Attempts, row.RunID, len(got))
		if len(got) != 0 {
			t.Errorf("a relaunch that failed to start ONCE (budget 1/%d) was escalated as a death:\n%s", maxUnattendedLaunchAttempts, got[0].Body)
		}
	})

	// The tail books what it did even when its caller is gone — each of its
	// booking sites on its own path: a failed launch, a refused grant, the
	// gate's in-flight claim. Left Accepted, a row reads as a launch in
	// flight, then as a death, instead of a retry on the budget.
	t.Run("a failed launch is booked even when its caller is gone", func(t *testing.T) {
		w := build(t)
		w.s.webhookDeliveries = ctxHonoringDeliveries{DeliveryStore: w.s.webhookDeliveries}
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			cancel()
			return "", errors.New("queue blip")
		}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(callCtx, r1, gateTriggerEvent)
		row, err := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		if err != nil || row.Status != webhooks.StatusLaunchError || row.Attempts != 1 || row.FailedAt == nil {
			t.Errorf("claim row after a failed launch whose caller left: %s attempts=%d failed_at=%v (err %v), want launch_error / 1 / set",
				row.Status, row.Attempts, row.FailedAt, err)
		}
	})

	t.Run("a refused grant is booked even when its caller is gone", func(t *testing.T) {
		w := build(t)
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		callCtx = auth.WithIdentity(callCtx, auth.Identity{UserID: "a", TeamID: team, Role: identity.RoleMember, Kind: auth.KindWebhook})
		callCtx = store.WithIdentity(callCtx, team, "a")
		w.s.webhookDeliveries = ctxHonoringDeliveries{DeliveryStore: cancelAfterClaim{DeliveryStore: w.s.webhookDeliveries, cancel: cancel}}
		w.s.forgePublishTokens.Register("foreign", ForgePublishGrant{TeamID: "t2", ConnectionID: "c1", Repo: repo})
		cfg, err := w.s.webhookConfigs.Get(ctx, "w1")
		if err != nil {
			t.Fatal(err)
		}
		vars := map[string]string{"pr_url": prURL, "gate_context": gateNm, "head_sha": head, forgePublishVarToken: "foreign"}
		res := w.s.launchWebhookTarget(callCtx, nil, cfg,
			webhookEventMeta{Kind: gateRelaunchEventKind, Action: "review_died", ProjectPath: repo, SubjectID: "pr:7", SubjectURL: prURL, SubjectSHA: head},
			forgeLaunchTarget{BotID: botID, IdemKey: "k-refused", Vars: vars, RepoURL: "https://github.com/acme/widgets.git", RepoRef: "feat/x"}, "", "")
		row, err := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, "k-refused")
		if res.Status != webhooks.StatusLaunchError || err != nil || row.Status != webhooks.StatusLaunchError || row.FailedAt == nil {
			t.Errorf("a launch refused for a foreign grant, its caller gone: result %s, row %s failed_at=%v (err %v), want launch_error booked",
				res.Status, row.Status, row.FailedAt, err)
		}
	})

	t.Run("the gate's in-flight claim lands even when its caller is gone", func(t *testing.T) {
		w := build(t)
		gc := ctxHonoringGateClient{w.gc}
		w.s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			cancel()
			return "run-relaunched", nil
		}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(callCtx, r1, gateTriggerEvent)
		claimed := false
		for _, st := range w.gc.posted {
			claimed = claimed || (isGateInFlight(st) && strings.Contains(st.TargetURL, "run-relaunched"))
		}
		if !claimed {
			t.Errorf("the relaunch launched but its in-flight claim never landed (statuses: %+v)", w.gc.posted)
		}
	})

	t.Run("the fixer's in-flight claim lands even when its caller is gone", func(t *testing.T) {
		w := build(t)
		w.s.cfg.Bots.Paths = []string{botsDirAbs(t)}
		gc := ctxHonoringGateClient{w.gc}
		w.s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		callCtx = auth.WithIdentity(callCtx, auth.Identity{UserID: "fixer-actor", TeamID: team, Role: identity.RoleMember, Kind: auth.KindWebhook})
		callCtx = store.WithIdentity(callCtx, team, "fixer-actor")
		w.s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			cancel()
			return "run-fixer", nil
		}
		vars := fixLaunchVars()
		vars["pr_url"] = prURL
		delete(vars, "gate_context")
		cfg, err := w.s.webhookConfigs.Get(ctx, "w1")
		if err != nil {
			t.Fatal(err)
		}
		cfg.BotIDs = []string{"branch-improve-loop"}
		res := w.s.launchWebhookTarget(callCtx, nil, cfg,
			webhookEventMeta{Kind: "pull_request", Action: "x", ProjectPath: repo, SubjectID: "pr:7", SubjectURL: prURL, SubjectSHA: "deadbeef"},
			forgeLaunchTarget{BotID: "branch-improve-loop", IdemKey: "fixer-idem", Vars: vars, RepoURL: "https://github.com/acme/widgets.git", RepoRef: "feat/x"}, "", "")
		if res.Status != webhooks.StatusLaunched {
			t.Fatalf("launch result %+v", res)
		}
		claimed := false
		for _, st := range w.gc.posted {
			claimed = claimed || st.Context == fixInFlightContext
		}
		if !claimed {
			t.Errorf("the fixer launched but its in-flight claim never landed (statuses: %+v)", w.gc.posted)
		}
	})

	// A claim row the store refuses once — a primary election — is written on
	// the next try: left without its run, it would read as a death in ten
	// minutes while the relaunch reviews.
	t.Run("a claim row the store refuses once is written on the next try", func(t *testing.T) {
		w := build(t)
		w.s.webhookDeliveries = &refusingOnceDeliveries{DeliveryStore: w.s.webhookDeliveries, refusals: 1}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent)
		row, err := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		if err != nil || row.Status != webhooks.StatusLaunched || row.RunID != "run-relaunched" {
			t.Errorf("claim row after one refused update: %s run %q (err %v), want launched run-relaunched", row.Status, row.RunID, err)
		}
	})

	// A write that commits and then loses its ack must not be retried over a
	// redelivery that claimed the row in between: the stale copy would roll
	// the claim back and re-arm the launch-storm guard.
	t.Run("a retry never rolls a concurrent claim back", func(t *testing.T) {
		w := build(t)
		store := &commitThenConcurrentClaim{DeliveryStore: w.s.webhookDeliveries}
		w.s.webhookDeliveries = store
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownClaim(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerEvent)
		row, err := w.s.webhookDeliveries.GetByIdempotencyKey(ctx, relaunchIdem)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("after the tail: updates=%d row status=%s attempts=%d claimed_at=%v run=%q",
			store.updates, row.Status, row.Attempts, row.ClaimedAt != nil, row.RunID)
		if store.updates != 1 {
			t.Errorf("the tail wrote the row %d time(s) — the second write rolled the concurrent claim back", store.updates)
		}
		if row.Status != webhooks.StatusAccepted || row.Attempts != 2 || row.RunID != "" {
			t.Errorf("the concurrent claim was clobbered: %s attempts=%d run=%q", row.Status, row.Attempts, row.RunID)
		}
	})

	// A stale claim is escalated BEFORE the integration and hold-label round
	// trips — a forge read against the quota this lane lives on: here the
	// repo's integration is gone, and the card still comes, naming the launch
	// that never recorded a run.
	t.Run("a stale claim escalates before any round trip", func(t *testing.T) {
		w := build(t)
		claimed := time.Now().UTC().Add(-30 * time.Minute)
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-stuck", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusAccepted, Attempts: 1, ReceivedAt: claimed, ClaimedAt: &claimed,
		}); err != nil {
			t.Fatal(err)
		}
		if err := w.s.forgeIntegrations.Delete(ctx, "i1"); err != nil {
			t.Fatal(err)
		}
		r1 := seedDeadRun(t, w.s)
		w.gc.statuses = ownMarker(r1)
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep)
		got := cards(t, w)
		if len(got) != 1 || !strings.Contains(got[0].Body, "never recorded a run") {
			t.Fatalf("cards=%d, want one naming the launch that never recorded a run", len(got))
		}
	})

	// The window, both sides of it, in the documented minutes: nine minutes
	// after its claim a relaunch is still launching; eleven minutes after, it
	// is a death.
	for _, tc := range []struct {
		age   time.Duration
		cards int
	}{{9 * time.Minute, 0}, {11 * time.Minute, 1}} {
		t.Run(fmt.Sprintf("a claim %s old", tc.age), func(t *testing.T) {
			w := build(t)
			claimed := time.Now().UTC().Add(-tc.age)
			if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
				ID: "d-claim", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
				Status: webhooks.StatusAccepted, Attempts: 1, ReceivedAt: claimed, ClaimedAt: &claimed,
			}); err != nil {
				t.Fatal(err)
			}
			r1 := seedDeadRun(t, w.s)
			w.gc.statuses = ownMarker(r1)
			_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep)
			if got := len(cards(t, w)); got != tc.cards {
				t.Errorf("cards=%d, want %d", got, tc.cards)
			}
		})
	}

	// The head's one relaunch failed on a usage window and its retry is armed
	// — the reconciler itself reads that as "not dead". Another dead run of
	// the same bot, offered behind a third run's synthetic marker, reaches the
	// relaunch tail and meets the claim: it must not escalate the relaunch.
	t.Run("a relaunch parked on an armed retry is alive", func(t *testing.T) {
		w := build(t)
		w.s.forgePublishTokens.Register("rel-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
		rel := seedDeadRun(t, w.s)
		run, err := w.s.cfg.Store.LoadRun(ctx, rel)
		if err != nil {
			t.Fatal(err)
		}
		after := time.Now().UTC().Add(time.Hour)
		run.Inputs[forgePublishVarToken] = "rel-token"
		run.RetryState = &store.RunRetryState{RetryAfter: &after, Reason: "usage_window", Attempts: 1}
		if err := w.s.cfg.Store.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		launched := time.Now().UTC().Add(-20 * time.Minute)
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-rel", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunched, RunID: rel, Attempts: 1, ReceivedAt: launched, LaunchedAt: &launched,
		}); err != nil {
			t.Fatal(err)
		}
		r0 := seedDeadRun(t, w.s)
		r2 := seedDeadRun(t, w.s)
		w.gc.statuses = ownMarker(r0)
		_ = w.s.reconcileGateForRunID(ctx, r2, gateTriggerSweep)
		if alive, why := w.s.relaunchStillRunning(ctx, rel); !alive {
			t.Errorf("the relaunch reads as dead (%s)", why)
		}
		if got := cards(t, w); len(got) != 0 {
			t.Errorf("a relaunch parked on an armed retry was escalated as dead:\n%s", got[0].Body)
		}
	})
}

// commitThenConcurrentClaim commits the first write, then lands a concurrent
// redelivery's claim on the row, then reports failure — the lost-ack shape the
// memory twin cannot express.
type commitThenConcurrentClaim struct {
	webhooks.DeliveryStore
	mu      sync.Mutex
	updates int
	done    bool
}

func (c *commitThenConcurrentClaim) Update(ctx context.Context, d webhooks.Delivery) error {
	c.mu.Lock()
	c.updates++
	first := !c.done
	c.done = true
	c.mu.Unlock()
	err := c.DeliveryStore.Update(ctx, d)
	if first && err == nil {
		claimed := time.Now().UTC()
		row, gerr := c.GetByIdempotencyKey(ctx, d.IdempotencyKey)
		if gerr == nil {
			row.Status = webhooks.StatusAccepted
			row.Attempts++
			row.RunID = ""
			row.LaunchedAt = nil
			row.ClaimedAt = &claimed
			_ = c.DeliveryStore.Update(ctx, row)
		}
		return errors.New("i/o timeout")
	}
	return err
}

// ctxHonoringGateClient refuses every call on a done context, as the real HTTP
// clients do.
type ctxHonoringGateClient struct{ *listingGateClient }

func (c ctxHonoringGateClient) ListCommitStatuses(ctx context.Context, repo, sha string) ([]forge.CommitStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.listingGateClient.ListCommitStatuses(ctx, repo, sha)
}

func (c ctxHonoringGateClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.listingGateClient.SetCommitStatus(ctx, repo, sha, st)
}

// cancelAfterClaim ends the caller right after its claim lands: a sweep term
// ending, a request cut.
type cancelAfterClaim struct {
	webhooks.DeliveryStore
	cancel context.CancelFunc
}

func (c cancelAfterClaim) Insert(ctx context.Context, d webhooks.Delivery) error {
	err := c.DeliveryStore.Insert(ctx, d)
	c.cancel()
	return err
}

// refusingOnceDeliveries refuses its first updates, as a store does through a
// primary election.
type refusingOnceDeliveries struct {
	webhooks.DeliveryStore
	mu       sync.Mutex
	refusals int
}

func (r *refusingOnceDeliveries) Update(ctx context.Context, d webhooks.Delivery) error {
	r.mu.Lock()
	refuse := r.refusals > 0
	if refuse {
		r.refusals--
	}
	r.mu.Unlock()
	if refuse {
		return errors.New("mongo: primary stepped down")
	}
	return r.DeliveryStore.Update(ctx, d)
}

// The re-request collapse ages an Accepted row by its claim: a retry claimed a
// second ago is in flight, though its first attempt was received long before.
func TestHeadReviewClaimAgesARetryByItsClaim(t *testing.T) {
	ctx := context.Background()
	recently := time.Now().UTC().Add(-time.Second)
	for _, tc := range []struct {
		name      string
		claimedAt *time.Time
		live      bool
	}{
		{"a third attempt claimed a second ago", &recently, true},
		{"a row with no claim time, received 20 min ago", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			base := "https://github.com/acme/widgets|7|cafe1234"
			if err := s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
				ID: "d1", TenantID: "t1", WebhookID: "w1", IdempotencyKey: forgeIdemKey(base, "review-pr", false), BotID: "review-pr",
				Status: webhooks.StatusAccepted, Attempts: 3, ReceivedAt: time.Now().UTC().Add(-20 * time.Minute), ClaimedAt: tc.claimedAt,
			}); err != nil {
				t.Fatal(err)
			}
			claimed, live := s.headReviewClaim(ctx, webhooks.Config{ID: "w1", TenantID: "t1"}, []webhooks.BotRule{{BotID: "review-pr"}}, base)
			if !claimed || live != tc.live {
				t.Errorf("claimed=%v live=%v, want claimed and live=%v", claimed, live, tc.live)
			}
		})
	}
}

// flakyGateClient fails its first N SetCommitStatus calls with a 502.
type flakyGateClient struct {
	*listingGateClient
	mu    sync.Mutex
	fails int
	down  bool // every call answers context.Canceled (the replica is shutting down)
}

func (f *flakyGateClient) GetPullRequest(ctx context.Context, repo string, n int) (forge.PullRef, error) {
	if f.down {
		return forge.PullRef{}, fmt.Errorf("Get \"https://api.github.com/repos/%s/pulls/%d\": %w", repo, n, context.Canceled)
	}
	return f.listingGateClient.GetPullRequest(ctx, repo, n)
}

func (f *flakyGateClient) SetCommitStatus(ctx context.Context, repo, sha string, st forge.CommitStatus) error {
	f.mu.Lock()
	fail := f.fails > 0
	if fail {
		f.fails--
	}
	f.mu.Unlock()
	if f.down {
		return fmt.Errorf("Post status: %w", context.Canceled)
	}
	if fail {
		f.setCalls++
		return &forge.StatusError{Provider: forge.ProviderGitHub, Op: "set commit status", Code: http.StatusBadGateway}
	}
	return f.listingGateClient.SetCommitStatus(ctx, repo, sha, st)
}

// A deferred verdict meets the relaunch lane: a transient failure at replay,
// a replay cut by a shutdown, a relaunch whose own verdict waits — none of
// them buys a paid relaunch or a false escalation.
func TestDeferralAndRelaunchInteractions(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "revi/review"
		botID  = "dep-update-guard"
		base   = "https://iterion.test"
	)
	ctx := context.Background()
	type world struct {
		s        *Server
		gc       *flakyGateClient
		board    native.BoardStore
		launched *int
	}
	build := func(t *testing.T) world {
		t.Helper()
		s := newWebhookTestServer(t)
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs
		s.cfg.PublicURL = base
		conns := forge.NewMemoryConnectionStore()
		if err := conns.Create(ctx, forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
		ints := forge.NewMemoryRepoIntegrationStore()
		if err := ints.Create(ctx, forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{botID}, WebhookID: "w1",
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(ctx, webhooks.Config{ID: "w1", TenantID: team, BotIDs: []string{botID}}); err != nil {
			t.Fatal(err)
		}
		gc := &flakyGateClient{listingGateClient: &listingGateClient{fakeGateClient: fakeGateClient{headSHA: head}}}
		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
		board, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
		launched := 0
		s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
			launched++
			return "run-relaunched", nil
		}
		return world{s: s, gc: gc, board: board, launched: &launched}
	}
	seedRun := func(t *testing.T, s *Server, token string, status store.RunStatus) string {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(ctx, id, "dep_update_guard", map[string]any{
			"pr_url": prURL, "gate_context": gateNm, "head_sha": head,
			forgePublishVarToken: token,
			forgePublishVarURL:   base + "/api/v1/forge/publish-review",
		})
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = botID
		run.Status = status
		if status != store.RunStatusFinished {
			run.Error = "provider error: 529 overloaded"
		}
		if err := s.cfg.Store.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	cards := func(t *testing.T, w world) []*native.Issue {
		t.Helper()
		got, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	verdict := publishReviewGate{Enabled: true, Context: gateNm, AuditedSHA: head, Threshold: "high"}

	// Q5: a deferred verdict whose replay meets ONE transient 502 (not a rate
	// limit) is cleared, and the same offer pays a relaunch of a revision the
	// first review already judged.
	t.Run("a transient 502 at replay re-arms instead of paying a relaunch", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		if ok, err := w.s.forgePublishTokens.update("run-token", func(g *ForgePublishGrant) {
			g.Deferred = &gateDeferral{Gate: verdict, Repo: repo, Number: 7, RetryAt: now.Add(-time.Minute), Attempts: 1}
		}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		r1 := seedRun(t, w.s, "run-token", store.RunStatusFinished) // the review FINISHED; only its verdict waited
		w.gc.statuses = []forge.CommitStatus{{Context: gateNm, State: forge.CommitStatePending,
			Description: gateInFlightDescription, TargetURL: deeplink.Run(base, r1)}}
		w.gc.fails = 1
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep)
		g, _ := w.s.forgePublishTokens.lookup("run-token")
		for i, st := range w.gc.posted {
			t.Logf("status %d: %s %q", i, st.State, st.Description)
		}
		t.Logf("deferral after the offer: %+v; relaunches launched: %d", g.Deferred, *w.launched)
		if *w.launched != 0 {
			t.Errorf("a transient 502 on the replay of a JUDGED revision's verdict cost a paid relaunch (%d) and a synthetic failure", *w.launched)
		}
	})

	// Q5b: the replay runs while the sweeping replica shuts down (its ctx is
	// cancelled): the deferral is cleared; the next leader's offer pays.
	t.Run("a replay cut by a shutdown keeps its deferral", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		if ok, err := w.s.forgePublishTokens.update("run-token", func(g *ForgePublishGrant) {
			g.Deferred = &gateDeferral{Gate: verdict, Repo: repo, Number: 7, RetryAt: now.Add(-time.Minute), Attempts: 1}
		}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		r1 := seedRun(t, w.s, "run-token", store.RunStatusFinished)
		w.gc.statuses = []forge.CommitStatus{{Context: gateNm, State: forge.CommitStatePending,
			Description: gateInFlightDescription, TargetURL: deeplink.Run(base, r1)}}
		w.gc.down = true
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep)
		g, _ := w.s.forgePublishTokens.lookup("run-token")
		t.Logf("deferral after the cancelled replay: %+v", g.Deferred)
		w.gc.down = false
		_ = w.s.reconcileGateForRunID(ctx, r1, gateTriggerSweep) // the next leader's sweep
		for i, st := range w.gc.posted {
			t.Logf("status %d: %s %q", i, st.State, st.Description)
		}
		if *w.launched != 0 {
			t.Errorf("a replay cut by a shutdown lost the deferred verdict: next offer posted a synthetic failure and paid %d relaunch(es)", *w.launched)
		}
	})

	// Q6: the head's one relaunch FINISHED its review and its verdict waits
	// out a rate limit on its own grant; another dead run of the same bot on
	// the same head is offered meanwhile.
	t.Run("a relaunch whose verdict is deferred is not escalated as dead", func(t *testing.T) {
		w := build(t)
		now := time.Now().UTC()
		w.s.forgePublishTokens.Register("rel-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo,
			Deferred: &gateDeferral{Gate: verdict, Repo: repo, Number: 7, RetryAt: now.Add(40 * time.Minute), Attempts: 1}})
		rel := seedRun(t, w.s, "rel-token", store.RunStatusFinished)
		relaunchIdem := gateRelaunchIdemKey(deadGateRun{grant: ForgePublishGrant{TeamID: team}, repo: repo, number: 7, pr: forge.PullRef{HeadSHA: head}}, botID)
		launchedAt := now.Add(-20 * time.Minute)
		if err := w.s.webhookDeliveries.Insert(ctx, webhooks.Delivery{
			ID: "d-rel", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem, BotID: botID,
			Status: webhooks.StatusLaunched, RunID: rel, Attempts: 1, ReceivedAt: launchedAt, LaunchedAt: &launchedAt,
		}); err != nil {
			t.Fatal(err)
		}
		r1 := seedRun(t, w.s, "run-token", store.RunStatusFailedResumable)  // the first death
		r1x := seedRun(t, w.s, "run-token", store.RunStatusFailedResumable) // another dead run of the bot on the head
		// The relaunch's in-flight claim was refused by the same limit: the head keeps the first death's marker.
		w.gc.statuses = []forge.CommitStatus{{Context: gateNm, State: forge.CommitStateFailure,
			Description: gateInterruptedDescription, TargetURL: deeplink.Run(base, r1)}}
		// The relaunch's own offer stays silent (its verdict waits) …
		_ = w.s.reconcileGateForRunID(ctx, rel, gateTriggerSweep)
		silent := len(cards(t, w))
		// … but r1x's offer reads the relaunch as dead.
		_ = w.s.reconcileGateForRunID(ctx, r1x, gateTriggerSweep)
		got := cards(t, w)
		t.Logf("cards after the relaunch's own offer: %d; after r1x's offer: %d", silent, len(got))
		if len(got) != 0 {
			t.Errorf("escalated 'the automatic relaunch died too' while the relaunch's verdict waits out a rate limit until %s:\n%s",
				now.Add(40*time.Minute).Format(time.RFC3339), got[0].Body)
		}
	})
}

// ctxHonoringDeliveries refuses an update on a done context, as a networked
// store does.
type ctxHonoringDeliveries struct{ webhooks.DeliveryStore }

func (c ctxHonoringDeliveries) Update(ctx context.Context, d webhooks.Delivery) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.DeliveryStore.Update(ctx, d)
}

// refusingUpdateDeliveries refuses every update.
type refusingUpdateDeliveries struct{ webhooks.DeliveryStore }

func (refusingUpdateDeliveries) Update(context.Context, webhooks.Delivery) error {
	return errors.New("mongo: server selection timeout")
}
