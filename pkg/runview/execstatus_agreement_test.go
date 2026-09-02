package runview

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestExecStatusTerminalAgreement documents the node-row ExecStatus
// vocabulary's relation to the canonical RunStatus contract (ADR-095):
// isTerminalExecStatus answers MONOTONICITY ("may a stale node_started
// downgrade this row?"), not liveness — which is why Paused counts as
// terminal here while store.RunStatus.IsPaused is emphatically not
// store-terminal, and why there is no cancelled exec status at all
// (handleRunCancelled closes in-flight rows as failed with a
// cancelled-by-user marker).
func TestExecStatusTerminalAgreement(t *testing.T) {
	terminal := []ExecStatus{ExecStatusFinished, ExecStatusFailed, ExecStatusSkipped, ExecStatusPaused}
	for _, s := range terminal {
		if !isTerminalExecStatus(s) {
			t.Errorf("isTerminalExecStatus(%s) = false, want true", s)
		}
	}
	if isTerminalExecStatus(ExecStatusRunning) {
		t.Error("running must stay downgradeable")
	}
	// The documented divergence, asserted from both sides: paused is
	// exec-terminal but not store-terminal.
	if !isTerminalExecStatus(ExecStatusPaused) || store.RunStatusPausedWaitingHuman.IsTerminal() {
		t.Error("paused divergence drifted: exec-terminal AND store-non-terminal is the contract")
	}
}
