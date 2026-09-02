package dispatcher

import (
	"context"
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
	c.cfg.Store(&Config{Agent: AgentConfig{CompletedState: native.StateDone, FailedState: native.StateBlocked}})
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
// state. Honor it."). The reaper must hold the same line: an operator who
// drags a stuck card back to ready to re-queue it, or a bot that moved it
// with board.move, has expressed an intent that arrives BEFORE the
// watchdog and outranks its default filing. Without this the re-queue is
// silently undone fifteen minutes later, into a non-eligible column.
func TestReapOne_HonoursADeliberateStateMove(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	mkRun(t, runs, "run-done-moved", store.RunStatusFinished)
	cfg := c.cfg.Load()
	cfg.Agent.RunningState = native.StateInProgress
	c.cfg.Store(cfg)
	cand := seedClaimedCard(t, board, "run-done-moved")

	// The operator re-queues the stuck card while its owner is dead.
	if _, err := board.SetState(cand.IssueID, native.StateReady); err != nil {
		t.Fatalf("operator move: %v", err)
	}
	cand.State = native.StateReady

	c.reapOne(context.Background(), c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != native.StateReady {
		t.Fatalf("the watchdog overwrote a deliberate state move: card is %q, the operator had put it in %q",
			got.State, native.StateReady)
	}
	if got.Claim != "" {
		t.Fatalf("the dead owner's claim must still be freed: claim=%q", got.Claim)
	}
}
