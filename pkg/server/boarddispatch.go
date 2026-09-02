package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// boardCoordinator is the cross-tenant board view the cloud dispatcher needs.
// *boardmongo.Coordinator satisfies it; tests pass a fake.
type boardCoordinator interface {
	ListEligible(ctx context.Context, eligible []string, limit int, newestFirst bool) ([]boardmongo.Candidate, error)
	Claim(ctx context.Context, tenant, id, marker string) (tracker.ClaimToken, error)
	SetState(ctx context.Context, tenant, id, state string) error
	Release(ctx context.Context, tenant, id, marker string) error
	// The fenced family: RenewClaim is the heartbeat processCard runs for
	// as long as it holds the card (its poll-to-terminal has NO upper
	// bound, so "the claim window is short" is false by construction);
	// the Owned writes are CAS on the token so a superseded replica's
	// late writes are refused, never landed.
	RenewClaim(ctx context.Context, tenant, id string, tok tracker.ClaimToken) error
	SetStateOwned(ctx context.Context, tenant, id, state string, tok tracker.ClaimToken) error
	ReleaseOwned(ctx context.Context, tenant, id string, tok tracker.ClaimToken) error
	// The reaper pair (the cloud half of the claim watchdog — the hole
	// boarddispatch itself documented as R5ceb26: "no TTL and no reaper
	// exists yet"). List is cross-tenant; Reclaim is a CAS TRANSFER.
	ListExpiredClaimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]boardmongo.ExpiredCandidate, error)
	ListAbandonedRecoveryClaims(ctx context.Context, markerPrefix string, cutoff time.Time, limit int) ([]boardmongo.ExpiredCandidate, error)
	ListUnleasedClaims(ctx context.Context, cutoff time.Time, limit int) ([]boardmongo.ExpiredCandidate, error)
	ReclaimExpired(ctx context.Context, tenant, id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, string, error)
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
	// clockFallbackReason latches WHICH degradation was last reported, so
	// the notice stays edge-triggered (a per-pass warn is a log storm at
	// the watchdog's cadence) without a change of cause going unsaid —
	// an operator reading a stale reason is looking at the wrong problem.
	clockFallbackReason string
	// runReadFailure latches whether the run store is currently failing,
	// so the abstention it causes is reported on its edges rather than
	// once per candidate per pass.
	runReadFailure bool
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
		tok, err := d.coord.Claim(ctx, c.Tenant, c.Issue.ID, d.marker)
		if err != nil {
			<-d.sem // claim lost (another replica won, or conflict) — release the slot
			continue
		}
		claimed++
		d.wg.Add(1)
		errtrack.Go("server.boardDispatch.processCard", func() { d.processCard(ctx, c, tok) })
	}
	return claimed
}

func (d *boardDispatcher) processCard(ctx context.Context, c boardmongo.Candidate, tok tracker.ClaimToken) {
	defer d.wg.Done()
	defer func() { <-d.sem }()

	// Heartbeat for the WHOLE hold: the poll-to-terminal below has no
	// upper bound (a long run is normal), so without renewal every card
	// held longer than one lease would read as reapable. Reuse the SAME
	// claimSession primitive the local dispatcher uses (cadence, conflict
	// vs transient semantics, "two missed beats of slack" invariant — all
	// defined once) through a tenant-binding adapter; onLost cancels the
	// poll, since the fenced writes below are refused already and the
	// cancel just stops this replica working a card it no longer owns.
	cardCtx, cancelCard := context.WithCancel(ctx)
	sess := dispatcher.StartClaimSession(
		coordLeaser{coord: d.coord, tenant: c.Tenant},
		c.Issue.ID, tok, d.warn, func(error) { cancelCard() })
	defer func() { cancelCard(); sess.Stop() }()

	// Move to in-progress for board visibility (best-effort, fenced).
	if err := d.coord.SetStateOwned(cardCtx, c.Tenant, c.Issue.ID, d.inProgressState, tok); err != nil {
		d.warn("card %s/%s → in_progress: %v", c.Tenant, c.Issue.ID, err)
	}
	runErr := d.process(cardCtx, c.Tenant, c.Issue)
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
	// Final writes on the PARENT ctx: a superseded claim cancelled
	// cardCtx, and these fenced writes must still run to be REFUSED
	// loudly (typed conflict in the log) rather than die on a dead ctx
	// reading as a store outage.
	if err := d.coord.SetStateOwned(ctx, c.Tenant, c.Issue.ID, final, tok); err != nil {
		d.warn("card %s/%s → %s: %v", c.Tenant, c.Issue.ID, final, err)
	}
	if err := d.coord.ReleaseOwned(ctx, c.Tenant, c.Issue.ID, tok); err != nil {
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
		d.warn("fork-adoption move %s/%s → %s: %v — reverting the pointer so the adoption retries whole", c.Tenant, c.Issue.ID, d.doneState, err)
		// Half-adopted is the one shape nothing revisits (R716c91): a
		// blocked card whose pointer now reads finished is skipped by the
		// operator-placement guard above, so the filing would never
		// complete. Put the dead parent back (best-effort) so the next TTL
		// retries the adoption as a UNIT — including the deterministic
		// SetState failure of a custom board with no done column, which
		// stays a per-TTL warn instead of a silent permanent stranding.
		if rerr := d.adoptRun(c.Tenant, c.Issue.ID, runID, c.Issue.LastWorkdir); rerr != nil {
			d.warn("fork-adoption revert %s/%s to %s: %v — card left half-adopted; an operator state move re-arms the sweep", c.Tenant, c.Issue.ID, runID, rerr)
		}
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
	reaperOn := dispatcher.ClaimReaperEnabled()
	if reaperOn {
		d.warn("claim watchdog active (%s=on): expired leases are reclaimed and routed by the decision table", dispatcher.ClaimReaperEnvName())
	}
	// The watchdog is paced by its OWN interval, not by the dispatch tick
	// it happens to share a loop with: a lease measured in minutes has no
	// use for a five-second sweep, and the difference is a cross-tenant
	// query plus a server-clock round trip per replica, twelve times over.
	// Independently of the gate: free recovery claims nobody came back
	// for. The watchdog holds a card under `reaper:<host>` for one lease
	// when it conserves it, and only the NEXT pass releases it — so
	// turning the gate off in that window (the documented rollback lever)
	// would strand the card under a marker nothing else in cloud releases.
	// The local twin gets this from its boot sweep's pid probe; cloud has
	// no such sweep, so it needs its own.
	d.sweepAbandonedRecoveryClaims(ctx)
	d.sweepUnleasedClaims(ctx)
	var lastReap time.Time
	for {
		d.tick(ctx)
		d.sweepParked(ctx)
		d.sweepForkAdoptions(ctx)
		if reaperOn && time.Since(lastReap) >= dispatcher.ClaimReaperInterval() {
			lastReap = time.Now()
			d.reapExpiredClaims(ctx, d.reapCutoff(ctx))
		}
		select {
		case <-ctx.Done():
			d.wg.Wait() // let in-flight cards finish their state transition
			return
		case <-t.C:
		}
	}
}

// sweepAbandonedRecoveryClaims releases expired claims still held under a
// watchdog's own recovery marker. Runs ONCE at startup and never behind
// the reaper gate: its whole purpose is to repair the state a disabled
// reaper would otherwise leave behind for ever.
func (d *boardDispatcher) sweepAbandonedRecoveryClaims(ctx context.Context) {
	// ONE clock for the listing and for the CAS beneath it: the lease is
	// stamped server-side, so measuring it with this pod's clock is the
	// hole reapCutoff exists to close.
	now := d.reapCutoff(ctx)
	cands, err := d.coord.ListAbandonedRecoveryClaims(ctx, dispatcher.ReaperMarkerPrefix(), now, sweepBatch)
	if err != nil {
		d.warn("recovery-claim sweep: %v", err)
		return
	}
	if len(cands) == sweepBatch {
		// A full batch means there are probably more, and NOTHING else can
		// reach them: a recovery claim carries a fresh lease, so it sorts
		// behind every ordinary one in the reaper's own listing. Say so —
		// a silent cap reads as "all handled".
		d.warn("recovery-claim sweep: batch of %d was full — more abandoned claims remain and only another sweep reaches them",
			sweepBatch)
	}
	d.sweepClaims(ctx, "recovery-claim sweep", cands, now)
}

// sweepUnleasedClaims is the OTHER half of "what a disabled reaper leaves
// behind": ordinary claims a mixed-fleet write stripped of their lease.
// Nothing else lists them, and a rolling deploy is what creates them, so
// the repair cannot sit behind a gate this release ships off.
func (d *boardDispatcher) sweepUnleasedClaims(ctx context.Context) {
	now := d.reapCutoff(ctx)
	cands, err := d.coord.ListUnleasedClaims(ctx, now, sweepBatch)
	if err != nil {
		d.warn("un-leased claim sweep: %v", err)
		return
	}
	d.sweepClaims(ctx, "un-leased claim sweep", cands, now)
}

// sweepClaims is the ONE body both startup sweeps run. Two rules hold
// whatever the gate says:
//
//   - LIVENESS IS NEVER SKIPPED. Every path that takes a card from its
//     holder consults DecideTransfer first, and these are no exception:
//     "no lease" is not "no owner". A legacy claim carries no epoch, which
//     ownedFilter deliberately admits — so its holder can still renew,
//     write and release, and is merely not heartbeating. Only the run says
//     whether anyone is alive.
//   - THE GATE STOPS DECISIONS, NOT REPAIRS. With it on, dispose properly
//     (reapOne re-judges and files); with it off, drop the residue and
//     nothing more, leaving the card as it was before a watchdog touched
//     it.
func (d *boardDispatcher) sweepClaims(ctx context.Context, label string, cands []boardmongo.ExpiredCandidate, now time.Time) {
	if len(cands) == 0 {
		return
	}
	gated := dispatcher.ClaimReaperEnabled()
	swept := 0
	for _, cand := range cands {
		if gated {
			d.reapOne(ctx, cand, now)
			swept++
			continue
		}
		run, runErr := d.loadRunForCard(ctx, cand)
		card := dispatcher.StuckCard{
			State: cand.Claim.State, RunningState: d.inProgressState, LaunchStates: d.eligible,
			StampWindowOpen: dispatcher.StampWindowOpen(cand.Claim.ClaimedAt, now),
		}
		if pre := dispatcher.DecideTransfer(run, runErr, card); pre.Action == dispatcher.StuckKeep {
			d.warn("%s leaves %s/%s alone: %s", label, cand.Tenant, cand.Claim.IssueID, pre.Reason)
			continue
		}
		if d.releaseSweptClaim(ctx, label, cand, now) {
			swept++
		}
	}
	if swept > 0 {
		d.warn("%s: handled %d claim(s) (%s=%s)", label, swept,
			dispatcher.ClaimReaperEnvName(), map[bool]string{true: "on", false: "off"}[gated])
	}
}

// loadRunForCard resolves a candidate's run the way reapOne does, so the
// two never judge the same card on different evidence.
func (d *boardDispatcher) loadRunForCard(ctx context.Context, cand boardmongo.ExpiredCandidate) (*store.Run, error) {
	if cand.Claim.LastRunID == "" {
		return nil, nil
	}
	if d.runFor == nil {
		d.noteRunReadFailure(cand.Tenant, errNoRunLoader)
		return nil, errNoRunLoader
	}
	run, err := d.runFor(ctx, cand.Tenant, cand.Claim.LastRunID)
	if err != nil && errors.Is(err, store.ErrRunNotFound) {
		run, err = nil, nil // a pruned run proves nothing is alive
	}
	d.noteRunReadFailure(cand.Tenant, err)
	return run, err
}

// releaseSweptClaim drops a claim without judging the card — the
// gate-off behaviour. It still transfers first: releasing needs
// ownership, and the CAS is what elects one winner between replicas.
func (d *boardDispatcher) releaseSweptClaim(ctx context.Context, label string, cand boardmongo.ExpiredCandidate, now time.Time) bool {
	tok, _, err := d.coord.ReclaimExpired(ctx, cand.Tenant, cand.Claim.IssueID,
		cand.Claim.Prev, dispatcher.ReaperMarker(d.marker), now)
	if err != nil {
		if !errors.Is(err, tracker.ErrClaimConflict) {
			d.warn("%s reclaim %s/%s: %v", label, cand.Tenant, cand.Claim.IssueID, err)
		}
		return false
	}
	if err := d.coord.ReleaseOwned(ctx, cand.Tenant, cand.Claim.IssueID, tok); err != nil {
		// Retry once: the transfer already replaced the original marker
		// with ours, so giving up here leaves the card under a RECOVERY
		// claim that only a boot sweep releases — strictly worse than the
		// stranded claim we came to repair.
		if err2 := d.coord.ReleaseOwned(ctx, cand.Tenant, cand.Claim.IssueID, tok); err2 != nil {
			d.warn("%s could not release %s/%s (was held by %q, now under %q): %v",
				label, cand.Tenant, cand.Claim.IssueID, cand.Claim.Prev.Marker,
				dispatcher.ReaperMarker(d.marker), err2)
			return false
		}
	}
	d.warn("%s freed %s/%s (was held under %q); the card keeps state %q — with the watchdog gated off, nothing decides for it",
		label, cand.Tenant, cand.Claim.IssueID, cand.Claim.Prev.Marker, cand.Claim.State)
	return true
}

// errNoRunLoader marks the wiring gap that makes the watchdog unable to
// judge anything — reported through the same edge-triggered latch as a
// failing store, since the consequence for every card is identical.
var errNoRunLoader = errors.New("no run loader wired")

// noteRunReadFailure reports the run store's health on its EDGES: the
// transition into failure (every card conserved from here) and back out
// of it. A per-card line would bury both — the batch is a hundred cards,
// once a minute, per replica.
func (d *boardDispatcher) noteRunReadFailure(tenant string, err error) {
	if err == nil {
		if d.runReadFailure {
			d.runReadFailure = false
			d.warn("claim watchdog can read runs again — cards are being judged rather than conserved")
		}
		return
	}
	if !d.runReadFailure {
		d.runReadFailure = true
		d.warn("claim watchdog cannot read runs (first failure on tenant %s: %v) — every card is conserved until this clears",
			tenant, err)
	}
}

// reapCutoff resolves the instant an expired lease is measured against.
// The lease itself is stamped from the DATABASE clock, so measuring it
// with this pod's clock re-opens from the other end the very hole that
// stamping closed: a replica running fast would see live leases as
// expired and reclaim cards from owners that are still working. Ask the
// database. If it cannot answer, fall back to the local clock and SAY so
// — a watchdog silently measuring against a suspect clock is worse than
// one that logs its degradation.
func (d *boardDispatcher) reapCutoff(ctx context.Context) time.Time {
	local := time.Now().UTC()
	clocked, ok := d.coord.(interface {
		ServerNow(context.Context) (time.Time, error)
	})
	if !ok {
		return local
	}
	srv, err := clocked.ServerNow(ctx)
	if err != nil {
		// Edge-triggered like the zero branch below: a store hiccup at the
		// watchdog's cadence would otherwise be a log storm, which is the
		// noise that hides the next real signal.
		if d.clockFallbackReason != "unavailable" {
			d.clockFallbackReason = "unavailable"
			d.warn("claim watchdog: server clock unavailable (%v) — measuring leases against this pod's clock", err)
		}
		return local
	}
	if srv.IsZero() {
		// An empty board has no document to read a clock from. Nothing is
		// claimed either, so a skewed cutoff has nothing to steal — but say
		// it once rather than never: silence here reads as "the server
		// clock is in use" when it is not.
		if d.clockFallbackReason != "empty" {
			d.clockFallbackReason = "empty"
			d.warn("claim watchdog: no document to read the server clock from — measuring leases against this pod's clock until one exists")
		}
		return local
	}
	d.clockFallbackReason = ""
	return srv
}

// reapExpiredClaims is the cloud claim watchdog pass: same decision
// table as the local dispatcher (dispatcher.DecideStuckCard — one
// authority, F16), same transfer-before-acting order (F9). Runs on
// every replica; the per-card CAS transfer elects exactly one winner.
func (d *boardDispatcher) reapExpiredClaims(ctx context.Context, now time.Time) {
	cands, err := d.coord.ListExpiredClaimCandidates(ctx, now, 100)
	if err != nil {
		d.warn("claim watchdog list: %v", err)
		return
	}
	for _, cand := range cands {
		d.reapOne(ctx, cand, now)
	}
}

// watchdogRunCeiling is a coarse SPEND backstop, not a retry policy:
// past it the cloud watchdog stops returning a card to the pool, because
// each return costs a FRESH run there (that launcher cannot resume a
// recorded one). It is compared against the card's lifetime run count —
// every run it ever carried, whatever launched them — so it must sit far
// above any healthy card's normal traffic. A card re-queued three times
// by an operator is not a runaway.
const watchdogRunCeiling = 20

// sweepBatch bounds one recovery-claim sweep. A full batch is reported,
// never silently truncated: nothing but this sweep can reach the
// remainder.
const sweepBatch = 100

func (d *boardDispatcher) reapOne(ctx context.Context, cand boardmongo.ExpiredCandidate, now time.Time) {
	var run *store.Run
	var runErr error
	if cand.Claim.LastRunID != "" {
		if d.runFor == nil {
			// Without a run loader the table cannot be consulted —
			// conserve (the read-error row), and say so: a watchdog that
			// silently cannot decide is the failure mode this exists for.
			// Same shape as the read failure below, so the same latch: a
			// missing loader conserves EVERY card on EVERY pass, and one
			// line per card would bury the one that matters.
			d.noteRunReadFailure(cand.Tenant, errNoRunLoader)
			return
		}
		run, runErr = d.runFor(ctx, cand.Tenant, cand.Claim.LastRunID)
		if runErr != nil && errors.Is(runErr, store.ErrRunNotFound) {
			run, runErr = nil, nil // pruned run proves nothing is alive
		}
		d.noteRunReadFailure(cand.Tenant, runErr)
	}
	card := dispatcher.StuckCard{
		State: cand.Claim.State, RunningState: d.inProgressState, LaunchStates: d.eligible,
		StampWindowOpen: dispatcher.StampWindowOpen(cand.Claim.ClaimedAt, now),
	}
	// PRE-transfer: only the rows that protect a live owner (see
	// DecideTransfer). Refusing the transfer on the parked row would make
	// its own bound unreachable — and in cloud there is no boot sweep to
	// free the card later, so "held" means held for ever.
	if pre := dispatcher.DecideTransfer(run, runErr, card); pre.Action == dispatcher.StuckKeep {
		// The store's health was already reported above, and ONLY by a
		// card that actually consulted it. Reporting again here would let
		// a card that never touched the store (kept because a run stamp
		// may still be in flight) clear the latch, announcing a recovery
		// nobody observed — the flap the local twin was fixed for.
		return
	}
	var dec dispatcher.StuckDecision
	tok, liveState, err := d.coord.ReclaimExpired(ctx, cand.Tenant, cand.Claim.IssueID, cand.Claim.Prev, dispatcher.ReaperMarker(d.marker), now)
	if err != nil {
		if !errors.Is(err, tracker.ErrClaimConflict) {
			d.warn("claim watchdog reclaim %s/%s: %v", cand.Tenant, cand.Claim.IssueID, err)
		}
		return
	}
	// The transfer is the first moment state and ownership are known
	// TOGETHER, so the decision is re-taken on what it saw — every rule
	// that reads the card must judge this value, not the listing's copy.
	card.State = liveState
	if dec = dispatcher.DecideStuckCard(run, runErr, card); dec.Action == dispatcher.StuckKeep {
		// Conservation is granted ONCE: a card held under a recovery claim
		// is invisible to ListEligible and to sweepParked alike, so holding
		// it forever is the stuck card the watchdog exists to clear. The
		// recovery marker already on the claim is the record of the grant.
		if !dispatcher.IsReaperMarker(cand.Claim.Prev.Marker) {
			d.warn("claim watchdog holds %s/%s under a recovery claim: %s — re-judged at the next lease",
				cand.Tenant, cand.Claim.IssueID, dec.Reason)
			return
		}
		d.warn("claim watchdog releases %s/%s after conserving it for a full lease (%s) — "+
			"the reason has not cleared, and holding it any longer only hides the card",
			cand.Tenant, cand.Claim.IssueID, dec.Reason)
		if err := d.coord.ReleaseOwned(ctx, cand.Tenant, cand.Claim.IssueID, tok); err != nil {
			d.warn("claim watchdog release %s/%s: %v", cand.Tenant, cand.Claim.IssueID, err)
		}
		return
	}
	switch dec.Action {
	case dispatcher.StuckComplete:
		d.fileReapedCard(ctx, cand, card, d.doneState, tok)
	case dispatcher.StuckFail:
		d.fileReapedCard(ctx, cand, card, d.blockedState, tok)
	case dispatcher.StuckRepark, dispatcher.StuckReleaseOnly:
		// Unlike the local dispatcher — where the running column is itself
		// eligible, so a bare release re-arms the card — this tick only
		// ever lists d.eligible. Releasing an in_progress card here frees
		// the claim and strands the card: no cloud net picks it up
		// (sweepParked lists awaiting_input only, and there is no board
		// retry sweeper). So the return to the pool must be WRITTEN, under
		// the recovery token, before the release below.
		//
		// And it must be BOUNDED. The local dispatcher resumes the
		// RECORDED run (resolveRunID); this path calls runs.Launch, which
		// always starts a fresh one. Without a ceiling an always-failing
		// card would be relaunched once per lease forever — the watchdog
		// turning a stuck card into a spend loop. Past the ceiling the card
		// is filed as failed, which is visible and terminal.
		switch {
		case dec.Action == dispatcher.StuckRepark && cand.Claim.LifetimeRuns >= watchdogRunCeiling:
			// Only a REPARK spends a fresh run here. A release-only card
			// (the claimant died before recording anything, or its run was
			// pruned by the documented retention command) has failed at
			// nothing and must never be filed as failed.
			d.warn("claim watchdog stops returning %s/%s to the pool: it has already carried %d runs — "+
				"filing it as %s rather than paying for another",
				cand.Tenant, cand.Claim.IssueID, cand.Claim.LifetimeRuns, d.blockedState)
			d.fileReapedCard(ctx, cand, card, d.blockedState, tok)
		case len(d.eligible) > 0:
			d.fileReapedCard(ctx, cand, card, d.eligible[0], tok)
		}
	}
	if err := d.coord.ReleaseOwned(ctx, cand.Tenant, cand.Claim.IssueID, tok); err != nil {
		d.warn("claim watchdog release %s/%s: %v", cand.Tenant, cand.Claim.IssueID, err)
	}
	d.warn("claim watchdog reclaimed %s/%s from %q (%s → %s): %s",
		cand.Tenant, cand.Claim.IssueID, cand.Claim.Prev.Marker, cand.Claim.State, dec.Action, dec.Reason)
}

// fileReapedCard writes a reaped card's disposition under the recovery
// token, gated by the SHARED predicate the local reaper uses: a card an
// operator (or a bot with board.move) already moved out of the running
// column carries an intent that predates the watchdog, and overwriting it
// would silently undo, say, a manual re-queue.
func (d *boardDispatcher) fileReapedCard(ctx context.Context, cand boardmongo.ExpiredCandidate, card dispatcher.StuckCard, target string, tok tracker.ClaimToken) {
	if !dispatcher.ShouldFileStuckCard(card.State, card.RunningState, target, card.LaunchStates) {
		if card.State != target {
			d.warn("claim watchdog leaves %s/%s in %q (moved out of %q deliberately — not overwriting it with %q)",
				cand.Tenant, cand.Claim.IssueID, card.State, card.RunningState, target)
		}
		return
	}
	if err := d.coord.SetStateOwned(ctx, cand.Tenant, cand.Claim.IssueID, target, tok); err != nil {
		d.warn("claim watchdog file %s/%s → %s: %v", cand.Tenant, cand.Claim.IssueID, target, err)
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
// filing when the stamp did not land (Re9efb2). A missing seam is therefore
// an ERROR, not a silent no-op: a wiring that runs the sweep without the
// board seam must not file cards it cannot stamp (production wires
// CloudBoardCoordinator and CloudBoardFor together; this guards any future
// wiring that doesn't). Only an empty run id is a no-op — nothing to stamp.
func (s *Server) adoptCardRun(tenant, cardID, runID, workdir string) error {
	if runID == "" {
		return nil
	}
	if s.cfg.CloudBoardFor == nil {
		return errors.New("cloud board seam unwired (CloudBoardFor)")
	}
	store := s.cfg.CloudBoardFor(tenant)
	if store == nil {
		return fmt.Errorf("no board store for tenant %s", tenant)
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
	lb, err := s.resolveBotSource(ctx, iss.Bot)
	if err != nil {
		return err
	}
	defer lb.Cleanup()
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
	spec := runview.LaunchSpec{
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
	}
	lb.Stamp(&spec)
	res, err := s.runs.Launch(ctx, spec)
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

// coordLeaser binds a tenant onto the cross-tenant coordinator so the
// tenant-agnostic claimSession heartbeat can renew a cloud card's lease.
// Only RenewClaim is exercised by the session; the rest satisfy the
// tracker.ClaimLeaser interface (the cloud never calls them through it —
// its board writes go through the fenced coord methods directly).
type coordLeaser struct {
	coord  boardCoordinator
	tenant string
}

func (l coordLeaser) RenewClaim(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return l.coord.RenewClaim(ctx, l.tenant, id, tok)
}
func (l coordLeaser) ClaimLease(ctx context.Context, id, marker string) (tracker.ClaimToken, error) {
	return l.coord.Claim(ctx, l.tenant, id, marker)
}
func (l coordLeaser) ReleaseOwned(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return l.coord.ReleaseOwned(ctx, l.tenant, id, tok)
}
func (l coordLeaser) UpdateStateOwned(ctx context.Context, id, newState string, tok tracker.ClaimToken) error {
	return l.coord.SetStateOwned(ctx, l.tenant, id, newState, tok)
}
