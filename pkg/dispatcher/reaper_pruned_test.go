package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestReapOne_PrunedRunIsAGiveUpNotARelaunch: a card whose RECORDED run is
// gone (pruned by `iterion runs prune`, or deleted behind a tombstone) is
// not "died pre-launch" — a stamp landed, so a run happened, and nobody
// can tell any more whether its work was delivered. Released bare, the
// card sat in the eligible running column, the next tick re-dispatched
// it, and lastRunHoldBeforeClaim read the absence as a legitimate fresh
// start: a NEW run minted on the watchdog's authority for work that may
// already be delivered. The disposition is a distinct give-up — filed
// out of the pool, stamped with why — that an operator decides on.
func TestReapOne_PrunedRunIsAGiveUpNotARelaunch(t *testing.T) {
	c, board, runs := newReaperHarness(t)
	ctx := context.Background()
	if _, err := runs.CreateRun(ctx, "run-pruned", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := runs.DeleteRun(ctx, "run-pruned"); err != nil {
		t.Fatal(err)
	}
	cand := seedClaimedCard(t, board, "run-pruned")

	c.reapOne(ctx, c.tracker.(tracker.ClaimReaper), runsFor(c), cand, time.Now().Add(2*native.ClaimLeaseDuration))

	got, err := board.Get(cand.IssueID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Claim != "" {
		t.Fatalf("the recovery claim must come off once the disposition landed, claim = %q", got.Claim)
	}
	if got.State != native.StateBlocked {
		t.Fatalf("REPRODUCED: the pruned pointer left the card in %q — an eligible column, so the next tick mints a "+
			"fresh run for work that may already be delivered; want %q with a give-up stamp", got.State, native.StateBlocked)
	}
	if got.GaveUp == nil || got.GaveUp.RunID != "run-pruned" || got.GaveUp.State != native.StateBlocked || got.GaveUp.Reason == "" {
		t.Fatalf("give-up stamp = %+v, want the pruned run named, the filed state, and a reason", got.GaveUp)
	}
	if !got.GaveUp.Current(got.State, got.LastRunID) {
		t.Fatalf("the stamp must read as CURRENT for the card's own pointer (the Needs-attention predicate): %+v vs state %q run %q",
			got.GaveUp, got.State, got.LastRunID)
	}
	// Machine provenance: the watchdog decided this by itself, so the
	// card's downstream chain must not fire as if a run had failed.
	if p := lastStatePayloadFS(t, board, cand.IssueID); p["reason"] != tracker.ReasonWatchdog {
		t.Fatalf("give-up filing provenance = %v, want reason=%q", p, tracker.ReasonWatchdog)
	}
}

func lastStatePayloadFS(t *testing.T, s *native.Store, id string) map[string]any {
	t.Helper()
	var last map[string]any
	if err := s.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueState && e.IssueID == id {
			last = e.Payload
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if last == nil {
		t.Fatal("no state event")
	}
	return last
}
