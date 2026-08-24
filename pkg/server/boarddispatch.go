package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// boardCoordinator is the cross-tenant board view the cloud dispatcher needs.
// *boardmongo.Coordinator satisfies it; tests pass a fake.
type boardCoordinator interface {
	ListEligible(ctx context.Context, eligible []string, limit int, newestFirst bool) ([]boardmongo.Candidate, error)
	Claim(ctx context.Context, tenant, id, marker string) error
	SetState(ctx context.Context, tenant, id, state string) error
	Release(ctx context.Context, tenant, id, marker string) error
}

// errCardPaused marks a processBoardCard error whose run parked on a
// human/operator gate (paused_waiting_human / paused_operator) rather than
// failing. processCard routes such a card to the awaiting-input column, not
// blocked.
var errCardPaused = errors.New("board dispatcher: run paused awaiting input")

// boardDispatcher polls the cloud board for eligible cards and runs each via
// the injected process func (launch + poll-to-terminal). Multi-replica-safe
// WITHOUT leader election: the per-card Claim is a CAS, so each card is claimed
// by exactly one replica; the rest skip. In-flight cards are bounded by a
// semaphore shared across ticks, so a slow run never starves polling.
type boardDispatcher struct {
	coord   boardCoordinator
	process func(ctx context.Context, tenant string, iss native.Issue) error
	marker  string

	eligible        []string
	inProgressState string
	doneState       string
	blockedState    string
	awaitingState   string

	// statusFor + clearBadge power the parked-card sweep (sweepParked).
	// statusFor reads a run's persisted status for a tenant; clearBadge
	// clears the card's denormalized awaiting-input hint. Both optional —
	// a nil statusFor disables the sweep.
	statusFor  func(ctx context.Context, tenant, runID string) (store.RunStatus, error)
	clearBadge func(tenant, id string)

	// runFor + issueRuns + adoptRun power the fork-adoption sweep
	// (sweepForkAdoptions). runFor loads a full run record tenant-scoped
	// (the sweep needs CreatedAt to order fork candidates, which statusFor
	// alone can't provide); issueRuns lists the runs sourced from an issue
	// via the indexed reverse edge; adoptRun stamps the adopted fork onto
	// the card via the CloudBoardFor seam — a stamp error must SKIP the
	// filing (done is terminal for the sweep, so a card filed while still
	// pointing at the dead parent would never self-heal). All optional — a
	// nil runFor disables the sweep.
	runFor    func(ctx context.Context, tenant, runID string) (*store.Run, error)
	issueRuns func(ctx context.Context, tenant, issueID string) ([]*store.Run, error)
	adoptRun  func(tenant, cardID, runID, workdir string) error

	// reconcileMemo bounds the fork-adoption sweep's per-card cost: a card
	// the sweep evaluated and left in place is not re-evaluated until
	// forkAdoptionScanTTL has elapsed — in_progress/blocked are resting
	// states that never drain on their own, so an unmemoized per-tick read
	// would cost one run load per abandoned card per tick forever
	// (keyed tenant|issueID).
	reconcileMemoMu sync.Mutex
	reconcileMemo   map[string]time.Time

	// saturationWarned dedups the fork-adoption sweep's listing-cap warning
	// to one line per condition edge (only touched from the run-loop
	// goroutine, which calls the sweeps sequentially).
	saturationWarned bool

	interval time.Duration
	sem      chan struct{}
	logger   *iterlog.Logger
	wg       sync.WaitGroup // tracks in-flight processCard goroutines (for tests + drain)
}

// newBoardDispatcher wires a cloud board dispatcher with sensible defaults.
func newBoardDispatcher(coord boardCoordinator, process func(context.Context, string, native.Issue) error, marker string, concurrency int, logger *iterlog.Logger) *boardDispatcher {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &boardDispatcher{
		coord:           coord,
		process:         process,
		marker:          marker,
		eligible:        []string{native.StateReady},
		inProgressState: native.StateInProgress,
		doneState:       native.StateDone,
		blockedState:    native.StateBlocked,
		awaitingState:   native.StateAwaitingInput,
		interval:        5 * time.Second,
		sem:             make(chan struct{}, concurrency),
		logger:          logger,
	}
}

// tick claims as many eligible cards as there are free slots and dispatches
// each in a detached goroutine. Returns the number it claimed this tick.
func (d *boardDispatcher) tick(ctx context.Context) int {
	cands, err := d.coord.ListEligible(ctx, d.eligible, cap(d.sem)*2, false)
	if err != nil {
		d.warn("list eligible: %v", err)
		return 0
	}
	claimed := 0
	for _, c := range cands {
		select {
		case d.sem <- struct{}{}: // acquired a slot
		default:
			return claimed // no free slots; the rest wait for the next tick
		}
		if err := d.coord.Claim(ctx, c.Tenant, c.Issue.ID, d.marker); err != nil {
			<-d.sem // claim lost (another replica won, or conflict) — release the slot
			continue
		}
		claimed++
		d.wg.Add(1)
		errtrack.Go("server.boardDispatch.processCard", func() { d.processCard(ctx, c) })
	}
	return claimed
}

func (d *boardDispatcher) processCard(ctx context.Context, c boardmongo.Candidate) {
	defer d.wg.Done()
	defer func() { <-d.sem }()
	// Move to in-progress for board visibility (best-effort).
	if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, d.inProgressState); err != nil {
		d.warn("card %s/%s → in_progress: %v", c.Tenant, c.Issue.ID, err)
	}
	runErr := d.process(ctx, c.Tenant, c.Issue)
	final := d.doneState
	if runErr != nil {
		// A pause is not a failure: route the card to the awaiting-input
		// column so the operator answers it there, not to blocked.
		if errors.Is(runErr, errCardPaused) {
			final = d.awaitingState
		} else {
			final = d.blockedState
			d.warn("card %s/%s run failed: %v", c.Tenant, c.Issue.ID, runErr)
		}
	}
	if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, final); err != nil {
		d.warn("card %s/%s → %s: %v", c.Tenant, c.Issue.ID, final, err)
	}
	if err := d.coord.Release(ctx, c.Tenant, c.Issue.ID, d.marker); err != nil {
		d.warn("card %s/%s release: %v", c.Tenant, c.Issue.ID, err)
	}
}

// sweepParked reconciles cards parked in the awaiting-input column whose
// runs have since reached a terminal status. A cloud card parks UNCLAIMED
// (processCard moved it to awaitingState and released), and every resume
// surface — answer-from-board, run console, CLI — completes the run outside
// this dispatcher's poll loop (which returned at the pause), so without the
// sweep the card strands in awaiting_input forever. Mirrors the local
// dispatcher's reconcileParked (pkg/dispatcher/parked.go): finished →
// doneState, hard-failed → blockedState; resumable statuses (paused_*,
// failed_resumable, cancelled) and in-flight ones stay parked. Multi-replica
// safe without claims: a concurrent double-move lands on the same final
// state, and awaiting_input is not in `eligible` so no replica re-dispatches.
func (d *boardDispatcher) sweepParked(ctx context.Context) {
	if d.statusFor == nil {
		return
	}
	cands, err := d.coord.ListEligible(ctx, []string{d.awaitingState}, sweepCardLimit, false)
	if err != nil {
		d.warn("parked sweep list: %v", err)
		return
	}
	for _, c := range cands {
		runID := c.Issue.LastRunID
		if runID == "" {
			continue
		}
		st, err := d.statusFor(ctx, c.Tenant, runID)
		if err != nil {
			continue // best-effort: unreadable run → leave the card alone
		}
		var target string
		switch st {
		case store.RunStatusFinished:
			target = d.doneState
			d.log("card %s/%s resumed out-of-band and finished (run=%s) — moving to %s", c.Tenant, c.Issue.ID, runID, target)
		case store.RunStatusFailed:
			target = d.blockedState
			d.warn("card %s/%s resumed out-of-band and failed hard (run=%s) — moving to %s", c.Tenant, c.Issue.ID, runID, target)
		default:
			continue // still paused / resumable / in flight — genuinely awaiting the operator
		}
		if d.clearBadge != nil {
			d.clearBadge(c.Tenant, c.Issue.ID)
		}
		if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, target); err != nil {
			d.warn("parked sweep move %s/%s → %s: %v", c.Tenant, c.Issue.ID, target, err)
		}
	}
}

// forkAdoptionScanTTL bounds how often the fork-adoption sweep re-evaluates
// a card it left in place: one evaluation per stranded card per TTL instead
// of one per tick — in_progress/blocked are resting states that never drain
// on their own, so an unmemoized per-tick read would cost one run load per
// abandoned card per tick, forever (R642c4d).
const forkAdoptionScanTTL = 30 * time.Second

// sweepCardLimit caps the cross-tenant listing a sweep pass works through
// (shared by sweepParked and sweepForkAdoptions).
const sweepCardLimit = 200

// sweepForkAdoptions reconciles cards stranded on a DEAD pointer: the card
// sits in in_progress or blocked (the run's terminal resting states once its
// poll loop is gone), the pointer run is terminal, and nothing will ever
// touch the card again — a recovery fork never becomes LastRunID on its
// own. When the operator forked the dead run and that fork ACTUALLY
// finished (FinishedAt != nil — a parked shell has delivered nothing), the
// projection already lets the fork replace its parent on the card; this
// sweep converges the TICKET with what the card shows: it adopts the newest
// finished fork as the card's pointer, then files the card done (the tenant
// store's SetState cascades the waiting_deps promotion of satisfied
// dependents). Mirrors the local reconcileFinishedTickets
// (pkg/server/pipeline_admission.go), which is gated to local mode while
// the projection also runs in cloud (#379).
//
// The sweep rides ListEligible, so it only sees UNCLAIMED cards: a card
// being processed right now is claimed (invisible — its live pointer fails
// the terminal check anyway), and a card still claimed by a hard-killed
// replica stays out of reach — boardmongo claims carry no TTL and no
// reaper exists yet, so the replica-death case needs that reaper first
// (R5ceb26, follow-up). The reachable stranded states are the ones
// processCard itself released (blocked after a failure) or an operator
// moved by hand.
//
// Cost: the fork search runs ONLY for stuck cards and rides the indexed
// ListRunsBySourceIssue edge (never a full store scan); every evaluated
// card is then memoized per (tenant, issue) for forkAdoptionScanTTL, so a
// board of abandoned failures pays one status read + at most one indexed
// query per card per TTL — not per tick (R642c4d). Multi-replica safe
// without claims: the fork choice is deterministic, SetLastRun/SetState are
// idempotent for the same values, and neither in_progress nor blocked is in
// `eligible`, so no replica re-dispatches a filed card.
func (d *boardDispatcher) sweepForkAdoptions(ctx context.Context) {
	if d.statusFor == nil || d.runFor == nil || d.issueRuns == nil || d.adoptRun == nil {
		return
	}
	// Newest-updated FIRST: a stranding bumps the card's UpdatedAt, so the
	// freshest strandings always enter the window even on a board saturated
	// past the cap — under oldest-first the forgotten blocked pile (which the
	// sweep leaves in place, so its timestamps never move) would occupy the
	// window permanently and starve exactly the cards this sweep exists to
	// rescue (R0544a9).
	cands, err := d.coord.ListEligible(ctx, []string{d.inProgressState, d.blockedState}, sweepCardLimit, true)
	if err != nil {
		d.warn("fork-adoption sweep list: %v", err)
		return
	}
	// At the cap, cards older than the window's tail are out of reach until
	// operators drain the pile. Say so — once per condition edge, not per
	// 5s tick (the sweeps run on the single run-loop goroutine).
	if saturated := len(cands) == sweepCardLimit; saturated != d.saturationWarned {
		d.saturationWarned = saturated
		if saturated {
			d.warn("fork-adoption sweep at the %d-card listing cap — cards stranded longer than the newest %d are out of reach until the blocked pile drains", sweepCardLimit, sweepCardLimit)
		} else {
			d.log("fork-adoption sweep back under the %d-card listing cap", sweepCardLimit)
		}
	}
	for _, c := range cands {
		d.reconcileDeadPointer(ctx, c)
	}
}

// reconcileDeadPointer files one stranded card: directly when its pointer
// run finished cleanly, or by adopting the issue's newest finished fork
// when the pointer died. Best-effort per card — an unreadable record leaves
// the card for the next tick.
func (d *boardDispatcher) reconcileDeadPointer(ctx context.Context, c boardmongo.Candidate) {
	runID := c.Issue.LastRunID
	if runID == "" {
		return
	}
	if !d.reconcileDue(c.Tenant, c.Issue.ID) {
		return
	}
	// The card is being evaluated: memoize BEFORE the read, not after. A
	// card whose LastRunID no longer resolves (the run was pruned or
	// deleted) fails statusFor on EVERY tick — permanently, not
	// transiently — so memoizing only on success would leave exactly the
	// cards the memo exists for paying one run load per tick, forever
	// (R08601b). A genuinely transient blip just waits out one TTL.
	d.noteReconcile(c.Tenant, c.Issue.ID)
	st, err := d.statusFor(ctx, c.Tenant, runID)
	if err != nil {
		return // unreadable pointer — re-evaluated after the TTL
	}
	if st == store.RunStatusFinished {
		// Only in_progress is the dispatcher's own orphan window (the run
		// finished, the state move never landed). A blocked card is an
		// operator-facing "bad outcome" flag — re-filing it done would
		// override a deliberate placement within one tick (R751dc1).
		if c.Issue.State != d.inProgressState {
			return
		}
		d.log("card %s/%s pointer run %s finished but the card was never filed — moving to %s", c.Tenant, c.Issue.ID, runID, d.doneState)
		if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, d.doneState); err != nil {
			d.warn("fork-adoption move %s/%s → %s: %v", c.Tenant, c.Issue.ID, d.doneState, err)
		}
		return
	}
	if !st.IsTerminal() {
		return // live pointer — processCard (or a resume) still owns the card
	}
	pointer, err := d.runFor(ctx, c.Tenant, runID)
	if err != nil || pointer == nil {
		return
	}
	runs, err := d.issueRuns(ctx, c.Tenant, c.Issue.ID)
	if err != nil {
		d.warn("fork-adoption sweep: list runs of card %s/%s: %v", c.Tenant, c.Issue.ID, err)
		return
	}
	forks := make([]*store.Run, 0, len(runs))
	for _, r := range runs {
		if r == nil || r.ForkedFrom == "" || r.Source == nil || r.Source.IssueID == "" {
			continue
		}
		if r.Status != store.RunStatusFinished || r.FinishedAt == nil {
			continue
		}
		forks = append(forks, r)
	}
	fork := newestFinishedIssueFork(forks, pointer)
	if fork == nil {
		return
	}
	// Adopt the fork as the current attempt so the pointer converges with
	// what the card already shows — workdir included, unlike launch-time
	// stamps: the fork has already executed, and LastWorkdir feeds the
	// studio's inspect-the-diff link. A stamp failure must SKIP the filing
	// (mirror pipeline_admission.go's guard): done is terminal for this
	// sweep, so a card filed while still pointing at the dead parent would
	// never self-heal.
	if err := d.adoptRun(c.Tenant, c.Issue.ID, fork.ID, fork.WorkDir); err != nil {
		d.warn("fork-adoption stamp %s on card %s/%s: %v", fork.ID, c.Tenant, c.Issue.ID, err)
		return
	}
	d.log("card %s/%s adopted finished fork %s over dead run %s — moving to %s", c.Tenant, c.Issue.ID, fork.ID, runID, d.doneState)
	if err := d.coord.SetState(ctx, c.Tenant, c.Issue.ID, d.doneState); err != nil {
		d.warn("fork-adoption move %s/%s → %s: %v", c.Tenant, c.Issue.ID, d.doneState, err)
	}
}

// reconcileDue reports whether the sweep's evaluation of a (tenant, issue)
// card is due again — false while a previous evaluation is younger than
// forkAdoptionScanTTL.
func (d *boardDispatcher) reconcileDue(tenant, issueID string) bool {
	d.reconcileMemoMu.Lock()
	defer d.reconcileMemoMu.Unlock()
	last, ok := d.reconcileMemo[tenant+"|"+issueID]
	return !ok || time.Since(last) >= forkAdoptionScanTTL
}

// noteReconcile records that a (tenant, issue) card was evaluated, and
// prunes expired entries so the memo can't grow past the set of
// recently-stuck cards.
func (d *boardDispatcher) noteReconcile(tenant, issueID string) {
	d.reconcileMemoMu.Lock()
	defer d.reconcileMemoMu.Unlock()
	if d.reconcileMemo == nil {
		d.reconcileMemo = map[string]time.Time{}
	}
	now := time.Now()
	for k, at := range d.reconcileMemo {
		if now.Sub(at) >= forkAdoptionScanTTL {
			delete(d.reconcileMemo, k)
		}
	}
	d.reconcileMemo[tenant+"|"+issueID] = now
}

// run loops tick every interval until ctx is cancelled, then drains in-flight
// cards. Start one per replica.
func (d *boardDispatcher) run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		d.tick(ctx)
		d.sweepParked(ctx)
		d.sweepForkAdoptions(ctx)
		select {
		case <-ctx.Done():
			d.wg.Wait() // let in-flight cards finish their state transition
			return
		case <-t.C:
		}
	}
}

func (d *boardDispatcher) warn(format string, args ...any) {
	if d.logger != nil {
		d.logger.Warn("board dispatcher: "+format, args...)
	}
}

func (d *boardDispatcher) log(format string, args ...any) {
	if d.logger != nil {
		d.logger.Info("board dispatcher: "+format, args...)
	}
}

// processBoardCard is the cloud board dispatcher's process func: launch the
// card's bot for its tenant through the run service (→ publisher), then poll
// the run record until it terminates. Returns nil on a clean finish, an error
// on failure or pause (the dispatcher then moves the card to blocked). The
// tenant identity is stamped on ctx so the publisher seals credentials.
// Reserved BotArgs keys carrying the launch context a webhook-launched card
// targets — the repo to clone AND the webhook's BYOK key / secret overrides.
// The board coordinator launches from the card and otherwise has NONE of this
// (the webhook's own SecretOverrides/KeyOverrides never reach it). ensureBoardCard
// stamps them; liftBoardLaunchContext extracts them into the LaunchSpec so they
// don't leak to the bot as vars. Secret resolution ALSO works via a (tenant,bot)
// binding — the override is the belt to that braces, and the only route for a
// per-webhook KeyOverride (BYOK billing), which has no binding equivalent.
const (
	boardRepoURLKey         = "__iterion_repo_url"
	boardRepoRefKey         = "__iterion_repo_ref"
	boardKeyOverridesKey    = "__iterion_key_overrides"
	boardSecretOverridesKey = "__iterion_secret_overrides"
)

// boardLaunchContext is the non-var launch state lifted off a card's BotArgs.
type boardLaunchContext struct {
	Vars            map[string]string
	RepoURL         string
	RepoRef         string
	KeyOverrides    map[string]string
	SecretOverrides map[string]string
}

// liftBoardLaunchContext splits a card's BotArgs into the bot's vars and the
// reserved launch context (repo + overrides), removing the reserved keys from
// the vars so they never leak to the bot. A malformed override blob is an
// error: silently dropping it would launch the run WITHOUT the webhook's
// BYOK key / secret overrides, so the failure must surface instead.
func liftBoardLaunchContext(botArgs map[string]string) (boardLaunchContext, error) {
	lc := boardLaunchContext{
		RepoURL: botArgs[boardRepoURLKey],
		RepoRef: botArgs[boardRepoRefKey],
	}
	if blob := botArgs[boardKeyOverridesKey]; blob != "" {
		if err := json.Unmarshal([]byte(blob), &lc.KeyOverrides); err != nil {
			return boardLaunchContext{}, fmt.Errorf("malformed %s bot-arg (edit the card's bot args to fix): %w", boardKeyOverridesKey, err)
		}
	}
	if blob := botArgs[boardSecretOverridesKey]; blob != "" {
		if err := json.Unmarshal([]byte(blob), &lc.SecretOverrides); err != nil {
			return boardLaunchContext{}, fmt.Errorf("malformed %s bot-arg (edit the card's bot args to fix): %w", boardSecretOverridesKey, err)
		}
	}
	lc.Vars = make(map[string]string, len(botArgs))
	for k, v := range botArgs {
		switch k {
		case boardRepoURLKey, boardRepoRefKey, boardKeyOverridesKey, boardSecretOverridesKey:
			continue
		}
		lc.Vars[k] = v
	}
	return lc, nil
}

// stampCardLastRun records the launched run on the tenant's board card via the
// same SetLastRun seam the local dispatcher uses, resolved through CloudBoardFor
// (the Mongo-backed store in cloud, a native store in tests). Best-effort: a
// stamp failure never fails the run — the card simply lacks its live-run link.
func (s *Server) stampCardLastRun(tenant, cardID, runID string) {
	if err := s.adoptCardRun(tenant, cardID, runID, ""); err != nil && s.logger != nil {
		s.logger.Warn("board dispatcher: stamp run %s on card %s/%s: %v", runID, tenant, cardID, err)
	}
}

// adoptCardRun stamps a run onto the tenant's board card (SetLastRun, workdir
// included) via the CloudBoardFor seam. The fork-adoption sweep uses it to
// converge a stranded card's pointer with the finished fork the projection
// already shows — and the returned error is what lets the sweep SKIP the done
// filing when the stamp did not land (Re9efb2). A missing seam or empty run
// id stays a silent no-op: there is no board to converge with.
func (s *Server) adoptCardRun(tenant, cardID, runID, workdir string) error {
	if s.cfg.CloudBoardFor == nil || runID == "" {
		return nil
	}
	store := s.cfg.CloudBoardFor(tenant)
	if store == nil {
		return nil
	}
	return store.SetLastRun(cardID, runID, workdir)
}

// setCardAwaitingInput denormalizes the pause hint onto the tenant's board
// card via the same CloudBoardFor seam as stampCardLastRun, so the studio
// grid can badge a paused card without an N+1 run fetch. Best-effort: a
// write failure never fails the run. It is a HINT — the modal's answer
// affordance still keys off getRun(last_run_id).status.
func (s *Server) setCardAwaitingInput(tenant, cardID string, v bool) {
	if s.cfg.CloudBoardFor == nil {
		return
	}
	store := s.cfg.CloudBoardFor(tenant)
	if store == nil {
		return
	}
	if err := store.SetAwaitingInput(cardID, v); err != nil && s.logger != nil {
		s.logger.Warn("board dispatcher: set awaiting-input=%v on card %s/%s: %v", v, tenant, cardID, err)
	}
}

// boardRunStatus reads a run's persisted status for the parked-card sweep,
// tenant-scoped exactly like processBoardCard's launch context.
func (s *Server) boardRunStatus(ctx context.Context, tenant, runID string) (store.RunStatus, error) {
	if s.runs == nil {
		return "", errors.New("run service unavailable")
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	run, err := s.runs.LoadRunCtx(ctx, runID)
	if err != nil {
		return "", err
	}
	return run.Status, nil
}

// boardRun reads a run's full record for the fork-adoption sweep (the sweep
// needs CreatedAt to order fork candidates, which boardRunStatus alone can't
// provide), tenant-scoped exactly like boardRunStatus.
func (s *Server) boardRun(ctx context.Context, tenant, runID string) (*store.Run, error) {
	if s.runs == nil {
		return nil, errors.New("run service unavailable")
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	return s.runs.LoadRunCtx(ctx, runID)
}

// boardIssueRuns lists the runs sourced from an issue for the fork-adoption
// sweep, tenant-scoped, via the indexed card←run reverse edge
// (ListRunsBySourceIssue) — never a full run-store scan.
func (s *Server) boardIssueRuns(ctx context.Context, tenant, issueID string) ([]*store.Run, error) {
	if s.runs == nil {
		return nil, errors.New("run service unavailable")
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return nil, errors.New("run store unavailable")
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	ids, err := rs.ListRunsBySourceIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	runs := make([]*store.Run, 0, len(ids))
	for _, id := range ids {
		r, err := s.runs.LoadRunCtx(ctx, id)
		if err != nil || r == nil {
			continue // best-effort: an unreadable run simply can't be adopted
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func (s *Server) processBoardCard(ctx context.Context, tenant string, iss native.Issue) error {
	if s.runs == nil {
		return errors.New("run service unavailable")
	}
	if iss.Bot == "" {
		return fmt.Errorf("card %s has no bot", iss.ID)
	}
	ctx = store.WithIdentity(ctx, tenant, "board-dispatcher")
	path, source, err := s.resolveBotSource(iss.Bot)
	if err != nil {
		return err
	}
	// A webhook-launched card carries its launch context (repo + the webhook's
	// BYOK key / secret overrides) in reserved BotArgs keys (ensureBoardCard) —
	// the coordinator otherwise has none of it. Lift it into the LaunchSpec so
	// the runner clones + the publisher applies the overrides, and strip the
	// reserved keys from the bot's vars.
	lc, err := liftBoardLaunchContext(iss.BotArgs)
	if err != nil {
		return fmt.Errorf("card %s: %w", iss.ID, err)
	}
	// A card that targets a pull request also needs the repo's launch policy
	// and a publish grant, neither of which can ride the card itself.
	lc.Vars = s.applyPRLaunchContext(ctx, tenant, "", iss.Bot, lc.Vars, nil)
	res, err := s.runs.Launch(ctx, runview.LaunchSpec{
		FilePath:        path,
		Source:          source,
		BotID:           iss.Bot,
		Vars:            lc.Vars,
		RepoURL:         lc.RepoURL,
		RepoRef:         lc.RepoRef,
		KeyOverrides:    lc.KeyOverrides,
		SecretOverrides: lc.SecretOverrides,
		// Stamp the card onto the run record (ADR-046 SourceRef) — the
		// card→run edge SetLastRun writes below is not enough: the
		// fork-adoption sweep resolves an issue's runs through the indexed
		// ListRunsBySourceIssue REVERSE edge, which only exists when every
		// board-dispatched run persists Source.IssueID (Rf72821 — without
		// it the sweep's fork search comes back empty forever in cloud).
		// Kind mirrors the local dispatcher's engine_runner stamp.
		SourceRef: &store.RunSource{
			Kind:       store.RunSourceKindDispatcher,
			IssueID:    iss.ID,
			IssueTitle: iss.Title,
		},
	})
	if err != nil {
		return err
	}
	runID := res.RunID
	// Stamp the launched run onto the card immediately (not after the run
	// terminates) so the studio can link the LIVE run while it executes. The
	// local dispatcher already does this via SetLastRun; the cloud coordinator
	// launches through runs.Launch and must stamp the same seam itself.
	s.stampCardLastRun(tenant, iss.ID, runID)
	// A fresh dispatch supersedes any prior pause — clear the denormalized
	// awaiting-input badge so the card doesn't show a stale ⏸.
	s.setCardAwaitingInput(tenant, iss.ID, false)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if run, lerr := s.runs.LoadRunCtx(ctx, runID); lerr == nil {
			switch st := run.Status; {
			case st == store.RunStatusFinished:
				return nil
			case st.IsTerminal():
				return fmt.Errorf("run %s ended %s", runID, st)
			case st.IsPaused():
				// Parked on a human/operator gate — stop waiting; the operator
				// resumes the run. Denormalize the pause hint so the grid can
				// badge the card without a per-run fetch, and wrap errCardPaused
				// so processCard routes the card to the awaiting-input column
				// instead of blocked (a pause is not a failure).
				s.setCardAwaitingInput(tenant, iss.ID, true)
				return fmt.Errorf("run %s paused (%s): %w", runID, st, errCardPaused)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
