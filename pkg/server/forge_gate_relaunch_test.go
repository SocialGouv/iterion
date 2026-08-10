package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The relaunch lane is the recovery half of the reconciler: a gating run that
// died without a verdict gets its bot re-run ONCE per head, through the same
// admission tail as any webhook launch, and a head whose one attempt is spent
// graduates to a board card instead of looping. Pinned here: the launch fires
// with the original run's inputs (minus the per-run grant), the once-per-head
// bound holds, the board card dedups, and a bot the repo no longer enables is
// never relaunched.
func TestGateRelaunch(t *testing.T) {
	const (
		team   = "t1"
		repo   = "acme/widgets"
		prURL  = "https://github.com/acme/widgets/pull/7"
		head   = "cafe1234cafe1234cafe1234cafe1234cafe1234"
		gateNm = "revi/review"
		botID  = "dep-update-guard"
	)

	type world struct {
		s        *Server
		gc       *listingGateClient
		board    native.BoardStore
		launched *int
		lastVars *map[string]string
	}
	build := func(t *testing.T, tune func(*Server, *forge.RepoIntegration, *webhooks.Config)) world {
		t.Helper()
		s := newWebhookTestServer(t)

		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.Store = rs
		s.cfg.PublicURL = "https://iterion.test"

		conns := forge.NewMemoryConnectionStore()
		conn := forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}
		if err := conns.Create(context.Background(), conn); err != nil {
			t.Fatal(err)
		}
		s.forgeConnections = conns
		s.forgePublishTokens = NewForgePublishTokenRegistry()
		s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})

		ints := forge.NewMemoryRepoIntegrationStore()
		integ := forge.RepoIntegration{
			ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
			BotIDs: []string{botID}, WebhookID: "w1",
			LaunchVars: map[string]string{gateContextVar: gateNm},
		}
		cfg := webhooks.Config{ID: "w1", TenantID: team, BotIDs: []string{botID}}
		if tune != nil {
			tune(s, &integ, &cfg)
		}
		if err := ints.Create(context.Background(), integ); err != nil {
			t.Fatal(err)
		}
		s.forgeIntegrations = ints
		if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}

		gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: head}}
		s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) {
			return gc, nil
		}

		board, err := native.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }

		launched := 0
		var lastVars map[string]string
		s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
			launched++
			lastVars = vars
			return "run-relaunched", nil
		}
		return world{s: s, gc: gc, board: board, launched: &launched, lastVars: &lastVars}
	}

	deadInputs := map[string]any{
		"pr_url":             prURL,
		"gate_context":       gateNm,
		"head_sha":           head,
		"arm_automerge":      "true", // the operator's pinned vars must survive the relaunch
		forgePublishVarToken: "run-token",
		forgePublishVarURL:   "https://iterion.test/api/v1/forge/publish-review",
	}
	seedDeadRun := func(t *testing.T, s *Server) string {
		t.Helper()
		id, err := store.GenerateRunID()
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.cfg.Store.CreateRun(context.Background(), id, "dep_update_guard", deadInputs)
		if err != nil {
			t.Fatal(err)
		}
		run.BotID = botID
		run.Status = store.RunStatusFailedResumable // dead: no retry armed
		run.Error = "budget exceeded: duration (2401987036905/2400000000000)"
		if err := s.cfg.Store.SaveRun(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	// The bot id is part of the claim key: two gating bots on one head are two
	// independent recoveries.
	relaunchIdem := knowledge.ChecksumHex([]byte("gaterelaunch|" + team + "|" + repo + "|7|" + head + "|" + botID))

	t.Run("a dead gating run posts its failure and is relaunched once", func(t *testing.T) {
		w := build(t, nil)
		runID := seedDeadRun(t, w.s)
		if err := w.s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		// Two statuses, and their ORDER is the contract: the synthetic failure
		// lands BEFORE any recovery (so the PR is never blind even when the
		// relaunch cannot happen), and the recovery run then claims the check
		// back to "running" — a review really is in flight again, and leaving
		// a red cross up would misreport a verdict that was never reached.
		if w.gc.setCalls != 2 {
			t.Fatalf("posted %d statuses, want 2 (failure, then the recovery's claim)", w.gc.setCalls)
		}
		if got := w.gc.posted[0]; got.State != forge.CommitStateFailure || !isSyntheticGateInterruption(got.Description) {
			t.Fatalf("first status = %s/%q, want the synthetic failure BEFORE any recovery", got.State, got.Description)
		}
		if got := w.gc.posted[1]; !isGateInFlight(got) {
			t.Fatalf("second status = %s/%q, want the relaunched run's in-flight claim", got.State, got.Description)
		}
		if *w.launched != 1 {
			t.Fatalf("launched %d runs, want 1 — the dead review's bot must be re-run", *w.launched)
		}
		vars := *w.lastVars
		if vars["pr_url"] != prURL || vars["arm_automerge"] != "true" {
			t.Errorf("relaunch dropped original launch vars: %v", vars)
		}
		if tok := vars[forgePublishVarToken]; tok == "" || tok == "run-token" {
			t.Errorf("publish token = %q — the tail must mint a FRESH grant, not reuse the dead run's", tok)
		}
		// The claim is durable: the delivery row under the per-head key is
		// what makes attempt two impossible.
		if d, err := w.s.webhookDeliveries.GetByIdempotencyKey(context.Background(), relaunchIdem); err != nil || d.RunID != "run-relaunched" {
			t.Errorf("no durable claim under the per-head key (%v, %+v)", err, d)
		}
	})

	// The natural second-death sequence: the first death posted the synthetic
	// failure and spent the head's one relaunch; now the RELAUNCHED run dies
	// too. The gate carries the reconciler's own marker — which must not read
	// as "already posted", or this exact case would go silent right where the
	// recovery runs out (found adversarially: an earlier version stood down on
	// its own synthetic status and the board escalation was unreachable).
	t.Run("the second death on the same head escalates to the board instead", func(t *testing.T) {
		w := build(t, nil)
		// The head's one attempt is already spent…
		if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
			ID: "d-spent", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem,
			Status: webhooks.StatusLaunched, RunID: "run-prior-relaunch",
		}); err != nil {
			t.Fatal(err)
		}
		// …and the gate still shows the first death's synthetic failure.
		w.gc.statuses = []forge.CommitStatus{{
			Context: gateNm, State: forge.CommitStateFailure,
			Description: gateInterruptedDescription,
		}}
		runID := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if *w.launched != 0 {
			t.Fatalf("launched %d runs, want 0 — one relaunch per head, ever", *w.launched)
		}
		if w.gc.setCalls != 1 {
			t.Fatalf("posted %d statuses, want 1 — the synthetic failure is refreshed with the newest death's reason", w.gc.setCalls)
		}
		cards, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards = %d (%v), want exactly 1", len(cards), err)
		}
		if !strings.Contains(cards[0].Body, "run-prior-relaunch") || !strings.Contains(cards[0].Body, "budget exceeded") {
			t.Errorf("the card must name the dead runs and the reason; got body:\n%s", cards[0].Body)
		}

		// A third death on the same head adds no second card.
		runID2 := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID2))
		cards, err = w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards after a third death = %d (%v), want still 1", len(cards), err)
		}
	})

	// "Duplicate" is not "the replacement died". The idempotency claim is a
	// read-then-insert and the gate sweep runs UNELECTED on every replica, so
	// two passes landing on one dead run give one launch and one duplicate —
	// for the same, live, replacement. Escalating on that files a card telling
	// a human the automation is out of moves while the replacement is alive and
	// reviewing.
	t.Run("a relaunch still in flight does not escalate", func(t *testing.T) {
		w := build(t, nil)
		alive, err := w.s.cfg.Store.CreateRun(context.Background(), "run-prior-relaunch", "dep_update_guard", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		alive.Status = store.RunStatusRunning
		if err := w.s.cfg.Store.SaveRun(context.Background(), alive); err != nil {
			t.Fatal(err)
		}
		if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
			ID: "d-inflight", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem,
			Status: webhooks.StatusLaunched, RunID: alive.ID,
		}); err != nil {
			t.Fatal(err)
		}
		w.gc.statuses = []forge.CommitStatus{{
			Context: gateNm, State: forge.CommitStateFailure,
			Description: gateInterruptedDescription,
		}}
		runID := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID))

		cards, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil {
			t.Fatal(err)
		}
		if len(cards) != 0 {
			t.Fatalf("board cards = %d, want 0 — a human was told automation is out of moves while run %s is still reviewing", len(cards), alive.ID)
		}
	})

	t.Run("a real red verdict is left alone entirely", func(t *testing.T) {
		w := build(t, nil)
		// Vetty's own red verdict — the review HAPPENED. Nothing to reconcile,
		// nothing to relaunch, nothing for the board.
		w.gc.statuses = []forge.CommitStatus{{
			Context: gateNm, State: forge.CommitStateFailure,
			Description: "1 blocking finding (≥high)",
		}}
		runID := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if w.gc.setCalls != 0 || *w.launched != 0 {
			t.Fatalf("touched a PR whose gate carries a real verdict (posts=%d launches=%d)", w.gc.setCalls, *w.launched)
		}
		if cards, _ := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}}); len(cards) != 0 {
			t.Fatalf("filed %d cards over a real verdict", len(cards))
		}
	})

	t.Run("a bot the repo no longer enables is not relaunched", func(t *testing.T) {
		w := build(t, func(_ *Server, i *forge.RepoIntegration, _ *webhooks.Config) {
			i.BotIDs = []string{"someone-else"}
		})
		runID := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if *w.launched != 0 {
			t.Fatal("relaunched a bot the operator has since removed from the repo")
		}
	})
}
