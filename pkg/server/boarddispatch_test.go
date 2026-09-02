package server

import (
	"bytes"
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

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	epochs   map[string]int64
	expired  []boardmongo.ExpiredCandidate
	unleased []boardmongo.ExpiredCandidate
	// unleasedLists counts ListUnleasedClaims calls — the periodicity
	// oracle for the repair sweeps.
	unleasedLists int
}

func newFakeBoardCoord(cands ...boardmongo.Candidate) *fakeBoardCoord {
	return &fakeBoardCoord{cands: cands, claimed: map[string]string{}, states: map[string]string{}, claimErr: map[string]error{}, stateErr: map[string]error{}, renews: map[string]int{}, epochs: map[string]int64{}}
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

// The reaper pair honours the real coordinator's contract too: a stub
// that lists nothing and reclaims unconditionally makes every watchdog
// assertion vacuous. Candidates are seeded by the test; the transfer is
// a CAS on (marker, epoch) that BUMPS the epoch, exactly as the Mongo
// twin's conformance suite pins it.
func (f *fakeBoardCoord) ListExpiredClaimCandidates(_ context.Context, _ time.Time, limit int) ([]boardmongo.ExpiredCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]boardmongo.ExpiredCandidate, 0, len(f.expired))
	for _, e := range f.expired {
		if limit > 0 && len(out) >= limit {
			break
		}
		e.Claim.State = f.states[e.Claim.IssueID]
		out = append(out, e)
	}
	return out, nil
}

// ListAbandonedRecoveryClaims selects by marker prefix like the real
// coordinator — a fake that ignored the prefix would make the sweep's
// whole point (asking for the right population rather than filtering a
// capped batch) untestable.
func (f *fakeBoardCoord) ListAbandonedRecoveryClaims(_ context.Context, markerPrefix string, _ time.Time, limit int) ([]boardmongo.ExpiredCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]boardmongo.ExpiredCandidate, 0, len(f.expired))
	for _, e := range f.expired {
		if limit > 0 && len(out) >= limit {
			break
		}
		if !strings.HasPrefix(e.Claim.Prev.Marker, markerPrefix) {
			continue
		}
		e.Claim.State = f.states[e.Claim.IssueID]
		out = append(out, e)
	}
	return out, nil
}

// ListUnleasedClaims: the fake honours the contract that matters here —
// only claims whose candidate carries no lease-derived evidence, which
// the tests seed explicitly via unleased.
func (f *fakeBoardCoord) ListUnleasedClaims(_ context.Context, _ time.Time, limit int) ([]boardmongo.ExpiredCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unleasedLists++
	out := make([]boardmongo.ExpiredCandidate, 0, len(f.unleased))
	for _, e := range f.unleased {
		if limit > 0 && len(out) >= limit {
			break
		}
		e.Claim.State = f.states[e.Claim.IssueID]
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeBoardCoord) ReclaimExpired(_ context.Context, _, id string, prev tracker.ClaimToken, marker string, _ time.Time) (tracker.ClaimToken, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.claimed[id]; !ok || held != prev.Marker || f.epochs[id] != prev.Epoch {
		return tracker.ClaimToken{}, "", tracker.ErrClaimConflict
	}
	f.claimed[id] = marker
	f.epochs[id]++
	// The state is read BY the transfer, like the real twins: the caller
	// must never decide on the listing's stale copy.
	return tracker.ClaimToken{Marker: marker, Epoch: f.epochs[id]}, f.states[id], nil
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

// TestCloudReaper_ReparkReturnsTheCardToThePool is the cloud half of the
// watchdog's disposition contract, and the reason the fake above had to
// stop stubbing: the local dispatcher's running column is itself
// eligible, so a bare release re-arms the card, but this tick lists only
// d.eligible. A cloud repark that merely releases leaves the card in
// in_progress, unclaimed, and reachable by NO cloud net — sweepParked
// lists awaiting_input, and there is no board retry sweeper. The card is
// stranded exactly where the watchdog was supposed to rescue it.
func TestCloudReaper_ReparkReturnsTheCardToThePool(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-repark"] = "dead-owner"
	f.epochs["c-repark"] = 3
	f.states["c-repark"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-repark", LastRunID: "run-resumable",
			Prev: tracker.ClaimToken{Marker: "dead-owner", Epoch: 3},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFailedResumable}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.states["c-repark"]; got != native.StateReady {
		t.Fatalf("a reparked card must be written back into the eligible pool: state=%q, want %q "+
			"(released in %q it is claimed by nobody and listed by nothing)", got, native.StateReady, got)
	}
	if _, still := f.claimed["c-repark"]; still {
		t.Fatalf("the dead owner's claim must be released after the repark")
	}
}

// TestCloudReaper_HonoursADeliberateStateMove: same contract as the local
// reaper (shared predicate) — an operator who moved the card while its
// owner was dead outranks the watchdog's default filing.
func TestCloudReaper_HonoursADeliberateStateMove(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-moved"] = "dead-owner"
	f.epochs["c-moved"] = 1
	f.states["c-moved"] = native.StateReview // parked by the operator, a non-launch column
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-moved", LastRunID: "run-finished",
			Prev: tracker.ClaimToken{Marker: "dead-owner", Epoch: 1},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFinished}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.states["c-moved"]; got != native.StateReview {
		t.Fatalf("the watchdog overwrote a deliberate state move: card is %q, the operator had put it in %q",
			got, native.StateReview)
	}
	if _, still := f.claimed["c-moved"]; still {
		t.Fatalf("the dead owner's claim must still be freed")
	}
}

// TestCloudReaper_ReparkIsBounded: returning a card to the pool costs a
// FRESH run on this surface — processBoardCard always calls runs.Launch
// and never resumes the recorded run the way the local dispatcher does.
// So an always-failing card would be relaunched once per lease, forever:
// the watchdog turning a stuck card into a spend loop. Past the ceiling
// it must file the card instead.
//
// The ceiling counts LIFETIME runs, so it sits far above healthy traffic —
// see TestCloudReaper_CeilingSparesAHealthyCard for the other side.
func TestCloudReaper_ReparkIsBounded(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-loop"] = "dead-owner"
	f.epochs["c-loop"] = 1
	f.states["c-loop"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-loop", LastRunID: "run-resumable", LifetimeRuns: watchdogRunCeiling,
			Prev: tracker.ClaimToken{Marker: "dead-owner", Epoch: 1},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFailedResumable}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.states["c-loop"]; got != native.StateBlocked {
		t.Fatalf("past the repark ceiling the card must be filed, not relaunched: state=%q, want %q",
			got, native.StateBlocked)
	}
}

// TestCloudReaper_CeilingSparesAHealthyCard: the spend backstop is
// compared against every run a card ever carried — operator re-queues,
// dispatcher retries, fork adoptions. A card worked on a handful of times
// is not a runaway, and filing it as failed on its FIRST repark would be
// the watchdog inventing a failure that never happened.
func TestCloudReaper_CeilingSparesAHealthyCard(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-healthy"] = "dead-owner"
	f.epochs["c-healthy"] = 1
	f.states["c-healthy"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-healthy", LastRunID: "run-4", LifetimeRuns: 3,
			Prev: tracker.ClaimToken{Marker: "dead-owner", Epoch: 1},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFailedResumable}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.states["c-healthy"]; got != native.StateReady {
		t.Fatalf("a card with 3 lifetime runs is healthy: state=%q, want %q — the ceiling counted runs as if they were reparks",
			got, native.StateReady)
	}
}

// TestCloudReaper_ReleaseOnlyIsNeverFiledAsFailed: a release-only card
// failed at nothing — its claimant died before recording a run, or the
// run was removed by the documented retention command. Filing it as
// failed reports a failure that never happened.
func TestCloudReaper_ReleaseOnlyIsNeverFiledAsFailed(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-nofail"] = "dead-owner"
	f.epochs["c-nofail"] = 1
	f.states["c-nofail"] = native.StateReady
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-nofail", LastRunID: "run-pruned", LifetimeRuns: watchdogRunCeiling + 5,
			Prev: tracker.ClaimToken{Marker: "dead-owner", Epoch: 1},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, _ string) (*store.Run, error) {
		return nil, store.ErrRunNotFound // pruned by `iterion runs prune`
	}

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.states["c-nofail"]; got == native.StateBlocked {
		t.Fatalf("a card whose run was pruned failed at nothing — it must not be filed as %q", got)
	}
}

// TestCloudReaper_ParkedCardIsNotHeldForever is the cloud twin of the
// local test of the same name, and it exists because two of the decision
// table's card-context rows could be deleted outright with this suite
// staying green. Cloud is the twin where the defect is permanent: there
// is no boot sweep here, so a card held by a dead pod's claim is held
// until somebody edits the database.
func TestCloudReaper_ParkedCardIsNotHeldForever(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-parked"] = "dead-pod"
	f.epochs["c-parked"] = 3
	f.states["c-parked"] = native.StateReview // parked by the operator
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-parked", LastRunID: "run-resumable",
			Prev: tracker.ClaimToken{Marker: "dead-pod", Epoch: 3},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFailedResumable}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())
	if _, held := f.claimed["c-parked"]; !held {
		t.Fatal("first pass must CONSERVE a card parked out of the pool: releasing lifts the operator's brake")
	}
	if got := f.states["c-parked"]; got != native.StateReview {
		t.Fatalf("the park must be honoured, card is %q", got)
	}
	// The recovery marker now on the claim is the record of that grant.
	f.expired[0].Claim.Prev = tracker.ClaimToken{Marker: f.claimed["c-parked"], Epoch: f.epochs["c-parked"]}

	d.reapExpiredClaims(context.Background(), time.Now())
	if _, held := f.claimed["c-parked"]; held {
		t.Fatalf("conservation must be granted once, not for ever: still claimed by %q — invisible to the "+
			"dispatch poll, and nothing in cloud ever frees it", f.claimed["c-parked"])
	}
	if got := f.states["c-parked"]; got != native.StateReview {
		t.Fatalf("releasing the claim must not move the card out of where the operator put it, got %q", got)
	}
}

// TestCloudReaper_DoesNotStealFromAFreshClaim closes the cloud twin's
// coverage of the anti-double-launch row: deleting that row entirely left
// this suite green, which is how it could be broken twice without notice.
// A card claimed moments ago, sitting in the running column with no run
// stamped, may have a live worker whose stamp simply has not landed —
// taking its claim is stealing.
func TestCloudReaper_DoesNotStealFromAFreshClaim(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-fresh"] = "live-pod"
	f.epochs["c-fresh"] = 1
	f.states["c-fresh"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-fresh", LastRunID: "", // the stamp is best-effort and lands after the launch
			ClaimedAt: time.Now(),
			Prev:      tracker.ClaimToken{Marker: "live-pod", Epoch: 1},
		},
	}}
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, _ string) (*store.Run, error) { return nil, store.ErrRunNotFound }

	d.reapExpiredClaims(context.Background(), time.Now())

	if got := f.claimed["c-fresh"]; got != "live-pod" {
		t.Fatalf("a claim taken moments ago must not be transferred: now held by %q — its worker may be alive "+
			"and about to stamp its run, and taking the claim double-launches the card", got)
	}

	// Past the window the same card IS actionable — the row is a delay,
	// not a permanent exemption.
	f.expired[0].Claim.ClaimedAt = time.Now().Add(-4 * native.ClaimLeaseDuration)
	d.reapExpiredClaims(context.Background(), time.Now())
	if _, held := f.claimed["c-fresh"]; held {
		t.Fatal("past the stamp window the card must be freed: a stamp that never arrived is not a live worker")
	}
}

// TestCloudReaper_BootSweepFreesAbandonedRecoveryClaims: conserving a
// card holds it under `reaper:<host>` for one lease, and only the NEXT
// watchdog pass releases it. Turning the gate off inside that window —
// the documented rollback lever — would otherwise strand the card under
// a marker nothing else in cloud releases, which is the opposite of what
// a rollback is for.
func TestCloudReaper_BootSweepFreesAbandonedRecoveryClaims(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-abandoned"] = dispatcher.ReaperMarker("old-replica")
	f.epochs["c-abandoned"] = 7
	f.states["c-abandoned"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-abandoned",
			Prev:    tracker.ClaimToken{Marker: dispatcher.ReaperMarker("old-replica"), Epoch: 7},
		},
	}}
	// Gate ON: the reaper is running, so the sweep disposes properly —
	// releasing bare would take the repair away from the reaper, whose
	// listing cannot see a cleared claim.
	t.Setenv(dispatcher.ClaimReaperEnvName(), "on")
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, _ string) (*store.Run, error) { return nil, store.ErrRunNotFound }
	// Drive run() itself, not the method: what matters is that STARTUP
	// performs the sweep. Calling the helper directly would pass with the
	// wiring removed — the whole defect being that nothing invoked it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.run(ctx)

	if _, held := f.claimed["c-abandoned"]; held {
		t.Fatalf("a recovery claim nobody came back for must be freed, still held by %q", f.claimed["c-abandoned"])
	}
	// ...and freeing is not enough. A card released in the running column
	// is reachable by NO cloud net (the tick lists only `eligible`), and
	// clearing its claim also hides it from the reaper, which filters on a
	// non-empty claim — so a bare release strands the card permanently and
	// takes the repair away from the one component that could do it.
	if got := f.states["c-abandoned"]; got != native.StateReady {
		t.Fatalf("a card whose watchdog crashed must be returned to the pool, not just unclaimed: state=%q, want %q",
			got, native.StateReady)
	}
}

// TestCloudReaper_BootSweepHonoursTheDisposition: the sweep repairs what
// a crashed watchdog left behind, and "repair" means the card's recorded
// run decides where it goes — exactly as if the watchdog had lived. A
// finished run's card belongs in the completed column; dropping that on
// the floor loses the disposition the run already earned.
func TestCloudReaper_BootSweepHonoursTheDisposition(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-finished"] = dispatcher.ReaperMarker("crashed-replica")
	f.epochs["c-finished"] = 4
	f.states["c-finished"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-finished", LastRunID: "run-done",
			Prev: tracker.ClaimToken{Marker: dispatcher.ReaperMarker("crashed-replica"), Epoch: 4},
		},
	}}
	t.Setenv(dispatcher.ClaimReaperEnvName(), "on")
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFinished}, nil
	}

	d.sweepAbandonedRecoveryClaims(context.Background())

	if got := f.states["c-finished"]; got != native.StateDone {
		t.Fatalf("the run finished, so the card belongs in %q — the sweep dropped its disposition and left it in %q",
			native.StateDone, got)
	}
}

// TestCloudReaper_BootSweepAsksForItsOwnPopulation: a recovery claim is
// stamped with a FRESH lease at the moment of the transfer, so it sorts
// after every ordinary dead owner. Filtering a capped, lease-ordered
// batch for the reaper marker therefore finds none of them on any board
// that has been running a while — a dead capability with a green test.
// The sweep must SELECT its population.
func TestCloudReaper_BootSweepAsksForItsOwnPopulation(t *testing.T) {
	f := newFakeBoardCoord()
	// A pile of ordinary expired claims ahead of the one that matters.
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("c-ordinary-%d", i)
		f.claimed[id] = "some-pod"
		f.epochs[id] = 1
		f.states[id] = native.StateInProgress
		f.expired = append(f.expired, boardmongo.ExpiredCandidate{
			Tenant: "t1",
			Claim: tracker.ExpiredClaim{
				IssueID: id, Prev: tracker.ClaimToken{Marker: "some-pod", Epoch: 1},
			},
		})
	}
	f.claimed["c-abandoned"] = dispatcher.ReaperMarker("crashed-replica")
	f.epochs["c-abandoned"] = 9
	f.states["c-abandoned"] = native.StateInProgress
	f.expired = append(f.expired, boardmongo.ExpiredCandidate{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-abandoned",
			Prev:    tracker.ClaimToken{Marker: dispatcher.ReaperMarker("crashed-replica"), Epoch: 9},
		},
	})

	t.Setenv(dispatcher.ClaimReaperEnvName(), "on")
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, _ string) (*store.Run, error) { return nil, store.ErrRunNotFound }

	d.sweepAbandonedRecoveryClaims(context.Background())

	if _, held := f.claimed["c-abandoned"]; held {
		t.Fatal("the abandoned recovery claim sat behind 120 ordinary ones and was never reached — " +
			"the sweep filtered a capped batch instead of asking for its own population")
	}
}

// TestCloudReaper_GateOffSweepNeverFilesTerminal: ITERION_BOARD_CLAIM_REAPER
// is the lever an operator pulls when they judge the watchdog itself
// wrong. A boot sweep that keeps applying the decision table with the
// gate off makes that lever meaningless — and the dispositions it
// applies are TERMINAL: `done` promotes dependents, after which Reopen
// refuses. The residue may be dropped; the decision may not be taken.
func TestCloudReaper_GateOffSweepNeverFilesTerminal(t *testing.T) {
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	f := newFakeBoardCoord()
	f.claimed["c-gated"] = dispatcher.ReaperMarker("crashed-replica")
	f.epochs["c-gated"] = 2
	f.states["c-gated"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-gated", LastRunID: "run-done",
			Prev: tracker.ClaimToken{Marker: dispatcher.ReaperMarker("crashed-replica"), Epoch: 2},
		},
	}}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFinished}, nil
	}

	d.sweepAbandonedRecoveryClaims(context.Background())

	if got := f.states["c-gated"]; got == native.StateDone || got == native.StateBlocked {
		t.Fatalf("with the watchdog gated off the sweep filed the card TERMINALLY (%q) — the rollback lever "+
			"must stop the decisions, and this one promotes dependents and cannot be reopened", got)
	}
	if _, held := f.claimed["c-gated"]; held {
		t.Fatalf("the residue must still be dropped: card is held by %q", f.claimed["c-gated"])
	}
	// And an ordinary dead owner's claim is the WATCHDOG's business, not
	// this sweep's: the sweep exists to remove what a watchdog left, and
	// with the gate off nothing else may touch a card.
	f.claimed["c-ordinary"] = "some-pod"
	f.epochs["c-ordinary"] = 2
	f.states["c-ordinary"] = native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-ordinary",
			Prev:    tracker.ClaimToken{Marker: "some-pod", Epoch: 2},
		},
	}}
	d.sweepAbandonedRecoveryClaims(context.Background())
	if got := f.claimed["c-ordinary"]; got != "some-pod" {
		t.Fatalf("an ordinary owner's claim is the watchdog's business, not this sweep's: now %q", got)
	}
}

// TestCloudReaper_BootSweepFreesUnleasedClaims: the second population a
// disabled reaper abandons. A mixed-fleet write strips a LIVE claim of
// its lease and its fence; from then on the holder cannot renew, cannot
// write, and cannot even release its own card, and no listing reaches it
// except this sweep. A rolling deploy is what creates that state, so the
// repair cannot sit behind the gate — which ships off during exactly the
// release that introduces the fields.
func TestCloudReaper_BootSweepFreesUnleasedClaims(t *testing.T) {
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	f := newFakeBoardCoord()
	f.claimed["c-stripped"] = "podA-1"
	f.epochs["c-stripped"] = 0 // the fence field went with the lease
	f.states["c-stripped"] = native.StateInProgress
	f.unleased = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-stripped", State: native.StateInProgress,
			Prev: tracker.ClaimToken{Marker: "podA-1", Epoch: 0},
		},
	}}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, _ string) (*store.Run, error) { return nil, store.ErrRunNotFound }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.run(ctx)

	if _, held := f.claimed["c-stripped"]; held {
		t.Fatalf("a claim stripped of its lease by a mixed-fleet write must be freed at startup: still held by %q "+
			"— its holder is fenced out of its own card and cannot release it, and the only other listing that "+
			"reaches it is behind the gate this release ships off", f.claimed["c-stripped"])
	}
	// Released, not decided: the watchdog is off.
	if got := f.states["c-stripped"]; got != native.StateInProgress {
		t.Fatalf("the gated-off sweep must not decide, card moved to %q", got)
	}
}

// TestCloudReaper_UnleasedSweepNeverSkipsLiveness: "no lease" is not "no
// owner". A legacy claim carries no epoch, which ownedFilter admits by
// design — so its holder can still renew, write and release, and is
// merely not heartbeating. Only the run says whether anyone is alive, so
// this sweep must consult it exactly like every other path that takes a
// card from its holder. Releasing on the lease alone steals from a live
// run, and does it with the gate OFF, which is the one thing the gate
// exists to prevent.
func TestCloudReaper_UnleasedSweepNeverSkipsLiveness(t *testing.T) {
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	f := newFakeBoardCoord()
	f.claimed["c-live"] = "podA-1"
	f.epochs["c-live"] = 0
	f.states["c-live"] = native.StateInProgress
	f.unleased = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-live", LastRunID: "run-live", State: native.StateInProgress,
			Prev: tracker.ClaimToken{Marker: "podA-1", Epoch: 0},
		},
	}}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusRunning}, nil
	}

	d.sweepUnleasedClaims(context.Background())

	if got := f.claimed["c-live"]; got != "podA-1" {
		t.Fatalf("the card's run is RUNNING and its claim was taken anyway (now %q) — a missing lease says "+
			"nothing about liveness, and this holder can still write: only the run decides", got)
	}
}

// TestCloudReaper_UnleasedSweepFreesADeadOne is the counter-case, so the
// guard above cannot be satisfied by refusing everything.
func TestCloudReaper_UnleasedSweepFreesADeadOne(t *testing.T) {
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	f := newFakeBoardCoord()
	f.claimed["c-dead"] = "podA-1"
	f.epochs["c-dead"] = 0
	f.states["c-dead"] = native.StateInProgress
	f.unleased = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim: tracker.ExpiredClaim{
			IssueID: "c-dead", LastRunID: "run-dead", State: native.StateInProgress,
			Prev: tracker.ClaimToken{Marker: "podA-1", Epoch: 0},
		},
	}}
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error { return nil },
		"replica-new", 1, iterlog.Nop())
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFailedResumable}, nil
	}

	d.sweepUnleasedClaims(context.Background())

	if _, held := f.claimed["c-dead"]; held {
		t.Fatalf("a card whose run is terminal-resumable and whose claim carries no lease must be freed, still held by %q",
			f.claimed["c-dead"])
	}
	if got := f.states["c-dead"]; got != native.StateInProgress {
		t.Fatalf("the gated-off sweep must not decide, card moved to %q", got)
	}
}

// TestCloudReaper_LatchIsFedOncePerPass: the pass folds every card's run
// read into ONE latch verdict. Feeding the latch per card flapped it on a
// mixed batch — one unreadable run plus one healthy = a false
// failure/recovery warn pair on EVERY pass, the second line announcing a
// recovery the store never made.
func TestCloudReaper_LatchIsFedOncePerPass(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-bad"], f.claimed["c-ok"] = "dead-1", "dead-2"
	f.epochs["c-bad"], f.epochs["c-ok"] = 1, 1
	f.states["c-bad"], f.states["c-ok"] = native.StateInProgress, native.StateInProgress
	f.expired = []boardmongo.ExpiredCandidate{
		{Tenant: "t1", Claim: tracker.ExpiredClaim{IssueID: "c-bad", LastRunID: "run-bad", Prev: tracker.ClaimToken{Marker: "dead-1", Epoch: 1}}},
		{Tenant: "t1", Claim: tracker.ExpiredClaim{IssueID: "c-ok", LastRunID: "run-ok", Prev: tracker.ClaimToken{Marker: "dead-2", Epoch: 1}}},
	}
	var buf bytes.Buffer
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.New(iterlog.LevelWarn, &buf))
	healed := false
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		if id == "run-bad" && !healed {
			return nil, errors.New("decode run run-bad: invalid character")
		}
		return &store.Run{ID: id, Status: store.RunStatusFinished}, nil
	}

	d.reapExpiredClaims(context.Background(), time.Now())
	if cannot, again := strings.Count(buf.String(), "cannot read runs"), strings.Count(buf.String(), "can read runs again"); cannot != 1 || again != 0 {
		t.Fatalf("first pass: %d failure / %d recovery lines — want exactly 1/0 (one verdict per pass, no per-card flap)", cannot, again)
	}
	d.reapExpiredClaims(context.Background(), time.Now())
	if cannot, again := strings.Count(buf.String(), "cannot read runs"), strings.Count(buf.String(), "can read runs again"); cannot != 1 || again != 0 {
		t.Fatalf("second pass repeated the edge: %d failure / %d recovery lines", cannot, again)
	}
	healed = true
	d.reapExpiredClaims(context.Background(), time.Now())
	d.reapExpiredClaims(context.Background(), time.Now())
	if again := strings.Count(buf.String(), "can read runs again"); again != 1 {
		t.Fatalf("recovery announced %d times, want exactly once", again)
	}
}

// TestCloudSweep_GatedArmCountsOnlyDisposedCards: "handled N claim(s)"
// must count cards the sweep actually disposed. The gated arm counted
// every candidate — a card whose run is RUNNING is (correctly) conserved
// by reapOne, and the sweep then announced work it did not do.
func TestCloudSweep_GatedArmCountsOnlyDisposedCards(t *testing.T) {
	f := newFakeBoardCoord()
	f.claimed["c-live"] = "dead-replica"
	f.epochs["c-live"] = 2
	f.states["c-live"] = native.StateInProgress
	f.unleased = []boardmongo.ExpiredCandidate{{
		Tenant: "t1",
		Claim:  tracker.ExpiredClaim{IssueID: "c-live", LastRunID: "run-live", Prev: tracker.ClaimToken{Marker: "dead-replica", Epoch: 2}},
	}}
	t.Setenv(dispatcher.ClaimReaperEnvName(), "on")
	var buf bytes.Buffer
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.New(iterlog.LevelWarn, &buf))
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusRunning}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.run(ctx)

	if _, held := f.claimed["c-live"]; !held {
		t.Fatalf("a card whose run is RUNNING must be conserved, claim was taken")
	}
	if strings.Contains(buf.String(), "handled") {
		t.Fatalf("the gated sweep announced work it did not do: %q", buf.String())
	}
}

// TestBoardDispatcher_RepairSweepsRunOnTheWatchdogCadence: the two repair
// sweeps must run on every watchdog pass, gate OFF included — not only at
// boot. Boot-only meant "never" for the un-leased population: the rolling
// deploy that CREATES it is also a fleet of fresh boots, each younger
// than the 24h horizon, so every boot listed zero candidates while the
// residue accumulated (and the residue is a live holder locked out of
// its own card by an old binary's full-document replace).
func TestBoardDispatcher_RepairSweepsRunOnTheWatchdogCadence(t *testing.T) {
	f := newFakeBoardCoord()
	// Gate OFF is the arm under test: with the reaper disabled these
	// sweeps are the ONLY repair.
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
	d.interval = 2 * time.Millisecond
	d.reapEvery = 0 // every pass — the cadence itself is a prod constant

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	d.run(ctx)

	f.mu.Lock()
	lists := f.unleasedLists
	f.mu.Unlock()
	if lists < 2 {
		t.Fatalf("un-leased sweep ran %d time(s) over several watchdog passes — boot-only again: "+
			"at sub-24h deploy cadence that population is never repaired", lists)
	}
}
