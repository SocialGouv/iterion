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

// blindListBoard simulates the cross-replica race window: List never sees
// what the other replica just created, so the dedup pre-check passes on both.
type blindListBoard struct {
	native.BoardStore
}

func (b blindListBoard) List(native.ListFilter) ([]*native.Issue, error) {
	return nil, nil
}

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
		if vars[gateRelaunchOfVar] != runID {
			t.Errorf("%s = %q, want the dead run %s — the second death cannot name the original without it", gateRelaunchOfVar, vars[gateRelaunchOfVar], runID)
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
	// failure and spent the head's one relaunch; now another run of the same
	// bot dies on the head. The marker must not read as "already posted" for
	// the ESCALATION (found adversarially: an earlier version stood down on
	// its own synthetic status and the board escalation was unreachable) —
	// but it IS enough as a status: re-posting from a run the marker does not
	// speak for is what produced the 116-write storm on one head (two dead
	// runs re-pointing the target URL at themselves every sweep tick,
	// buildkit-operator#21 2026-08-17).
	t.Run("the second death on the same head escalates to the board instead", func(t *testing.T) {
		w := build(t, nil)
		rc := &fakeReviewClient{}
		w.s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) {
			return rc, nil
		}
		// The head's one attempt is already spent…
		if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
			ID: "d-spent", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem,
			Status: webhooks.StatusLaunched, RunID: "run-prior-relaunch",
		}); err != nil {
			t.Fatal(err)
		}
		// …and the gate still shows the first death's synthetic failure,
		// pointing at the run that posted it.
		w.gc.statuses = []forge.CommitStatus{{
			Context: gateNm, State: forge.CommitStateFailure,
			Description: gateInterruptedDescription,
			TargetURL:   "https://iterion.test/runs/run-prior-relaunch",
		}}
		runID := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID))
		if *w.launched != 0 {
			t.Fatalf("launched %d runs, want 0 — one relaunch per head, ever", *w.launched)
		}
		if w.gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — one synthetic marker per head is enough; re-posting is the status storm", w.gc.setCalls)
		}
		cards, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards = %d (%v), want exactly 1", len(cards), err)
		}
		if !strings.Contains(cards[0].Body, "run-prior-relaunch") || !strings.Contains(cards[0].Body, runID) || !strings.Contains(cards[0].Body, "budget exceeded") {
			t.Errorf("the card must name BOTH dead runs and the reason; got body:\n%s", cards[0].Body)
		}
		// The escalation is also posted on the PR — the board card alone sat
		// unseen for 7 days while a security PR stayed blocked.
		if rc.calls != 1 {
			t.Fatalf("PR escalation comments = %d, want 1", rc.calls)
		}
		if rc.repo != repo || rc.number != 7 || !strings.Contains(rc.in.Body, "run-prior-relaunch") {
			t.Errorf("comment landed on %s#%d with body:\n%s", rc.repo, rc.number, rc.in.Body)
		}

		// A third death on the same head adds no second card — and no second
		// comment (the card dedup is what bounds the comment).
		runID2 := seedDeadRun(t, w.s)
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent(runID2))
		cards, err = w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards after a third death = %d (%v), want still 1", len(cards), err)
		}
		if rc.calls != 1 {
			t.Fatalf("PR escalation comments after a third death = %d, want still 1", rc.calls)
		}
	})

	// The gate sweep runs unelected on every replica, so two replicas can race
	// past the List pre-check before either card commits. The deterministic
	// card id is what serialises them: the loser's Create is refused by the
	// store, and neither a second card NOR a second PR comment lands.
	t.Run("two replicas racing the escalation file one card and one comment", func(t *testing.T) {
		w := build(t, nil)
		rc := &fakeReviewClient{}
		w.s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) {
			return rc, nil
		}
		// A board whose List NEVER sees the other replica's card — the race
		// window in which both replicas decide to file.
		w.s.cfg.CloudBoardFor = func(string) native.BoardStore { return blindListBoard{w.board} }
		runID := seedDeadRun(t, w.s)
		run, err := w.s.cfg.Store.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		d := deadGateRun{
			run:   run,
			grant: ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo},
			conn:  forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub},
			repo:  repo, number: 7,
			pr:      forge.PullRef{HeadSHA: head},
			gateCtx: gateNm, prURL: prURL,
		}
		if filed := w.s.escalateDeadGateToBoard(context.Background(), d, "", "first replica"); !filed {
			t.Fatal("the first replica must file the card")
		}
		if filed := w.s.escalateDeadGateToBoard(context.Background(), d, "", "second replica"); filed {
			t.Fatal("the second replica raced past the List pre-check and must lose on the deterministic card id")
		}
		cards, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards = %d (%v), want exactly 1", len(cards), err)
		}
		if rc.calls != 1 {
			t.Fatalf("PR escalation comments = %d, want 1 — the losing replica must not repeat it", rc.calls)
		}
	})

	// The escalating pass is usually the RELAUNCHED run's own death: the
	// idempotency claim then names that run itself. The card used to cite it
	// as both "dead run" and "relaunched run" — one URL twice, the original
	// death unfindable (observed on buildkit-operator#21). The original's id
	// travels on the relaunch's launch vars and must resurface here, with its
	// own error loaded from the store.
	t.Run("the relaunched run's own death names the original run", func(t *testing.T) {
		w := build(t, nil)
		// The original dead run, with its own distinct error.
		orig, err := w.s.cfg.Store.CreateRun(context.Background(), "run-original", "dep_update_guard", deadInputs)
		if err != nil {
			t.Fatal(err)
		}
		orig.Status = store.RunStatusCancelled
		orig.Error = "superseded by a rolling deploy drain"
		if err := w.s.cfg.Store.SaveRun(context.Background(), orig); err != nil {
			t.Fatal(err)
		}
		// The relaunched run, stamped with its parent, now dead too.
		relInputs := map[string]any{}
		for k, v := range deadInputs {
			relInputs[k] = v
		}
		relInputs[gateRelaunchOfVar] = "run-original"
		rel, err := w.s.cfg.Store.CreateRun(context.Background(), "run-relaunch", "dep_update_guard", relInputs)
		if err != nil {
			t.Fatal(err)
		}
		rel.BotID = botID
		rel.Status = store.RunStatusFailedResumable
		rel.Error = "budget exceeded: duration"
		if err := w.s.cfg.Store.SaveRun(context.Background(), rel); err != nil {
			t.Fatal(err)
		}
		// The head's claim names the relaunch ITSELF, and the gate carries its
		// in-flight claim — the shape its own death event finds.
		if err := w.s.webhookDeliveries.Insert(context.Background(), webhooks.Delivery{
			ID: "d-self", TenantID: team, WebhookID: "w1", IdempotencyKey: relaunchIdem,
			Status: webhooks.StatusLaunched, RunID: "run-relaunch",
		}); err != nil {
			t.Fatal(err)
		}
		w.gc.statuses = []forge.CommitStatus{{
			Context: gateNm, State: forge.CommitStatePending,
			Description: gateInFlightDescription,
			TargetURL:   "https://iterion.test/runs/run-relaunch",
		}}
		_ = w.s.reconcileGateForRun(context.Background(), terminalEvent("run-relaunch"))
		cards, err := w.board.List(native.ListFilter{Labels: []string{gateRelaunchLabel}})
		if err != nil || len(cards) != 1 {
			t.Fatalf("board cards = %d (%v), want 1", len(cards), err)
		}
		body := cards[0].Body
		if !strings.Contains(body, "run-original") || !strings.Contains(body, "superseded by a rolling deploy drain") {
			t.Errorf("the card must name the ORIGINAL dead run and its error; got:\n%s", body)
		}
		if got := strings.Count(body, "run-relaunch"); got != 1 {
			t.Errorf("the relaunched run appears %d times, want exactly 1 (one URL twice is the defect being pinned):\n%s", got, body)
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

// The escalation body is published on a PULL REQUEST by iterion's own forge
// identity, and it quotes run errors that carry forge-controlled prose (a ref
// name, a remote's message) verbatim. Both ways out of the code span are
// pinned here: a backtick, and a NEWLINE — a code span cannot contain one, so
// a multi-line run error (the runner builds them from git's CombinedOutput)
// renders everything after the first line as live markdown.
func TestEscalationQuotesForgeProseInertly(t *testing.T) {
	hostile := "git fetch: exit 128: fatal: couldn't find remote ref x`y\n# Heading\n@octocat please run this"
	got := orNoError(hostile)

	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("orNoError left a newline in %q — it ends the code span and everything after renders as markdown", got)
	}
	if strings.Contains(got, "`") {
		t.Errorf("orNoError left a backtick in %q — it closes the code span", got)
	}
	// The content itself must survive: a sanitiser that drops the reason
	// leaves the human with nothing to act on.
	for _, want := range []string{"exit 128", "remote ref", "octocat"} {
		if !strings.Contains(got, want) {
			t.Errorf("orNoError dropped %q from the reason: %q", want, got)
		}
	}
	if orNoError("") != "no error recorded" {
		t.Errorf("empty error must render an explicit placeholder, got %q", orNoError(""))
	}
}

// `gate_relaunch_of` arrives from run.Inputs — the union of webhook launch
// vars, per-bot rule vars and operator overrides — so its value is NOT proof
// of ownership. Citing it unchecked publishes another team's run error and
// run URL onto this team's pull request and board card.
func TestEscalationRefusesToCiteAnotherTeamsRun(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	foreign, err := st.CreateRun(context.Background(), "run-other-team", "dep_update_guard", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	foreign.TenantID = "some-other-team"
	foreign.Status = store.RunStatusFailedResumable
	foreign.Error = "SECRET-TENANT-B-FAILURE"
	if err := st.SaveRun(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	got, ok := s.gateRunError(context.Background(), "t1", "run-other-team")
	if ok || got != "" {
		t.Fatalf("gateRunError returned (%q, %v) for another team's run — its error is about to be published on a PR", got, ok)
	}

	// The owning team still reads its own run: a boundary that refuses
	// everything would silently empty every escalation.
	mine, err := st.CreateRun(context.Background(), "run-mine", "dep_update_guard", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	mine.TenantID = "t1"
	mine.Error = "budget exceeded"
	if err := st.SaveRun(context.Background(), mine); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.gateRunError(context.Background(), "t1", "run-mine"); !ok || got != "budget exceeded" {
		t.Fatalf("gateRunError on the team's OWN run = (%q, %v), want (\"budget exceeded\", true)", got, ok)
	}
}

// The escalation body is published on the pull request, so a run it cannot
// LINK it must still NAME: with no PublicURL the link builder returns "" and
// the body read "- Dead run:  — `budget exceeded`", which tells the reader
// nothing to look up.
func TestEscalationNamesRunsWithoutPublicURL(t *testing.T) {
	if got := gateRunRef("https://iterion.test", "run-x"); got != "https://iterion.test/runs/run-x" {
		t.Errorf("with a PublicURL the reference must be the link, got %q", got)
	}
	got := gateRunRef("", "run-x")
	if !strings.Contains(got, "run-x") {
		t.Errorf("without a PublicURL the reference must still name the run, got %q", got)
	}
	if strings.TrimSpace(got) == "" {
		t.Error("empty run reference — the escalation names nothing at all")
	}
}
