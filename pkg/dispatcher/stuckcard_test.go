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
		{"no run = died pre-launch", nil, nil, StuckReleaseOnly},
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
		{"finished completes the card", run(store.RunStatusFinished), nil, StuckComplete},
		{"terminal failure files the card", run(store.RunStatusFailed), nil, StuckFail},
		{"resumable goes back to the retry machinery", run(store.RunStatusFailedResumable), nil, StuckRepark},
		{"unknown status conserved never guessed", run(store.RunStatus("weird_future_state")), nil, StuckKeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideStuckCard(tc.run, tc.err)
			if got.Action != tc.want {
				t.Fatalf("action = %s (%s), want %s", got.Action, got.Reason, tc.want)
			}
			if got.Reason == "" {
				t.Fatal("every decision must carry its evidence")
			}
		})
	}
}
