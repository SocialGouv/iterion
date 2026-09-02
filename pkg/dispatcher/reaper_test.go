package dispatcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newReaperHarness builds the minimum a reap pass needs: a real native
// board store behind the real Adapter, a real FS run store, and a
// Dispatcher wired with just the fields reapOne reads. Deterministic —
// the cutoff is injected, never waited out.
func newReaperHarness(t *testing.T) (*Dispatcher, *native.Store, *store.FilesystemRunStore) {
	t.Helper()
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	storeDir := t.TempDir()
	runs, err := store.New(storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	adapter := native.NewAdapter(board)
	c := &Dispatcher{
		tracker:    adapter,
		leaser:     adapter,
		logger:     iterlog.Nop(),
		storeDir:   storeDir,
		hostMarker: "test-host-1",
	}
	// RunningState must be SET: both card-context rows of the decision
	// table are gated on it, so a harness that leaves it empty disarms
	// them and every test built on it passes vacuously.
	c.cfg.Store(&Config{Agent: AgentConfig{
		RunningState:   native.StateInProgress,
		CompletedState: native.StateDone,
		FailedState:    native.StateBlocked,
	}})
	return c, board, runs
}

// seedClaimedCard creates an in_progress card claimed by a "dead" owner
// with a recorded run, and returns its expired-claim candidate (listed
// with a future cutoff — the lease is real, the wait is not).
func seedClaimedCard(t *testing.T, board *native.Store, runID string) tracker.ExpiredClaim {
	t.Helper()
	iss, err := board.Create(native.Issue{Title: "stuck", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := board.Claim(iss.ID, "dead-host-42"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if runID != "" {
		if err := board.SetLastRun(iss.ID, runID, "/tmp/wd"); err != nil {
			t.Fatalf("SetLastRun: %v", err)
		}
	}
	cands, err := board.ListExpiredClaimCandidates(time.Now().Add(2*native.ClaimLeaseDuration), 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, cand := range cands {
		if cand.IssueID == iss.ID {
			return cand
		}
	}
	t.Fatalf("seeded card not listed as expired")
	return tracker.ExpiredClaim{}
}

// runsFor reopens the harness dispatcher's run store the way the reaper
// pass does.
func runsFor(c *Dispatcher) *store.FilesystemRunStore {
	s, err := store.New(c.storeDir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		return nil
	}
	return s
}

func mkRun(t *testing.T, runs *store.FilesystemRunStore, id string, status store.RunStatus) {
	t.Helper()
	ctx := context.Background()
	if _, err := runs.CreateRun(ctx, id, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, _ := runs.LoadRun(ctx, id)
	r.Status = status
	if err := runs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

// TestReapOne_FinishedRunFilesTheCard is the card's "killed mid-run"
// soak criterion in deterministic form: a claimed in_progress card whose
// owner died after its run finished is reclaimed, filed as done, and
// released — today it was stuck forever.
func TestReapOne_FinishedRunFilesTheCard(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-done", store.RunStatusFinished)
	cand := seedClaimedCard(t, board, "run-done")

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateDone || got.Claim != "" {
		t.Fatalf("card = state %q claim %q, want done + released", got.State, got.Claim)
	}
	if got.ClaimEpoch <= cand.Prev.Epoch {
		t.Fatalf("epoch did not advance across the reclaim: %d", got.ClaimEpoch)
	}
}

// TestReapOne_PausedRunIsKept: ADR-014 — a parked card's retained claim
// is the parking brake; the watchdog must not touch it.
func TestReapOne_PausedRunIsKept(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-paused", store.RunStatusPausedWaitingHuman)
	cand := seedClaimedCard(t, board, "run-paused")

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, _ := board.Get(cand.IssueID)
	if got.Claim != "dead-host-42" || got.State != native.StateInProgress {
		t.Fatalf("paused card must be left untouched, got state %q claim %q", got.State, got.Claim)
	}
}

// TestReapOne_ResumableRunReleasesForRetry: failed_resumable with no
// continuation armed goes back to the pool — release only, the retry
// machinery resumes the RECORDED run.
func TestReapOne_ResumableRunReleasesForRetry(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-resumable", store.RunStatusFailedResumable)
	cand := seedClaimedCard(t, board, "run-resumable")

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, _ := board.Get(cand.IssueID)
	if got.Claim != "" {
		t.Fatalf("resumable card must be released for the retry machinery, claim = %q", got.Claim)
	}
	if got.State != native.StateInProgress {
		t.Fatalf("release-for-retry must not move the card, state = %q", got.State)
	}
	if got.LastRunID != "run-resumable" {
		t.Fatalf("the recorded run pointer must survive (resume, never a fresh sibling): %q", got.LastRunID)
	}
}

// TestReapOne_NoRunReleasesOnly: the claimant died before launching —
// the card just becomes eligible again.
func TestReapOne_NoRunReleasesOnly(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	cand := seedClaimedCard(t, board, "")

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, _ := board.Get(cand.IssueID)
	if got.Claim != "" {
		t.Fatalf("pre-launch death must release, claim = %q", got.Claim)
	}
}

// TestReapExpiredClaims_SkipsRenewedClaim: a claim whose owner renewed
// between the listing and the reclaim is a clean skip — the CAS carries
// the whole precondition.
func TestReapExpiredClaims_SkipsRenewedClaim(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-x", store.RunStatusFinished)
	cand := seedClaimedCard(t, board, "run-x")

	// The "dead" owner turns out alive: it renews after the listing.
	if err := board.RenewClaim(cand.IssueID, cand.Prev); err != nil {
		t.Fatalf("RenewClaim: %v", err)
	}
	// Reap with a cutoff that makes the ORIGINAL lease expired but not
	// the renewed one.
	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(native.ClaimLeaseDuration/2))

	got, _ := board.Get(cand.IssueID)
	if got.Claim != "dead-host-42" || got.ClaimEpoch != cand.Prev.Epoch {
		t.Fatalf("renewed claim must survive the reaper: %+v", got)
	}
}

// TestReapOne_HonoursADeliberateStateMove: the watchdog files a card the
// way its dead worker would have — and the live worker does NOT file a
// card somebody already moved (maybeTransitionToCompleted returns early
// on `cur != runningTarget`, "workflow (or operator) already moved the
// state. Honor it."). The reaper holds the same line.
//
// The move must be to a NON-launch column to be readable as an intent.
// A card sitting in a launch column (ready) is ambiguous: it is equally
// what a deliberate re-queue and a best-effort launch that never moved
// the card look like, and the two cannot be told apart from the card
// alone — see TestReapOne_FilesACardTheLaunchNeverMoved for the other
// side of that ambiguity, and why it is resolved toward filing (leaving
// such a card unfiled costs a duplicate run of delivered work; filing an
// operator's re-queue costs a reopen).
func TestReapOne_HonoursADeliberateStateMove(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-done-moved", store.RunStatusFinished)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)
	cand := seedClaimedCard(t, board, "run-done-moved")

	// The operator parks the stuck card in review while its owner is dead
	// — a non-launch column, so the intent is unambiguous.
	if _, err := board.SetState(cand.IssueID, native.StateReview); err != nil {
		t.Fatalf("operator move: %v", err)
	}
	cand.State = native.StateReview

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateReview {
		t.Fatalf("the watchdog overwrote a deliberate state move: card is %q, the operator had put it in %q",
			got.State, native.StateReview)
	}
	if got.Claim != "" {
		t.Fatalf("the dead owner's claim must still be freed: claim=%q", got.Claim)
	}
}

// TestReapOne_FilesACardTheLaunchNeverMoved is the counter-case to
// HonoursADeliberateStateMove, and the reason the guard cannot be a bare
// `cardState != runningState`: BOTH launch paths move the card into the
// running column best-effort (loop.go "continue regardless — claim is
// already taken"). A card can therefore be claimed, run to completion,
// and still sit in its launch column when the owner dies. Refusing to
// file it there leaves it eligible, and the next tick launches a second
// run for work already delivered.
func TestReapOne_FilesACardTheLaunchNeverMoved(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-done-unmoved", store.RunStatusFinished)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)

	iss, err := board.Create(native.Issue{Title: "launch never moved it", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := board.Claim(iss.ID, "dead-host-42"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := board.SetLastRun(iss.ID, "run-done-unmoved", "/tmp/wd"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	cands, err := board.ListExpiredClaimCandidates(time.Now().Add(2*native.ClaimLeaseDuration), 10)
	if err != nil || len(cands) == 0 {
		t.Fatalf("list: %v (%d)", err, len(cands))
	}
	var cand tracker.ExpiredClaim
	for _, x := range cands {
		if x.IssueID == iss.ID {
			cand = x
		}
	}

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateDone {
		t.Fatalf("a finished run whose launch never moved the card must still be filed: state=%q, want %q "+
			"(left in %q the card is eligible and the next tick launches a SECOND run for delivered work)",
			got.State, native.StateDone, got.State)
	}
}

// TestReapOne_DecidesOnTheStateTheTransferObserved: the card state that
// arrives on ExpiredClaim was read at LISTING time. Between that listing
// and the transfer the reaper loads the run (opening a store, running the
// orphan oracle) — a window an operator can walk into. Deciding the
// disposition on the listing's copy overwrites exactly the intent the
// guard exists to honour, so the state must come from the CAS itself.
func TestReapOne_DecidesOnTheStateTheTransferObserved(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-done-raced", store.RunStatusFinished)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)
	cand := seedClaimedCard(t, board, "run-done-raced")
	// cand.State is in_progress, captured at listing time. The operator
	// parks the card AFTER the listing, before the transfer.
	if _, err := board.SetState(cand.IssueID, native.StateReview); err != nil {
		t.Fatalf("operator move after listing: %v", err)
	}

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateReview {
		t.Fatalf("the watchdog decided on the LISTING's stale state: card is %q, the operator had moved it to %q "+
			"after the listing and before the transfer", got.State, native.StateReview)
	}
}

// TestReapOne_ReDecidesOnTheTransferState: the disposition is chosen
// before the transfer, on the state the LISTING carried, and only the
// filing was re-pointed at what the CAS saw. Every rule that reads the
// card — the anti-double-launch one, the parked-out-of-pool one — must
// judge the CAS value too, or an operator moving a card in that window
// gets a decision taken about a card that no longer exists.
func TestReapOne_ReDecidesOnTheTransferState(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-resumable-raced", store.RunStatusFailedResumable)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)
	cand := seedClaimedCard(t, board, "run-resumable-raced")
	// Listing said in_progress → the table would repark (release). The
	// operator parks the card out of the pool before the transfer lands.
	if _, err := board.SetState(cand.IssueID, native.StateReview); err != nil {
		t.Fatalf("operator park after listing: %v", err)
	}

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateReview {
		t.Fatalf("the card must stay where the operator parked it, got %q", got.State)
	}
	if got.Claim == "" {
		t.Fatal("the decision was taken on the LISTING's state: a card parked out of the dispatch pool must be " +
			"conserved, but its claim — the operator's only brake — was released")
	}
}

// TestReapOne_ConservationIsBoundedByTheStampWindow: a card in the
// running column with NO run recorded is conserved, because the run
// stamp is written after the launch and best-effort — its absence proves
// nothing YET. But "yet" is the whole point: the window is seconds, and
// holding the card past it produces exactly the stuck card the watchdog
// exists to clear (a claimed card is invisible to the dispatch poll).
func TestReapOne_ConservationIsBoundedByTheStampWindow(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)

	iss, err := board.Create(native.Issue{Title: "no stamp", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := board.Claim(iss.ID, "dead-host-42"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimedAt := time.Now()
	reapAt := func(at time.Time) {
		cands, err := board.ListExpiredClaimCandidates(at, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, cd := range cands {
			if cd.IssueID == iss.ID {
				c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cd, at)
			}
		}
	}

	// Lease expired, but a stamp could still be in flight: conserve.
	reapAt(claimedAt.Add(3 * native.ClaimLeaseDuration / 2))
	if cur, _ := board.Get(iss.ID); cur.Claim == "" {
		t.Fatal("while a run stamp is still plausibly in flight the claim must be conserved — " +
			"freeing it here can double-launch a worker that is alive")
	}
	// Well past it: the stamp is never coming, and holding the card only
	// hides it.
	reapAt(claimedAt.Add(4 * native.ClaimLeaseDuration))
	cur, _ := board.Get(iss.ID)
	if cur.Claim != "" {
		t.Fatalf("past the stamp window the card must be freed, not held forever: still claimed by %q "+
			"— invisible to the dispatch poll, which is the stuck card this watchdog exists to clear", cur.Claim)
	}
}

// TestReapOne_ParkedCardIsNotHeldForever: a card an operator parked out
// of the dispatch pool, whose owner then died, must not be conserved
// indefinitely. Conservation is the right FIRST answer (releasing lifts a
// brake somebody set), but a claimed card is invisible to the poll and
// unclaimable, so an unbounded hold is the stuck card this watchdog
// exists to clear — and in cloud no boot sweep ever frees it.
//
// The bound only works if the reaper TAKES the claim: the parked row is
// about where the card sits, not about anyone being alive, so refusing
// the transfer would make its own bound unreachable.
func TestReapOne_ParkedCardIsNotHeldForever(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-parked", store.RunStatusFailedResumable)

	iss, err := board.Create(native.Issue{Title: "parked by the operator", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := board.Claim(iss.ID, "dead-host-42"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := board.SetLastRun(iss.ID, "run-parked", "/tmp/wd"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	if _, err := board.SetState(iss.ID, native.StateReview); err != nil {
		t.Fatalf("operator park: %v", err)
	}

	future := time.Now().Add(2 * native.ClaimLeaseDuration)
	pass := func() {
		cands, err := board.ListExpiredClaimCandidates(future, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, cd := range cands {
			if cd.IssueID == iss.ID {
				c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cd, future)
			}
		}
	}

	pass()
	after1, _ := board.Get(iss.ID)
	if after1.State != native.StateReview {
		t.Fatalf("the operator's park must be honoured, card is %q", after1.State)
	}
	if after1.Claim == "" {
		t.Fatal("first pass must CONSERVE: releasing lifts the brake the operator set")
	}
	pass()
	after2, _ := board.Get(iss.ID)
	if after2.State != native.StateReview {
		t.Fatalf("the park must still be honoured on the second pass, card is %q", after2.State)
	}
	if after2.Claim != "" {
		t.Fatalf("conservation must be granted once, not for ever: still claimed by %q — a claimed card is "+
			"invisible to the dispatch poll and unclaimable, and in cloud nothing else frees it", after2.Claim)
	}
}

// TestReapOne_DoesNotStealFromAFreshClaim is the LOCAL half of the
// anti-steal guard — it had none, which is how the guard could exist in
// two copies with each twin covering only one. A worker whose run stamp
// has not landed yet is alive: transferring bumps the epoch and closes
// the fence on the card it is still working.
func TestReapOne_DoesNotStealFromAFreshClaim(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	iss, err := board.Create(native.Issue{Title: "stamp in flight", State: native.StateInProgress})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := board.Claim(iss.ID, "live-host-7"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// No SetLastRun: the stamp is best-effort and lands after the launch.
	// One missed beat is enough for the lease to lapse.
	at := time.Now().Add(native.ClaimLeaseDuration + time.Minute)
	cands, err := board.ListExpiredClaimCandidates(at, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, cd := range cands {
		if cd.IssueID == iss.ID {
			c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cd, at)
		}
	}
	after, _ := board.Get(iss.ID)
	if after.Claim != "live-host-7" {
		t.Fatalf("a claim taken one lease ago with its stamp still in flight must not be transferred: "+
			"now held by %q — the worker may be alive and its fenced writes are about to start failing", after.Claim)
	}
}

// TestReaperPass_LatchIsFedOncePerPass drives the PASS (the composition
// reapExpiredClaims wires), not reapOne: the latch takes ONE verdict per
// pass, so a mixed batch — one card whose run is unreadable, one healthy,
// two with no run at all — reports the failure once, repeats nothing on
// the next pass, and announces exactly one recovery when the store
// heals. Feeding it per card flapped it: a false failure/recovery warn
// pair every pass, for ever, half of it announcing a recovery nothing
// observed.
func TestReaperPass_LatchIsFedOncePerPass(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	var buf bytes.Buffer
	c.logger = iterlog.New(iterlog.LevelWarn, &buf)

	mkRun(t, runs, "run-ok", store.RunStatusFinished)
	mkRun(t, runs, "run-bad", store.RunStatusFinished)
	if err := os.WriteFile(filepath.Join(c.storeDir, "runs", "run-bad", "run.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt run: %v", err)
	}
	for _, runID := range []string{"run-ok", "run-bad", "", ""} {
		iss, err := board.Create(native.Issue{Title: "card", State: native.StateInProgress})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := board.Claim(iss.ID, "dead-host"); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if runID != "" {
			if err := board.SetLastRun(iss.ID, runID, "/tmp"); err != nil {
				t.Fatalf("SetLastRun: %v", err)
			}
		}
	}
	at := time.Now().Add(2 * native.ClaimLeaseDuration)
	reaper := c.tracker.(tracker.ClaimReaper)

	c.reapExpiredClaims(context.Background(), reaper, at)
	if cannot, again := strings.Count(buf.String(), "cannot read runs"), strings.Count(buf.String(), "can read runs again"); cannot != 1 || again != 0 {
		t.Fatalf("first pass: %d failure / %d recovery lines — want exactly 1/0 (one verdict per pass, no flap)", cannot, again)
	}

	// Second pass, same condition: the edge already fired, nothing new.
	c.reapExpiredClaims(context.Background(), reaper, at)
	if cannot, again := strings.Count(buf.String(), "cannot read runs"), strings.Count(buf.String(), "can read runs again"); cannot != 1 || again != 0 {
		t.Fatalf("second pass repeated the edge: %d failure / %d recovery lines", cannot, again)
	}

	// The store heals (the corrupt record is pruned): one recovery, once.
	if err := os.RemoveAll(filepath.Join(c.storeDir, "runs", "run-bad")); err != nil {
		t.Fatalf("prune: %v", err)
	}
	c.reapExpiredClaims(context.Background(), reaper, at)
	c.reapExpiredClaims(context.Background(), reaper, at)
	if again := strings.Count(buf.String(), "can read runs again"); again != 1 {
		t.Fatalf("recovery announced %d times, want exactly once", again)
	}
}

// TestShutdown_RevertsBeforeReleasing: every other write path in this
// package transitions first and releases last, because a release opens
// the card to the next claimant immediately. Shutdown did the reverse and
// had to UNFENCE its revert to make it land — and the revert's own guard
// ("is the card still in the running column?") cannot tell OUR
// in_progress from a successor's, so a shutting-down daemon could drag a
// card back into the launch column while a fresh run was already on it.
func TestShutdown_RevertsBeforeReleasing(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	adapter := native.NewAdapter(board)
	rec := &orderRecordingTracker{Tracker: adapter, leaser: adapter}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})

	iss, err := board.Create(native.Issue{Title: "in flight at shutdown", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := board.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatalf("SetStateOwned: %v", err)
	}
	c.state.running[iss.ID] = &runningEntry{
		IssueID: iss.ID, Identifier: "i1", TransitionedFromState: native.StateReady,
		claim: StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil),
	}

	c.shutdown()

	if len(rec.ops) < 2 || !strings.HasPrefix(rec.ops[0], "state:") {
		t.Fatalf("shutdown must transition BEFORE releasing, got %v", rec.ops)
	}
	if rec.ops[len(rec.ops)-1] != "release" {
		t.Fatalf("the release must be last, got %v", rec.ops)
	}
}

// TestShutdown_SlowRevertDoesNotStarveTheRelease: the revert opens with a
// tracker round trip, so sharing one deadline with the release lets a
// merely slow tracker spend all of it — and the release is the half
// shutdown exists for: an unreleased claim hides the card from the next
// daemon's candidate listing, and the reaper that would free it ships
// off.
func TestShutdown_SlowRevertDoesNotStarveTheRelease(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	adapter := native.NewAdapter(board)
	// Scaled down: the refresh outlasts BOTH budgets, so under one shared
	// deadline it eats all of it and the release goes out on a dead
	// context — the shape under test, at a hundredth of the cost.
	rec := &orderRecordingTracker{Tracker: adapter, leaser: adapter, slowRefresh: 120 * time.Millisecond}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.shutdownRevertBudget, c.shutdownReleaseBudget = 30*time.Millisecond, 50*time.Millisecond
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	iss, err := board.Create(native.Issue{Title: "in flight", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := board.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatalf("SetStateOwned: %v", err)
	}
	c.state.running[iss.ID] = &runningEntry{
		IssueID: iss.ID, Identifier: "i1", TransitionedFromState: native.StateReady,
		claim: StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil),
	}

	c.shutdown()

	after, _ := board.Get(iss.ID)
	if after.Claim != "" {
		t.Fatalf("a slow revert consumed the budget the release needed: card still claimed by %q — "+
			"it is now invisible to the next daemon's candidate listing", after.Claim)
	}
}

// orderRecordingTracker records the order of the board operations a
// shutdown performs. It delegates everything to the real adapter, so the
// semantics under test are production's.
type orderRecordingTracker struct {
	tracker.Tracker
	leaser      tracker.ClaimLeaser
	mu          sync.Mutex // ops — shutdown drains entries in parallel now
	ops         []string
	slowRefresh time.Duration
}

func (r *orderRecordingTracker) record(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

// RefreshStates is the revert's first act; slowing it is how a shared
// deadline starves whatever the revert is followed by.
func (r *orderRecordingTracker) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	if r.slowRefresh > 0 {
		select {
		case <-time.After(r.slowRefresh):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.Tracker.RefreshStates(ctx, ids)
}

func (r *orderRecordingTracker) ClaimLease(ctx context.Context, id, marker string) (tracker.ClaimToken, error) {
	return r.leaser.ClaimLease(ctx, id, marker)
}
func (r *orderRecordingTracker) RenewClaim(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return r.leaser.RenewClaim(ctx, id, tok)
}
func (r *orderRecordingTracker) ReleaseOwned(ctx context.Context, id string, tok tracker.ClaimToken) error {
	r.record("release")
	return r.leaser.ReleaseOwned(ctx, id, tok)
}
func (r *orderRecordingTracker) UpdateStateOwned(ctx context.Context, id, state string, tok tracker.ClaimToken) error {
	r.record("state:" + state)
	return r.leaser.UpdateStateOwned(ctx, id, state, tok)
}
func (r *orderRecordingTracker) Release(ctx context.Context, id, marker string) error {
	r.record("release")
	return r.Tracker.Release(ctx, id, marker)
}
func (r *orderRecordingTracker) UpdateState(ctx context.Context, id, state string) error {
	r.record("state:" + state)
	return r.Tracker.UpdateState(ctx, id, state)
}

// TestFinishWorker_SlowRevertDoesNotStarveTheRelease: the shutdown budget
// split's STRUCTURAL TWIN — runFinishWorker performs the same
// transition-then-release pair, and it runs at EVERY finished run, not
// only at shutdown. Under one shared deadline a merely slow tracker spent
// it all on the revert's RefreshStates and left the claim in place: the
// card invisible to the next daemon's listing, with the reaper that would
// free it gated off by default.
func TestFinishWorker_SlowRevertDoesNotStarveTheRelease(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	adapter := native.NewAdapter(board)
	rec := &orderRecordingTracker{Tracker: adapter, leaser: adapter, slowRefresh: 120 * time.Millisecond}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.shutdownRevertBudget, c.shutdownReleaseBudget = 30*time.Millisecond, 50*time.Millisecond
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	iss, err := board.Create(native.Issue{Title: "finishing", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok, err := board.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatalf("SetStateOwned: %v", err)
	}
	sess := StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil)

	c.runFinishWorker(finishPlan{
		kind: finishRevert, issueID: iss.ID, identifier: "i1",
		runningTarget: native.StateInProgress, sourceState: native.StateReady,
		session: sess,
	})

	after, _ := board.Get(iss.ID)
	if after.Claim != "" {
		t.Fatalf("a slow revert consumed the budget the release needed: card still claimed by %q — "+
			"invisible to the next daemon's candidate listing at EVERY run finish", after.Claim)
	}
}

// TestLastRunHold_DeletedRunIsProofOfAbsence: the dispatcher's hold
// authority must read a durable delete tombstone the way the admission
// loop's pipelineTicketLaunchable does — proof the run is gone, not the
// unreadable-store input the hold exists for. Divergent answers on the
// same input is how one authority bricks the card the other would free.
func TestLastRunHold_DeletedRunIsProofOfAbsence(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	c.state = newState() // the hold path records its decision on the actor state
	ctx := context.Background()
	if _, err := runs.CreateRun(ctx, "run-del", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := runs.DeleteRun(ctx, "run-del"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.LoadRun(ctx, "run-del"); !errors.Is(err, store.ErrRunDeleted) {
		t.Fatalf("precondition: want ErrRunDeleted, got %v", err)
	}
	iss, err := board.Create(native.Issue{Title: "retried", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetLastRun(iss.ID, "run-del", ""); err != nil {
		t.Fatal(err)
	}
	if c.lastRunHoldBeforeClaim(tracker.Issue{ID: iss.ID, Identifier: "i1"}) {
		t.Fatal("a ticket whose last run was DELETED is held for ever — the tombstone proves absence, a fresh start is the legitimate path")
	}
}

// panickyRefreshTracker panics inside the revert's first act — the shape
// of any bug in the transition switch.
type panickyRefreshTracker struct {
	tracker.Tracker
	leaser tracker.ClaimLeaser
}

func (p *panickyRefreshTracker) RefreshStates(context.Context, []string) (map[string]string, error) {
	panic("boom in the transition phase")
}
func (p *panickyRefreshTracker) ClaimLease(ctx context.Context, id, marker string) (tracker.ClaimToken, error) {
	return p.leaser.ClaimLease(ctx, id, marker)
}
func (p *panickyRefreshTracker) RenewClaim(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return p.leaser.RenewClaim(ctx, id, tok)
}
func (p *panickyRefreshTracker) ReleaseOwned(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return p.leaser.ReleaseOwned(ctx, id, tok)
}
func (p *panickyRefreshTracker) UpdateStateOwned(ctx context.Context, id, state string, tok tracker.ClaimToken) error {
	return p.leaser.UpdateStateOwned(ctx, id, state, tok)
}

// TestFinishWorker_PanicStillReleasesTheClaim: the release + heartbeat
// stop run in a defer, so a panic in the transition switch (recovered
// upstream by the worker wrapper) cannot leave a session beating on a
// card whose run is over — that session renews the lease for ever and
// the reaper never lists the card.
func TestFinishWorker_PanicStillReleasesTheClaim(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := native.NewAdapter(board)
	rec := &panickyRefreshTracker{Tracker: adapter, leaser: adapter}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	iss, err := board.Create(native.Issue{Title: "panics mid-finish", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := board.Claim(iss.ID, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatal(err)
	}
	sess := StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("precondition: the transition phase must have panicked")
			}
		}()
		c.runFinishWorker(finishPlan{
			kind: finishRevert, issueID: iss.ID, identifier: "i1",
			runningTarget: native.StateInProgress, sourceState: native.StateReady,
			session: sess,
		})
	}()

	after, _ := board.Get(iss.ID)
	if after.Claim != "" {
		t.Fatalf("panic skipped the release: card still claimed by %q with its heartbeat running — invisible to the reaper for ever", after.Claim)
	}
}

// TestShutdown_DrainIsBoundedByOneCardsBudgets: entries drain in
// parallel — a fleet of slow reverts costs ONE card's budgets, not N of
// them, or the SIGKILL lands before the tail and those claims are never
// released.
func TestShutdown_DrainIsBoundedByOneCardsBudgets(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := native.NewAdapter(board)
	rec := &orderRecordingTracker{Tracker: adapter, leaser: adapter, slowRefresh: 200 * time.Millisecond}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.shutdownRevertBudget, c.shutdownReleaseBudget = 40*time.Millisecond, 40*time.Millisecond
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	for i := 0; i < 4; i++ {
		iss, err := board.Create(native.Issue{Title: "in flight", State: native.StateReady})
		if err != nil {
			t.Fatal(err)
		}
		tok, err := board.Claim(iss.ID, "host-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
			t.Fatal(err)
		}
		c.state.running[iss.ID] = &runningEntry{
			IssueID: iss.ID, Identifier: "i", TransitionedFromState: native.StateReady,
			claim: StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil),
		}
	}

	start := time.Now()
	c.shutdown()
	elapsed := time.Since(start)

	// Sequential would be ≥ 4 × 40ms of dead revert contexts; parallel is
	// one card's worth plus scheduling noise.
	if elapsed > 120*time.Millisecond {
		t.Fatalf("drain took %s for 4 cards — sequential again: under a real grace period the SIGKILL beats the tail and those claims stay on disk", elapsed)
	}
	for id := range c.state.running {
		if cur, _ := board.Get(id); cur.Claim != "" {
			t.Fatalf("card %s still claimed by %q after shutdown", id, cur.Claim)
		}
	}
}

// serializingSlowTracker: one global lock, fixed latency per call — the
// shape of the FS store's single mutex and of a rate-limited forge
// client. What it exposes: goroutines whose budgets started before
// their turn burn a SHARED window while queued.
type serializingSlowTracker struct {
	tracker.Tracker
	leaser  tracker.ClaimLeaser
	gate    sync.Mutex
	latency time.Duration
}

func (r *serializingSlowTracker) call(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.gate.Lock()
	defer r.gate.Unlock()
	time.Sleep(r.latency)
	return ctx.Err()
}
func (r *serializingSlowTracker) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	if err := r.call(ctx); err != nil {
		return nil, err
	}
	return r.Tracker.RefreshStates(ctx, ids)
}
func (r *serializingSlowTracker) ClaimLease(ctx context.Context, id, marker string) (tracker.ClaimToken, error) {
	return r.leaser.ClaimLease(ctx, id, marker)
}
func (r *serializingSlowTracker) RenewClaim(ctx context.Context, id string, tok tracker.ClaimToken) error {
	return r.leaser.RenewClaim(ctx, id, tok)
}
func (r *serializingSlowTracker) ReleaseOwned(ctx context.Context, id string, tok tracker.ClaimToken) error {
	if err := r.call(ctx); err != nil {
		return err
	}
	return r.leaser.ReleaseOwned(ctx, id, tok)
}
func (r *serializingSlowTracker) UpdateStateOwned(ctx context.Context, id, state string, tok tracker.ClaimToken) error {
	if err := r.call(ctx); err != nil {
		return err
	}
	return r.leaser.UpdateStateOwned(ctx, id, state, tok)
}

// TestShutdown_DrainDoesNotLeakAgainstASerializingTracker: bounded
// parallelism with budgets that start at each card's TURN. Unbounded
// goroutines whose clocks all started at T0 burned one shared window
// while queued behind the tracker's lock — claims at the tail were
// refused at entry and leaked, the very failure the drain exists to
// prevent (measured at N=20 with production budgets: 7 leaked where the
// old sequential drain freed all 20).
func TestShutdown_DrainDoesNotLeakAgainstASerializingTracker(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := native.NewAdapter(board)
	rec := &serializingSlowTracker{Tracker: adapter, leaser: adapter, latency: 30 * time.Millisecond}
	c := &Dispatcher{
		tracker: rec, leaser: rec, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
		ws: newWsBridge(iterlog.Nop()),
	}
	c.shutdownRevertBudget, c.shutdownReleaseBudget = 500*time.Millisecond, 500*time.Millisecond
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	for i := 0; i < 32; i++ {
		iss, err := board.Create(native.Issue{Title: "in flight", State: native.StateReady})
		if err != nil {
			t.Fatal(err)
		}
		tok, err := board.Claim(iss.ID, "host-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
			t.Fatal(err)
		}
		c.state.running[iss.ID] = &runningEntry{
			IssueID: iss.ID, Identifier: "i", TransitionedFromState: native.StateReady,
			claim: StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil),
		}
	}

	c.shutdown()

	leaked := 0
	for id := range c.state.running {
		if cur, _ := board.Get(id); cur.Claim != "" {
			leaked++
		}
	}
	if leaked > 0 {
		t.Fatalf("%d/32 claims leaked against a serializing tracker — budgets that start before a card's turn burn a shared window while queued", leaked)
	}
}
