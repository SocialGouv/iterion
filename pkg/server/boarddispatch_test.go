package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

type fakeBoardCoord struct {
	mu       sync.Mutex
	cands    []boardmongo.Candidate
	claimed  map[string]string
	states   map[string]string
	claimErr map[string]error
	stateErr map[string]error
	renews   map[string]int
}

func newFakeBoardCoord(cands ...boardmongo.Candidate) *fakeBoardCoord {
	return &fakeBoardCoord{cands: cands, claimed: map[string]string{}, states: map[string]string{}, claimErr: map[string]error{}, stateErr: map[string]error{}, renews: map[string]int{}}
}

// ListEligible honours the real coordinator's contract — unclaimed cards in
// one of the requested states, capped — so a caller sweeping the wrong state
// list (or ignoring the claim filter) fails here instead of passing
// vacuously. The UpdatedAt ordering (newestFirst) is exercised against the
// real store in boardmongo's conformance suite, not here: the fake carries
// no timestamps.
func (f *fakeBoardCoord) ListEligible(_ context.Context, states []string, limit int, _ bool) ([]boardmongo.Candidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []boardmongo.Candidate
	for _, c := range f.cands {
		state := c.Issue.State
		if moved, ok := f.states[c.Issue.ID]; ok {
			state = moved // a SetState since construction wins, like the real store
		}
		if f.claimed[c.Issue.ID] != "" || !slices.Contains(states, state) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeBoardCoord) Claim(_ context.Context, _, id, marker string) (tracker.ClaimToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.claimErr[id]; err != nil {
		return tracker.ClaimToken{}, err
	}
	if f.claimed[id] != "" {
		return tracker.ClaimToken{}, errors.New("conflict")
	}
	f.claimed[id] = marker
	return tracker.ClaimToken{Marker: marker, Epoch: 1}, nil
}

func (f *fakeBoardCoord) SetState(_ context.Context, _, id, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stateErr[id]; err != nil {
		return err
	}
	f.states[id] = state
	return nil
}

func (f *fakeBoardCoord) Release(_ context.Context, _, id, _ string) error {
	f.mu.Lock()
	delete(f.claimed, id)
	f.mu.Unlock()
	return nil
}

// The fenced family mirrors the real coordinator's contract: writes land
// only while the token's marker still holds the claim; a superseded token
// gets tracker.ErrClaimConflict. renews counts heartbeats for the
// heartbeat test.
func (f *fakeBoardCoord) RenewClaim(_ context.Context, _, id string, tok tracker.ClaimToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renews[id]++
	if f.claimed[id] != tok.Marker {
		return tracker.ErrClaimConflict
	}
	return nil
}

func (f *fakeBoardCoord) SetStateOwned(ctx context.Context, tenant, id, state string, tok tracker.ClaimToken) error {
	f.mu.Lock()
	held := f.claimed[id] == tok.Marker
	f.mu.Unlock()
	if !held {
		return tracker.ErrClaimConflict
	}
	return f.SetState(ctx, tenant, id, state)
}

func (f *fakeBoardCoord) ReleaseOwned(ctx context.Context, tenant, id string, tok tracker.ClaimToken) error {
	f.mu.Lock()
	holder, ok := f.claimed[id]
	f.mu.Unlock()
	if ok && holder != tok.Marker {
		return tracker.ErrClaimConflict
	}
	return f.Release(ctx, tenant, id, tok.Marker)
}

func (f *fakeBoardCoord) ListExpiredClaimCandidates(_ context.Context, _ time.Time, _ int) ([]boardmongo.ExpiredCandidate, error) {
	return nil, nil
}

func (f *fakeBoardCoord) ReclaimExpired(_ context.Context, _, id string, _ tracker.ClaimToken, marker string, _ time.Time) (tracker.ClaimToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed[id] = marker
	return tracker.ClaimToken{Marker: marker, Epoch: 99}, nil
}

func readyCard(id, bot string) boardmongo.Candidate {
	return boardmongo.Candidate{Tenant: "t1", Issue: native.Issue{ID: id, Bot: bot, State: native.StateReady}}
}

// TestStampCardLastRun: the cloud coordinator must stamp the launched run onto
// the card via the CloudBoardFor seam (the local dispatcher does this itself;
// the cloud path launches through runs.Launch and previously never stamped, so
// a running card had no link to its run). A missing seam / empty run id / store
// error is a silent no-op — stamping is best-effort and never fails the run.
func TestStampCardLastRun(t *testing.T) {
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := boardStore.Create(native.Issue{Title: "x", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }

	s.stampCardLastRun("t1", iss.ID, "run-live-1")
	got, err := boardStore.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRunID != "run-live-1" {
		t.Fatalf("card LastRunID = %q, want run-live-1", got.LastRunID)
	}

	// Empty run id and a nil seam must be no-ops, not panics.
	s.stampCardLastRun("t1", iss.ID, "")
	if got, _ := boardStore.Get(iss.ID); got.LastRunID != "run-live-1" {
		t.Fatalf("empty run id must not clobber the stamp, got %q", got.LastRunID)
	}
	(&Server{}).stampCardLastRun("t1", iss.ID, "run-x") // CloudBoardFor nil → no-op
}

// TestSetCardAwaitingInput: the cloud coordinator denormalizes the pause hint
// onto the card via the same CloudBoardFor seam, so the board grid can badge a
// paused card without an N+1 run fetch. Set true on pause, clear false on
// dispatch; a nil seam is a silent no-op.
func TestSetCardAwaitingInput(t *testing.T) {
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := boardStore.Create(native.Issue{Title: "x", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }

	s.setCardAwaitingInput("t1", iss.ID, true)
	if got, _ := boardStore.Get(iss.ID); !got.AwaitingInput {
		t.Fatalf("card AwaitingInput = false, want true")
	}
	s.setCardAwaitingInput("t1", iss.ID, false)
	if got, _ := boardStore.Get(iss.ID); got.AwaitingInput {
		t.Fatalf("card AwaitingInput = true, want cleared")
	}
	(&Server{}).setCardAwaitingInput("t1", iss.ID, true) // CloudBoardFor nil → no-op
}

func TestBoardDispatcher_ClaimsProcessesAndMoves(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"), readyCard("native:2", "sec-audit-source"))
	var pmu sync.Mutex
	processed := map[string]int{}
	d := newBoardDispatcher(f, func(_ context.Context, _ string, iss native.Issue) error {
		pmu.Lock()
		processed[iss.ID]++
		pmu.Unlock()
		return nil
	}, "replica-A", 4, nil)

	if n := d.tick(context.Background()); n != 2 {
		t.Fatalf("tick should claim 2, got %d", n)
	}
	d.wg.Wait()

	if processed["native:1"] != 1 || processed["native:2"] != 1 {
		t.Errorf("each card should process once: %v", processed)
	}
	if f.states["native:1"] != native.StateDone || f.states["native:2"] != native.StateDone {
		t.Errorf("cards should move to done: %v", f.states)
	}
	if len(f.claimed) != 0 {
		t.Errorf("cards should be released: %v", f.claimed)
	}
}

func TestBoardDispatcher_FailedRunMovesToBlocked(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return errors.New("run failed")
	}, "replica-A", 4, nil)
	d.tick(context.Background())
	d.wg.Wait()
	if f.states["native:1"] != native.StateBlocked {
		t.Errorf("failed run should move card to blocked, got %q", f.states["native:1"])
	}
}

func TestBoardDispatcher_PausedRunMovesToAwaitingInput(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	// A process func that signals a pause (errCardPaused) rather than a failure.
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return fmt.Errorf("run r1 paused (paused_waiting_human): %w", errCardPaused)
	}, "replica-A", 4, nil)
	d.tick(context.Background())
	d.wg.Wait()
	if f.states["native:1"] != native.StateAwaitingInput {
		t.Errorf("paused run should move card to awaiting_input, got %q", f.states["native:1"])
	}
	if len(f.claimed) != 0 {
		t.Errorf("card should be released after a pause: %v", f.claimed)
	}
}

// TestBoardDispatcher_SweepMovesParkedCards: a cloud card parks UNCLAIMED in
// awaiting_input and every resume surface completes the run outside the
// dispatcher's poll loop, so only sweepParked can move it on. finished →
// done, hard-failed → blocked, resumable/in-flight statuses stay parked; the
// denormalized ⏸ badge is cleared on every terminal move.
func TestBoardDispatcher_SweepMovesParkedCards(t *testing.T) {
	awaiting := func(id, runID string) boardmongo.Candidate {
		return boardmongo.Candidate{Tenant: "t1", Issue: native.Issue{ID: id, State: native.StateAwaitingInput, LastRunID: runID}}
	}
	f := newFakeBoardCoord(
		awaiting("native:done", "run-finished"),
		awaiting("native:dead", "run-failed"),
		awaiting("native:wait", "run-paused"),
		awaiting("native:redo", "run-resumable"),
		awaiting("native:none", ""), // never dispatched — must be left alone
	)
	statuses := map[string]store.RunStatus{
		"run-finished":  store.RunStatusFinished,
		"run-failed":    store.RunStatusFailed,
		"run-paused":    store.RunStatusPausedWaitingHuman,
		"run-resumable": store.RunStatusFailedResumable,
	}
	var bmu sync.Mutex
	badgeCleared := map[string]bool{}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("sweep must never dispatch a parked card")
		return nil
	}, "replica-A", 4, nil)
	d.statusFor = func(_ context.Context, _, runID string) (store.RunStatus, error) {
		return statuses[runID], nil
	}
	d.clearBadge = func(_, id string) {
		bmu.Lock()
		badgeCleared[id] = true
		bmu.Unlock()
	}

	d.sweepParked(context.Background())

	if got := f.states["native:done"]; got != native.StateDone {
		t.Errorf("finished card state = %q, want %q", got, native.StateDone)
	}
	if got := f.states["native:dead"]; got != native.StateBlocked {
		t.Errorf("hard-failed card state = %q, want %q", got, native.StateBlocked)
	}
	for _, id := range []string{"native:wait", "native:redo", "native:none"} {
		if got, moved := f.states[id]; moved {
			t.Errorf("card %s must stay parked, was moved to %q", id, got)
		}
	}
	if !badgeCleared["native:done"] || !badgeCleared["native:dead"] {
		t.Errorf("badge must be cleared on terminal moves: %v", badgeCleared)
	}
	if badgeCleared["native:wait"] || badgeCleared["native:redo"] {
		t.Errorf("badge must survive on parked cards: %v", badgeCleared)
	}

	// nil statusFor (unwired) must be a hard no-op.
	d2 := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil }, "replica-A", 4, nil)
	d2.sweepParked(context.Background())
}

func TestBoardDispatcher_ClaimConflictSkips(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	f.claimErr["native:1"] = errors.New("held by another replica")
	var processed int
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		processed++
		return nil
	}, "replica-A", 4, nil)
	if n := d.tick(context.Background()); n != 0 {
		t.Fatalf("claim conflict should yield 0 claims, got %d", n)
	}
	d.wg.Wait()
	if processed != 0 {
		t.Errorf("a card we couldn't claim must not be processed, got %d", processed)
	}
	// The semaphore slot must have been released on the failed claim.
	if len(d.sem) != 0 {
		t.Errorf("semaphore slot leaked on failed claim: %d held", len(d.sem))
	}
}

// liftBoardLaunchContext must round-trip the webhook launch context stamped by
// ensureBoardCard: repo + BYOK key/secret overrides come off the reserved keys
// into the LaunchSpec, the bot's own vars are preserved, and the reserved keys
// never leak into those vars. Without this the board-coordinator launch has no
// overrides (the webhook's never reach it).
func TestLiftBoardLaunchContext(t *testing.T) {
	botArgs := map[string]string{
		"feature_prompt":        "add X",
		boardRepoURLKey:         "https://github.com/acme/api.git",
		boardRepoRefKey:         "main",
		boardKeyOverridesKey:    `{"anthropic":"key-1"}`,
		boardSecretOverridesKey: `{"forge_token":"sec-1"}`,
	}
	lc, err := liftBoardLaunchContext(botArgs)
	if err != nil {
		t.Fatalf("liftBoardLaunchContext: %v", err)
	}
	if lc.RepoURL != "https://github.com/acme/api.git" || lc.RepoRef != "main" {
		t.Errorf("repo lifted wrong: %q %q", lc.RepoURL, lc.RepoRef)
	}
	if lc.KeyOverrides["anthropic"] != "key-1" {
		t.Errorf("key overrides = %v", lc.KeyOverrides)
	}
	if lc.SecretOverrides["forge_token"] != "sec-1" {
		t.Errorf("secret overrides = %v", lc.SecretOverrides)
	}
	if lc.Vars["feature_prompt"] != "add X" {
		t.Errorf("bot var dropped: %v", lc.Vars)
	}
	for _, reserved := range []string{boardRepoURLKey, boardRepoRefKey, boardKeyOverridesKey, boardSecretOverridesKey} {
		if _, leaked := lc.Vars[reserved]; leaked {
			t.Errorf("reserved key %q leaked into bot vars", reserved)
		}
	}
	// A malformed override blob must fail the lift: silently dropping it
	// would launch without the webhook's key/secret overrides.
	if _, err := liftBoardLaunchContext(map[string]string{boardKeyOverridesKey: "{not json"}); err == nil {
		t.Errorf("malformed key-override blob should error")
	} else if !strings.Contains(err.Error(), boardKeyOverridesKey) {
		t.Errorf("error should name the offending bot-arg key, got: %v", err)
	}
	if _, err := liftBoardLaunchContext(map[string]string{boardSecretOverridesKey: "{not json"}); err == nil {
		t.Errorf("malformed secret-override blob should error")
	} else if !strings.Contains(err.Error(), boardSecretOverridesKey) {
		t.Errorf("error should name the offending bot-arg key, got: %v", err)
	}
}

// TestProcessBoardCardCarriesPRLaunchContext: a card that targets a pull
// request must reach its run with the repo's launch policy AND a forge-publish
// grant. Neither can ride the card — ensureBoardCard copies a curated subset of
// vars, and a grant expires — so the cloud coordinator resolves both at claim
// time. Without them a board-mode fixer pushes its commits and then has no
// endpoint to post its verdict or its merge-gate status to, leaving the repo's
// required check stale on the head it just created.
//
// Drives the real processBoardCard rather than applyPRLaunchContext alone: a
// correct composition is worthless if the launch path never calls it.
func TestProcessBoardCardCarriesPRLaunchContext(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	// The gate context arrives from the co-enabled bots' manifest union, not
	// from an operator pin — the layer a policy built from the integration
	// alone would drop, resolving the var on every webhook lane and nowhere
	// else.
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "wh1", TenantID: "team1",
		LaunchVars:         map[string]string{"gate_context": "iterion/review", "post_to_board": "false"},
		OperatorLaunchVars: map[string]string{"gate_severity": "high"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "ri1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r",
		WebhookID:  "wh1",
		LaunchVars: map[string]string{"gate_severity": "high"},
	}); err != nil {
		t.Fatal(err)
	}

	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "fixer.bot"), []byte(
		// worktree: none — isolation is the IR default, and here it would fork
		// a worktree off the live checkout and provision its devbox for a
		// fixture that only asserts on launch vars.
		"schema probe_out:\n  ok: string\n\ntool noop:\n  command: `printf '{\"ok\":\"yes\"}'`\n  output: probe_out\n\nworkflow board_probe:\n  worktree: none\n  entry: noop\n  noop -> done\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}

	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s.runs = svc

	// processBoardCard blocks polling the run on a 3s ticker; cancelling
	// releases it (the loop selects on ctx.Done) so the goroutine is joined
	// before the temp store is torn down.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.processBoardCard(ctx, "team1", native.Issue{
			ID: "card1", Bot: "fixer", State: native.StateReady,
			BotArgs: map[string]string{"pr_url": "https://github.com/o/r/pull/7"},
		})
	}()
	t.Cleanup(func() { cancel(); <-done })

	// Wait for the run to SETTLE, not merely to exist: reading run.json the
	// moment it appears leaves the engine's goroutines writing into the temp
	// store while cleanup removes it.
	var settled *store.Run
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if ids, lerr := rs.ListRuns(ctx); lerr == nil && len(ids) > 0 {
			if run, rerr := rs.LoadRun(ctx, ids[0]); rerr == nil && run.Status.IsTerminal() {
				settled = run
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if settled == nil {
		t.Fatal("no run settled from the card")
	}
	inputs := settled.Inputs

	// The run must persist its card as SourceRef (Rf72821): the fork-adoption
	// sweep resolves an issue's runs through the indexed ListRunsBySourceIssue
	// reverse edge, which exists only if the board launch stamps
	// Source.IssueID — the card→run SetLastRun stamp alone leaves the sweep's
	// search empty forever.
	if settled.Source == nil || settled.Source.IssueID != "card1" {
		t.Fatalf("run.Source = %+v, want the dispatched card stamped (IssueID=card1)", settled.Source)
	}
	if settled.Source.Kind != store.RunSourceKindDispatcher {
		t.Errorf("run.Source.Kind = %q, want %q (the cloud board dispatcher IS a dispatcher launch)", settled.Source.Kind, store.RunSourceKindDispatcher)
	}
	if ids, err := rs.ListRunsBySourceIssue(ctx, "card1"); err != nil || len(ids) != 1 || ids[0] != settled.ID {
		t.Errorf("ListRunsBySourceIssue(card1) = %v, %v — the sweep's reverse edge must resolve the board-dispatched run", ids, err)
	}

	if got, _ := inputs["gate_context"].(string); got != "iterion/review" {
		t.Errorf("the repo's gate_context did not reach the run: %q", got)
	}
	if got, _ := inputs["gate_severity"].(string); got != "high" {
		t.Errorf("the operator layer did not reach the run: %q", got)
	}
	if got, _ := inputs[forgePublishVarURL].(string); got != "https://iterion.example/api/v1/forge/publish-review" {
		t.Errorf("publish endpoint not injected: %q", got)
	}
	tok, _ := inputs[forgePublishVarToken].(string)
	if tok == "" {
		t.Fatal("no publish grant minted for the board launch")
	}
	g, ok := s.forgePublishTokens.lookup(tok)
	if !ok || g.TeamID != "team1" || g.ConnectionID != "conn1" || g.Repo != "o/r" {
		t.Fatalf("grant scoped wrong: ok=%v g=%+v", ok, g)
	}
}

// TestApplyPRLaunchContextPrecedence: the repo's policy fills gaps only — a
// value already on the launch is a deliberate per-run pin — and within that
// policy the operator's override beats the manifest union. A launch with no
// pull request gets nothing: neither the grant nor the gate has any meaning.
func TestApplyPRLaunchContextPrecedence(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "wh1", TenantID: "team1",
		// The union carries a co-enabled bot's value for post_to_board; the
		// per-bot rule is what keeps it from reaching the other bot.
		LaunchVars:         map[string]string{"gate_context": "revi/review", "gate_severity": "low", "post_to_board": "false"},
		OperatorLaunchVars: map[string]string{"gate_context": "iterion/review"},
		BotRules: []webhooks.BotRule{
			{BotID: "fixer", LaunchVars: map[string]string{"post_to_board": "true"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		ID: "ri1", TenantID: "team1", ConnectionID: "conn1", RepoFullName: "o/r",
		WebhookID:  "wh1",
		LaunchVars: map[string]string{"gate_context": "iterion/review"},
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{
		"pr_url":        "https://github.com/o/r/pull/7",
		"gate_severity": "medium", // pinned on the launch — must survive
		"gate_context":  "",       // cleared field: absent, not a decision
	}
	out := s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", vars, nil)
	if out["gate_severity"] != "medium" {
		t.Errorf("repo policy overwrote a launch pin: %q", out["gate_severity"])
	}
	if out["gate_context"] != "iterion/review" {
		t.Errorf("a blank value must not suppress the repo's pin: %q", out["gate_context"])
	}
	if out["post_to_board"] != "true" {
		t.Errorf("the bot's own rule must beat the co-enabled union: %q", out["post_to_board"])
	}

	// A repo re-provisioned onto another connection leaves the first row
	// behind — the live shape on the e2e repo, where the stale row is a
	// personal-token connection and the current one a GitHub App. The newer
	// provisioning is the operator's intent, and it decides which identity the
	// verdict is posted under, so it must win regardless of store order.
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn2", TenantID: "team1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(context.Background(), forge.RepoIntegration{
		// Sorts BEFORE ri1 lexicographically, and is older.
		ID: "aa-stale", TenantID: "team1", ConnectionID: "conn2", RepoFullName: "o/r",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer",
		map[string]string{"pr_url": "https://github.com/o/r/pull/7"}, nil)
	if g, ok := s.forgePublishTokens.lookup(out[forgePublishVarToken]); !ok || g.ConnectionID != "conn1" {
		t.Errorf("the stale provisioning won the grant: ok=%v g=%+v", ok, g)
	}
	if out["gate_context"] != "iterion/review" {
		t.Errorf("the stale provisioning won the policy: %q", out["gate_context"])
	}

	// Same slug on another forge is a different repo: no policy, no grant.
	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer",
		map[string]string{"pr_url": "https://gitlab.example/o/r/-/merge_requests/7"}, nil)
	if _, ok := out["gate_context"]; ok {
		t.Errorf("policy applied across forge hosts: %v", out)
	}
	if _, ok := out[forgePublishVarToken]; ok {
		t.Error("grant minted for a repo on a host no connection covers")
	}

	out = s.applyPRLaunchContext(context.Background(), "team1", "", "fixer", map[string]string{"base_ref": "main"}, nil)
	if _, ok := out[forgePublishVarToken]; ok {
		t.Error("no pr_url: nothing to grant")
	}
	if _, ok := out["gate_context"]; ok {
		t.Error("no pr_url: no repo policy to apply")
	}
}

// TestBoardDispatcher_SweepAdoptsFinishedFork: a cloud card stranded on a
// DEAD pointer — in_progress because its dispatcher replica died mid-run and
// nothing ever moved it on, or blocked because the run failed — is never
// touched again: tick only claims Ready cards, and a recovery fork never
// becomes LastRunID on its own. The projection already lets the finished
// fork replace its parent on the card (#377); the sweep must converge the
// TICKET with that view: adopt the issue's newest fork that ACTUALLY
// finished (FinishedAt != nil — a parked shell delivered nothing), then file
// the card done. A card whose own pointer finished cleanly is filed
// outright. Live pointers, pointer-less cards and forkless failures stay
// put. Cloud parity with the local reconcileFinishedTickets (#379).
func TestBoardDispatcher_SweepAdoptsFinishedFork(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	ended := older.Add(50 * time.Minute)
	card := func(id, state, runID string) boardmongo.Candidate {
		return boardmongo.Candidate{Tenant: "t1", Issue: native.Issue{ID: id, State: state, LastRunID: runID}}
	}
	src := func(issueID string) *store.RunSource { return &store.RunSource{IssueID: issueID} }
	f := newFakeBoardCoord(
		card("native:stuck", native.StateInProgress, "run-dead"),
		card("native:blocked", native.StateBlocked, "run-dead2"),
		card("native:live", native.StateInProgress, "run-live"),
		card("native:forkless", native.StateInProgress, "run-dead3"),
		card("native:finished", native.StateInProgress, "run-fin"),
		// blocked + finished pointer: an operator dragged a done card to
		// Blocked to flag a bad outcome — the direct-file branch must not
		// override that placement (R751dc1); only in_progress is the
		// dispatcher's own orphan window.
		card("native:blockedfin", native.StateBlocked, "run-fin2"),
		card("native:noptr", native.StateInProgress, ""),
	)
	statuses := map[string]store.RunStatus{
		"run-dead":  store.RunStatusFailed,
		"run-dead2": store.RunStatusFailed,
		"run-dead3": store.RunStatusFailed,
		"run-live":  store.RunStatusRunning,
		"run-fin":   store.RunStatusFinished,
		"run-fin2":  store.RunStatusFinished,
	}
	pointers := map[string]*store.Run{
		"run-dead":  {ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older},
		"run-dead2": {ID: "run-dead2", Status: store.RunStatusFailed, CreatedAt: older},
		"run-dead3": {ID: "run-dead3", Status: store.RunStatusFailed, CreatedAt: older},
	}
	byIssue := map[string][]*store.Run{
		"native:stuck": {
			{ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older, Source: src("native:stuck")},
			{ID: "run-fork", Status: store.RunStatusFinished, CreatedAt: older.Add(30 * time.Minute),
				FinishedAt: &ended, ForkedFrom: "run-dead", WorkDir: "/wt/fork", Source: src("native:stuck")},
			// Newer but parked: cancelled via Fork()'s initial SaveRun, no
			// FinishedAt — must not win over the fork that really finished.
			{ID: "run-fork-parked", Status: store.RunStatusCancelled, CreatedAt: older.Add(40 * time.Minute),
				ForkedFrom: "run-fork", Source: src("native:stuck")},
		},
		"native:blocked": {
			{ID: "run-dead2", Status: store.RunStatusFailed, CreatedAt: older, Source: src("native:blocked")},
			{ID: "run-fork2", Status: store.RunStatusFinished, CreatedAt: older.Add(20 * time.Minute),
				FinishedAt: &ended, ForkedFrom: "run-dead2", Source: src("native:blocked")},
		},
		"native:forkless": {
			{ID: "run-dead3", Status: store.RunStatusFailed, CreatedAt: older, Source: src("native:forkless")},
		},
	}

	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("the fork-adoption sweep must never dispatch a card")
		return nil
	}, "replica-A", 4, nil)
	d.statusFor = func(_ context.Context, _, runID string) (store.RunStatus, error) {
		return statuses[runID], nil
	}
	d.runFor = func(_ context.Context, _, runID string) (*store.Run, error) {
		r, ok := pointers[runID]
		if !ok {
			return nil, fmt.Errorf("run %s not found", runID)
		}
		return r, nil
	}
	var searches int
	d.issueRuns = func(_ context.Context, _, issueID string) ([]*store.Run, error) {
		searches++
		return byIssue[issueID], nil
	}
	var amu sync.Mutex
	adopted := map[string][2]string{}
	d.adoptRun = func(_, cardID, runID, workdir string) error {
		amu.Lock()
		adopted[cardID] = [2]string{runID, workdir}
		amu.Unlock()
		return nil
	}

	d.sweepForkAdoptions(context.Background())

	for _, id := range []string{"native:stuck", "native:blocked", "native:finished"} {
		if got := f.states[id]; got != native.StateDone {
			t.Errorf("card %s state = %q, want %q", id, got, native.StateDone)
		}
	}
	if got := adopted["native:stuck"]; got != [2]string{"run-fork", "/wt/fork"} {
		t.Errorf("native:stuck adoption = %v, want the finished fork with its workdir", got)
	}
	if got := adopted["native:blocked"]; got != [2]string{"run-fork2", ""} {
		t.Errorf("native:blocked adoption = %v, want run-fork2", got)
	}
	// A card whose own pointer finished is filed without an adoption.
	if _, ok := adopted["native:finished"]; ok {
		t.Errorf("native:finished must be filed outright, not adopted: %v", adopted["native:finished"])
	}
	// Live pointer, missing pointer, forkless failure — and a BLOCKED card
	// whose pointer finished (an operator placement, not a dispatcher
	// orphan; R751dc1) — stay put.
	for _, id := range []string{"native:live", "native:noptr", "native:forkless", "native:blockedfin"} {
		if got, moved := f.states[id]; moved {
			t.Errorf("card %s must stay put, was moved to %q", id, got)
		}
	}
	if searches != 3 {
		t.Errorf("fork searches = %d, want 3 (stuck, blocked, forkless — one per stuck card)", searches)
	}

	// A second sweep within the TTL must not re-search an issue that turned
	// up nothing to adopt — an abandoned failure is the RESTING state of a
	// board, not a reason to pay an indexed query per card per tick.
	d.sweepForkAdoptions(context.Background())
	if searches != 3 {
		t.Errorf("fork searches after a 2nd sweep = %d, want 3 (negative results memoized for the TTL)", searches)
	}

	// Nil wiring (sweep disabled) must be a hard no-op, not a panic.
	d2 := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil }, "replica-A", 4, nil)
	d2.sweepForkAdoptions(context.Background())
}

// TestBoardDispatcher_SweepMemoizesUnreadablePointer: a card whose LastRunID
// no longer resolves (the run was pruned or deleted) fails statusFor on
// every tick — permanently. The memo must be written BEFORE the read, or
// exactly the cards it exists to bound pay one run load per 5s tick forever
// (R08601b).
func TestBoardDispatcher_SweepMemoizesUnreadablePointer(t *testing.T) {
	f := newFakeBoardCoord(boardmongo.Candidate{
		Tenant: "t1",
		Issue:  native.Issue{ID: "native:pruned", State: native.StateBlocked, LastRunID: "run-gone"},
	})
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("the fork-adoption sweep must never dispatch a card")
		return nil
	}, "replica-A", 4, nil)
	var reads int
	d.statusFor = func(context.Context, string, string) (store.RunStatus, error) {
		reads++
		return "", errors.New("run run-gone not found (pruned)")
	}
	d.runFor = func(context.Context, string, string) (*store.Run, error) { return nil, errors.New("gone") }
	d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) { return nil, nil }
	d.adoptRun = func(string, string, string, string) error { return nil }

	d.sweepForkAdoptions(context.Background())
	d.sweepForkAdoptions(context.Background())

	if reads != 1 {
		t.Errorf("statusFor reads across two sweeps = %d, want 1 (an unreadable pointer must be memoized for the TTL, not re-read per tick)", reads)
	}
	if got, moved := f.states["native:pruned"]; moved {
		t.Errorf("unreadable pointer must leave the card in place, was moved to %q", got)
	}
}

// TestBoardDispatcher_SweepSaturationWarnDedups: the listing-cap warning
// fires once per condition EDGE, not per 5s tick (~17k identical lines/day
// otherwise), and re-arms when the board drops back under the cap (R0544a9).
func TestBoardDispatcher_SweepSaturationWarnDedups(t *testing.T) {
	cands := make([]boardmongo.Candidate, sweepCardLimit)
	for i := range cands {
		cands[i] = boardmongo.Candidate{
			Tenant: "t1",
			Issue:  native.Issue{ID: fmt.Sprintf("native:%d", i), State: native.StateBlocked},
		}
	}
	f := newFakeBoardCoord(cands...)
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil }, "replica-A", 4, nil)
	d.statusFor = func(context.Context, string, string) (store.RunStatus, error) { return "", errors.New("x") }
	d.runFor = func(context.Context, string, string) (*store.Run, error) { return nil, errors.New("x") }
	d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) { return nil, nil }
	d.adoptRun = func(string, string, string, string) error { return nil }

	d.sweepForkAdoptions(context.Background())
	if !d.saturationWarned {
		t.Fatal("a full listing window must flag saturation")
	}
	d.sweepForkAdoptions(context.Background())
	if !d.saturationWarned {
		t.Fatal("saturation must stay flagged while the condition holds")
	}

	// Drop under the cap: the flag resets so the next saturation warns again.
	f.mu.Lock()
	f.cands = f.cands[:sweepCardLimit-1]
	f.mu.Unlock()
	d.sweepForkAdoptions(context.Background())
	if d.saturationWarned {
		t.Fatal("dropping under the cap must reset the saturation flag")
	}
}

// TestBoardDispatcher_SweepRevertsAdoptionOnFilingFailure: adoptRun then
// SetState(done) are two writes; if the second fails the card would be
// HALF-adopted — blocked, pointer reading finished — and the
// operator-placement guard would skip it on every later pass, stranding it
// forever with a mutated pointer (R716c91). The sweep must revert the
// pointer to the dead parent so the next TTL retries the adoption as a
// unit.
func TestBoardDispatcher_SweepRevertsAdoptionOnFilingFailure(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	ended := older.Add(30 * time.Minute)
	f := newFakeBoardCoord(boardmongo.Candidate{
		Tenant: "t1",
		Issue:  native.Issue{ID: "native:stuck", State: native.StateBlocked, LastRunID: "run-dead", LastWorkdir: "/wt/dead"},
	})
	f.stateErr["native:stuck"] = errors.New("transition rejected: no done column")
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("the fork-adoption sweep must never dispatch a card")
		return nil
	}, "replica-A", 4, nil)
	d.statusFor = func(context.Context, string, string) (store.RunStatus, error) {
		return store.RunStatusFailed, nil
	}
	d.runFor = func(context.Context, string, string) (*store.Run, error) {
		return &store.Run{ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older}, nil
	}
	d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) {
		return []*store.Run{
			{ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older,
				Source: &store.RunSource{IssueID: "native:stuck"}},
			{ID: "run-fork", Status: store.RunStatusFinished, CreatedAt: older.Add(10 * time.Minute),
				FinishedAt: &ended, ForkedFrom: "run-dead", WorkDir: "/wt/fork",
				Source: &store.RunSource{IssueID: "native:stuck"}},
		}, nil
	}
	var stamps [][2]string
	d.adoptRun = func(_, _, runID, workdir string) error {
		stamps = append(stamps, [2]string{runID, workdir})
		return nil
	}

	d.sweepForkAdoptions(context.Background())

	want := [][2]string{{"run-fork", "/wt/fork"}, {"run-dead", "/wt/dead"}}
	if len(stamps) != 2 || stamps[0] != want[0] || stamps[1] != want[1] {
		t.Errorf("stamps = %v, want adopt-then-revert %v", stamps, want)
	}
	if got, moved := f.states["native:stuck"]; moved {
		t.Errorf("filing failed — the card must not move, was set to %q", got)
	}
}

// TestBoardDispatcher_SweepSkipsFilingOnStampFailure: done is terminal for
// the sweep ({in_progress, blocked} is all it lists), so a card filed done
// while SetLastRun failed would show a Done card pointing at the FAILED
// parent forever — no later tick revisits it. A stamp error must therefore
// skip the SetState, mirroring the local twin's guard
// (pipeline_admission.go). Re9efb2.
func TestBoardDispatcher_SweepSkipsFilingOnStampFailure(t *testing.T) {
	older := time.Now().Add(-time.Hour)
	ended := older.Add(30 * time.Minute)
	f := newFakeBoardCoord(boardmongo.Candidate{
		Tenant: "t1",
		Issue:  native.Issue{ID: "native:stuck", State: native.StateInProgress, LastRunID: "run-dead"},
	})
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		t.Fatal("the fork-adoption sweep must never dispatch a card")
		return nil
	}, "replica-A", 4, nil)
	d.statusFor = func(context.Context, string, string) (store.RunStatus, error) {
		return store.RunStatusFailed, nil
	}
	d.runFor = func(context.Context, string, string) (*store.Run, error) {
		return &store.Run{ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older}, nil
	}
	d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) {
		return []*store.Run{
			{ID: "run-dead", Status: store.RunStatusFailed, CreatedAt: older,
				Source: &store.RunSource{IssueID: "native:stuck"}},
			{ID: "run-fork", Status: store.RunStatusFinished, CreatedAt: older.Add(10 * time.Minute),
				FinishedAt: &ended, ForkedFrom: "run-dead",
				Source: &store.RunSource{IssueID: "native:stuck"}},
		}, nil
	}
	d.adoptRun = func(string, string, string, string) error {
		return errors.New("mongo blip")
	}

	d.sweepForkAdoptions(context.Background())

	if got, moved := f.states["native:stuck"]; moved {
		t.Errorf("stamp failed but the card was filed to %q — it now points at the dead parent and can never self-heal", got)
	}
}

// TestBoardIssueRuns: the fork-adoption sweep reads an issue's runs through
// the indexed card←run reverse edge (ListRunsBySourceIssue), tenant-scoped
// like every other cloud board read — never a full run-store scan.
func TestBoardIssueRuns(t *testing.T) {
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := runview.NewService("", runview.WithStore(rs))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.runs = svc

	ctx := context.Background()
	ended := time.Now()
	for _, r := range []*store.Run{
		{ID: "run-a", WorkflowName: "wf", Status: store.RunStatusFailed, Source: &store.RunSource{IssueID: "native:1"}},
		{ID: "run-b", WorkflowName: "wf", Status: store.RunStatusFinished, FinishedAt: &ended,
			ForkedFrom: "run-a", Source: &store.RunSource{IssueID: "native:1"}},
		{ID: "run-c", WorkflowName: "wf", Status: store.RunStatusFinished, FinishedAt: &ended,
			Source: &store.RunSource{IssueID: "native:2"}},
	} {
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := s.boardIssueRuns(ctx, "t1", "native:1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range runs {
		got[r.ID] = true
	}
	if !got["run-a"] || !got["run-b"] || got["run-c"] || len(runs) != 2 {
		t.Errorf("boardIssueRuns(native:1) = %v, want exactly run-a and run-b", got)
	}

	// boardRun serves the full record (CreatedAt orders fork candidates).
	full, err := s.boardRun(ctx, "t1", "run-b")
	if err != nil || full == nil || full.ForkedFrom != "run-a" {
		t.Errorf("boardRun(run-b) = %+v, %v", full, err)
	}

	// Unwired server: an error, not a panic.
	if _, err := (&Server{}).boardIssueRuns(ctx, "t1", "native:1"); err == nil {
		t.Error("boardIssueRuns without a run service should error")
	}
	if _, err := (&Server{}).boardRun(ctx, "t1", "run-b"); err == nil {
		t.Error("boardRun without a run service should error")
	}
}

// TestAdoptCardRun: the fork-adoption stamp goes through the same
// CloudBoardFor seam as stampCardLastRun but keeps the fork's workdir (the
// run has already executed; LastWorkdir feeds the studio's inspect-the-diff
// link — cf. R26faf1 on the local twin).
func TestAdoptCardRun(t *testing.T) {
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	iss, err := boardStore.Create(native.Issue{Title: "x", State: native.StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return boardStore }

	if err := s.adoptCardRun("t1", iss.ID, "run-fork", "/wt/fork"); err != nil {
		t.Fatalf("adoptCardRun: %v", err)
	}
	got, err := boardStore.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRunID != "run-fork" || got.LastWorkdir != "/wt/fork" {
		t.Fatalf("card pointer = (%q, %q), want (run-fork, /wt/fork)", got.LastRunID, got.LastWorkdir)
	}
	// A failed stamp must SURFACE (the sweep skips the done filing on it) —
	// and so must a missing seam: a wiring that sweeps without the board
	// seam must not file cards it cannot stamp. Only an empty run id is a
	// no-op (nothing to stamp).
	if err := s.adoptCardRun("t1", "native:no-such-card", "run-y", ""); err == nil {
		t.Error("stamping a missing card must return the store error, got nil")
	}
	if err := (&Server{}).adoptCardRun("t1", iss.ID, "run-x", ""); err == nil {
		t.Error("a missing CloudBoardFor seam must surface as an error, got nil")
	}
	if err := (&Server{}).adoptCardRun("t1", iss.ID, "", ""); err != nil {
		t.Errorf("an empty run id is a no-op, got %v", err)
	}
}
