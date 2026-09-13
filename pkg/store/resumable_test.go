package store

import "testing"

// The rule was re-implemented by hand in three packages and they diverged:
// the bot-facing run_get left `cancelled` out while the engine's own resume
// gate accepted it. An assistant reading that tool then told the operator a
// cancelled run could not be resumed — closing a door that was open, and
// doing it with the authority of a tool result rather than a guess.
//
// `cancelled` is still resumable because it preserves a checkpoint. A
// successful rewind now parks in paused_operator, which is also resumable;
// neither recovery path may read as an unrecoverable run.
func TestIsResumable_CoversEveryStatusThatKeepsACheckpoint(t *testing.T) {
	for _, tc := range []struct {
		status RunStatus
		want   bool
		why    string
	}{
		{RunStatusPausedWaitingHuman, true, "parked on a human gate, answers pending"},
		{RunStatusPausedOperator, true, "the operator parked it and can unpark it"},
		{RunStatusFailedResumable, true, "the engine kept the checkpoint on purpose"},
		{RunStatusCancelled, true, "checkpoint preserved after an interruption"},
		{RunStatusFailed, false, "reached a fail node or died before a checkpoint"},
		{RunStatusFinished, false, "there is nothing left to do"},
		{RunStatusRunning, false, "already executing"},
		{RunStatusQueued, false, "not claimed yet, nothing to resume from"},
	} {
		if got := tc.status.IsResumable(); got != tc.want {
			t.Errorf("%s: IsResumable() = %v, want %v (%s)", tc.status, got, tc.want, tc.why)
		}
	}
}
