package dispatcher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// probeTrapTracker records whether the finish worker PROBED the card's
// state before its fenced write, and reproduces the window that probe
// opens: the operator re-queues the card the instant the probe returns.
// A probe-then-write worker then overwrites that move with its default
// filing — while the watchdog, which judges on the CAS-observed state,
// would have honoured it (the two authorities disagreeing on one card).
type probeTrapTracker struct {
	tracker.Tracker
	board  *native.Store
	id     string
	moveTo string // the operator's destination; "" = re-queue to ready
	probed bool
}

func (p *probeTrapTracker) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	states, err := p.Tracker.RefreshStates(ctx, ids)
	p.probed = true
	// The operator's move lands between the probe and the write.
	to := p.moveTo
	if to == "" {
		to = native.StateReady
	}
	if _, serr := p.board.SetState(p.id, to); serr != nil {
		return nil, serr
	}
	return states, err
}

// TestMaybeTransitionToCompleted_IsOneFencedCAS: with a lease backend the
// auto-transition is ONE CAS on (claim, epoch, state == running) — no
// state probe, so there is no window for an operator move to be lost in.
func TestMaybeTransitionToCompleted_IsOneFencedCAS(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	iss, err := board.Create(native.Issue{Title: "finishing", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := board.Claim(iss.ID, c.hostMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatal(err)
	}
	trap := &probeTrapTracker{Tracker: c.tracker, board: board, id: iss.ID}
	c.tracker = trap
	sess := StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil)
	defer sess.Stop()

	c.maybeTransitionToCompleted(context.Background(), iss.ID, "i1", native.StateInProgress, native.StateDone, sess)

	got, err := board.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if trap.probed {
		t.Fatalf("the finish worker probed the state before its fenced write; the operator's re-queue that landed "+
			"in that window now reads %q (a lost update the watchdog would not have made) — the auto-transition "+
			"must be ONE fenced CAS on the running state", got.State)
	}
	if got.State != native.StateDone {
		t.Fatalf("with nobody moving the card, the CAS must file it: state %q, want %q", got.State, native.StateDone)
	}
}

// TestMaybeTransitionToCompleted_HonoursAMoveThatLandedFirst: an operator
// (or the bot itself, via board.move) who moved the card out of the
// running column before the worker's write keeps it there — the CAS
// reports drift and nothing is written.
func TestMaybeTransitionToCompleted_HonoursAMoveThatLandedFirst(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	iss, err := board.Create(native.Issue{Title: "moved by hand", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := board.Claim(iss.ID, c.hostMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetState(iss.ID, native.StateReview); err != nil {
		t.Fatal(err)
	}
	sess := StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil)
	defer sess.Stop()

	c.maybeTransitionToCompleted(context.Background(), iss.ID, "i1", native.StateInProgress, native.StateDone, sess)

	if got, _ := board.Get(iss.ID); got.State != native.StateReview {
		t.Fatalf("a move that landed before the auto-transition must stand: state %q, want %q", got.State, native.StateReview)
	}
}

// TestRevertTransition_IsOneFencedCAS: the revert (a failed launch, a
// cancel, a shutdown) is the same class as the auto-transition — its
// "still in the running column" safety check must ride in the CAS, not
// precede a fenced overwrite.
func TestRevertTransition_IsOneFencedCAS(t *testing.T) {
	c, board, _ := newReaperHarness(t)
	iss, err := board.Create(native.Issue{Title: "reverting", State: native.StateReady})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := board.Claim(iss.ID, c.hostMarker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.SetStateOwned(iss.ID, native.StateInProgress, tok); err != nil {
		t.Fatal(err)
	}
	// The trap parks the card in REVIEW (an operator's decision) the
	// instant the probe returns in_progress.
	trap := &probeTrapTracker{Tracker: c.tracker, board: board, id: iss.ID, moveTo: native.StateReview}
	c.tracker = trap
	sess := StartClaimSession(c.leaser, iss.ID, tok, func(string, ...any) {}, nil)
	defer sess.Stop()

	c.revertTransition(context.Background(), iss.ID, "i1", native.StateReady, native.StateInProgress, sess)

	got, err := board.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if trap.probed {
		t.Fatalf("revertTransition probed the state before its fenced write; the operator's move that landed in "+
			"that window now reads %q — the safety check must ride in ONE fenced CAS", got.State)
	}
	if got.State != native.StateReady {
		t.Fatalf("with nobody moving the card, the revert must land: state %q, want %q", got.State, native.StateReady)
	}
}
