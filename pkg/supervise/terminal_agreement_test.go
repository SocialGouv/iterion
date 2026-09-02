package supervise

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestTerminalAgreementWithRunStatus pins how the event-level terminal
// vocabulary maps onto the canonical RunStatus contract (ADR-095), and
// documents its two DELIBERATE divergences:
//
//   - a run parking `failed_resumable` emits run_failed — the event
//     collapses final-failure and terminal-resumable, so a coordinator
//     self-closes on both (correct: the supervised execution ended;
//     a resume spawns a fresh coordinator).
//   - run_paused is NOT terminal here (the run will continue) even
//     though runview's node-row ExecStatus treats paused as settled for
//     monotonicity — different question, different answer.
func TestTerminalAgreementWithRunStatus(t *testing.T) {
	// The event the engine emits when a run ENTERS each status.
	// (finished/failed/cancelled/paused emitters live in
	// pkg/runtime/run_failure.go + engine_exec.go; failed_resumable
	// emits run_failed with resumable:true.)
	statusEvent := map[store.RunStatus]store.EventType{
		store.RunStatusFinished:           store.EventRunFinished,
		store.RunStatusFailed:             store.EventRunFailed,
		store.RunStatusFailedResumable:    store.EventRunFailed,
		store.RunStatusCancelled:          store.EventRunCancelled,
		store.RunStatusPausedWaitingHuman: store.EventRunPaused,
		store.RunStatusPausedOperator:     store.EventRunPaused,
	}
	for st, evtType := range statusEvent {
		evt := &store.Event{Type: evtType}
		got := IsTerminal(evt)
		want := st.IsTerminal() // the canonical verdict
		if st == store.RunStatusFailedResumable && got != true {
			t.Errorf("failed_resumable's run_failed must read terminal (coordinator self-close)")
		}
		if st.IsPaused() {
			if got {
				t.Errorf("run_paused must NOT be terminal for a coordinator (%s)", st)
			}
			continue
		}
		if got != want {
			t.Errorf("supervise.IsTerminal(%s → %s) = %v, canonical IsTerminal = %v — undocumented divergence", st, evtType, got, want)
		}
	}
}
