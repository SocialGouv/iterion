package dispatcher

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// One test per row of the DecideStuckCard table — the table is the
// policy, so every arm gets its own pin (a row nobody asserts is a row
// that drifts).
func TestDecideStuckCard(t *testing.T) {
	run := func(st store.RunStatus, mut ...func(*store.Run)) *store.Run {
		r := &store.Run{ID: "r1", Status: st}
		for _, m := range mut {
			m(r)
		}
		return r
	}
	cases := []struct {
		name string
		run  *store.Run
		err  error
		want StuckAction
	}{
		{"read error conserves", run(store.RunStatusFinished), errors.New("mongo down"), StuckKeep},
		// "no run" is card-dependent, so it lives in the card-context test
		// below rather than in this run-status table.
		{"running is never stolen from", run(store.RunStatusRunning), nil, StuckKeep},
		{"queued is never stolen from", run(store.RunStatusQueued), nil, StuckKeep},
		{"paused human keeps the parking brake", run(store.RunStatusPausedWaitingHuman), nil, StuckKeep},
		{"paused operator keeps the parking brake", run(store.RunStatusPausedOperator), nil, StuckKeep},
		{"operator cancel is never auto-routed", run(store.RunStatusCancelled), nil, StuckKeep},
		{"redelivery pending is platform-owned", run(store.RunStatusFailedResumable, func(r *store.Run) {
			r.ContinuationState = store.ContinuationRedeliveryPending
		}), nil, StuckKeep},
		{"retry armed is platform-owned", run(store.RunStatusFailedResumable, func(r *store.Run) {
			r.ContinuationState = store.ContinuationRetryArmed
		}), nil, StuckKeep},
		{"an orphan adopted by a delivery is platform-owned", run(store.RunStatusFailedResumable, func(r *store.Run) {
			r.FailureCode = store.FailureProcessOrphaned
			r.ContinuationState = store.ContinuationRedeliveryPending
		}), nil, StuckKeep},
		{"finished completes the card", run(store.RunStatusFinished), nil, StuckComplete},
		{"terminal failure files the card", run(store.RunStatusFailed), nil, StuckFail},
		{"resumable goes back to the retry machinery", run(store.RunStatusFailedResumable), nil, StuckRepark},
		{"unknown status conserved never guessed", run(store.RunStatus("weird_future_state")), nil, StuckKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The base table is judged on a card that IS in the running
			// column: that is the nominal shape, and it keeps these rows
			// about the RUN's status alone.
			got := DecideStuckCard(tc.run, tc.err, StuckCard{
				State: "in_progress", RunningState: "in_progress", LaunchStates: []string{"ready"},
			})
			if got.Action != tc.want {
				t.Fatalf("action = %s (%s), want %s", got.Action, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("every decision must carry its evidence")
			}
		})
	}
}

// TestDecideStuckCard_CardContext covers the two rows that are about the
// CARD rather than the run — each one a way the watchdog could destroy
// work by acting on an absence.
func TestDecideStuckCard_CardContext(t *testing.T) {
	// StampWindowOpen: a stamp could still be in flight (the claim is
	// fresh). Its false counterpart is the row right below.
	inRunning := StuckCard{State: "in_progress", RunningState: "in_progress", LaunchStates: []string{"ready"}, StampWindowOpen: true}
	parked := StuckCard{State: "awaiting_input", RunningState: "in_progress", LaunchStates: []string{"ready"}}
	inLaunch := StuckCard{State: "ready", RunningState: "in_progress", LaunchStates: []string{"ready"}}

	// No run stamped + card already running: the stamp is best-effort and
	// lands AFTER the launch, so this is not evidence the claimant died.
	if got := DecideStuckCard(nil, nil, inRunning); got.Action != StuckKeep {
		t.Fatalf("no run + running card = %s (%s), want keep — freeing it could double-launch a live worker",
			got.Action, got.Reason)
	}
	// No run stamped + card never left the launch column: nothing ran.
	if got := DecideStuckCard(nil, nil, inLaunch); got.Action != StuckReleaseOnly {
		t.Fatalf("no run + card still in its launch column = %s, want release", got.Action)
	}
	// A resumable run whose card an operator parked out of the pool: the
	// release would lift a brake somebody set on purpose.
	resumable := &store.Run{ID: "r1", Status: store.RunStatusFailedResumable}
	if got := DecideStuckCard(resumable, nil, parked); got.Action != StuckKeep {
		t.Fatalf("resumable + parked card = %s (%s), want keep", got.Action, got.Reason)
	}
	if got := DecideStuckCard(resumable, nil, inRunning); got.Action != StuckRepark {
		t.Fatalf("resumable + running card = %s, want repark", got.Action)
	}
	// ...and the conservative row EXPIRES: once no stamp can credibly be
	// in flight any more, holding the card only hides it.
	stale := inRunning
	stale.StampWindowOpen = false
	if got := DecideStuckCard(nil, nil, stale); got.Action != StuckReleaseOnly {
		t.Fatalf("no run + running card past the stamp window = %s (%s), want release — "+
			"a held card is invisible to the dispatch poll", got.Action, got.Reason)
	}
}

// The exact doc the runner's under-lock adoption writes — failed_resumable,
// PROCESS_ORPHANED, redelivery_pending — is about to be resumed by the very
// delivery that wrote it. The pre-transfer decision must keep the card:
// a transfer would re-park it into a duplicate run on cloud.
// TestDecideStuckCard_PrunedPointerIsAGiveUp: "no run loaded" has two
// causes and they must not share a disposition. No run RECORDED is the
// claimant dying before launch (release-only: the card is simply eligible
// again). A recorded run that is GONE — pruned by the retention command,
// deleted behind a tombstone — means a run happened and nobody can tell
// whether its work was delivered: freeing the card re-dispatches it
// (locally the running column is eligible; in cloud the repark writes it
// back into the pool), minting a fresh run on the watchdog's authority.
// That row is a give-up an operator decides on, whatever the stamp window
// says (the pointer IS the stamp, so nothing is still in flight).
func TestDecideStuckCard_PrunedPointerIsAGiveUp(t *testing.T) {
	pruned := StuckCard{
		State: "in_progress", RunningState: "in_progress", LaunchStates: []string{"ready"},
		RecordedRunID: "run-gone", StampWindowOpen: true,
	}
	if got := DecideStuckCard(nil, nil, pruned); got.Action != StuckGiveUp {
		t.Fatalf("pruned pointer: action = %s (%s), want give_up", got.Action, got.Reason)
	}
	if got := DecideTransfer(nil, nil, pruned); got.Action == StuckKeep {
		t.Fatalf("a pruned pointer protects no live owner — the transfer must proceed, got keep (%s)", got.Reason)
	}
	// In a launch column too: the card was never moved by its launch, and
	// its pointer is still gone.
	pruned.State, pruned.StampWindowOpen = "ready", false
	if got := DecideStuckCard(nil, nil, pruned); got.Action != StuckGiveUp {
		t.Fatalf("pruned pointer in a launch column: action = %s, want give_up", got.Action)
	}
	// A read error still conserves — absence must be PROVEN, never inferred
	// from a store blip.
	if got := DecideStuckCard(nil, errors.New("store down"), pruned); got.Action != StuckKeep {
		t.Fatalf("read error with a recorded run: action = %s, want keep", got.Action)
	}
	// And the genuine no-run shape keeps its release-only row.
	if got := DecideStuckCard(nil, nil, StuckCard{State: "ready", RunningState: "in_progress", LaunchStates: []string{"ready"}}); got.Action != StuckReleaseOnly {
		t.Fatalf("no run recorded: action = %s, want release", got.Action)
	}
}

func TestDecideTransfer_KeepsAnAdoptedOrphan(t *testing.T) {
	adopted := &store.Run{
		ID: "r1", Status: store.RunStatusFailedResumable,
		FailureCode: store.FailureProcessOrphaned, ContinuationState: store.ContinuationRedeliveryPending,
	}
	got := DecideTransfer(adopted, nil, StuckCard{State: "in_progress", RunningState: "in_progress", LaunchStates: []string{"ready"}})
	if got.Action != StuckKeep {
		t.Fatalf("action = %s (%s), want keep — the platform owns this run's next seconds", got.Action, got.Reason)
	}
}
