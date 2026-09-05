package runner

import (
	"fmt"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A `fail <name>: resumable: true` returns a *RuntimeError carrying the
// BOT's code after the engine wrote failed_resumable. On a cloud runner
// that error hit no carve-out — the set covers ErrRunPaused /
// ErrRunPausedOperator / ErrBudgetExceeded / ErrRunInterrupted /
// sandbox.ErrPhaseTimeout / ErrRunCancelled — so it fell to the generic
// nak-exec-failed. JetStream redelivered, dispositionForStatus synthesised
// a resume, the guard re-refused against identical caps, and the cycle
// repeated to MaxDeliver, where the DLQ park overwrote the typed code with
// DLQ_PARKED. Each turn provisioned a pod and a sandbox — the exact
// pathology the adjacent ErrBudgetExceeded carve-out exists for (R66c44b).
func TestClassifyExecResult_DeliberateRefusalAcks(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			// The typed, resumable shape: the run is parked
			// failed_resumable for a HUMAN with changed inputs.
			name: "typed resumable refusal",
			err: &runtime.RuntimeError{
				Code:    "PLAN_BUDGET_EXHAUSTED",
				NodeID:  "plan_exhausted",
				Message: "planning used 77% of max_duration",
				Cause:   runtime.ErrDeliberateFailure,
			},
		},
		{
			// The untyped `-> fail`: terminal failed. It survived only
			// because the SECOND delivery reads status `failed` and drops
			// as stale — an accident of status, not a decision, and one
			// wasted pod every time.
			name: "untyped fail node",
			err: &runtime.RuntimeError{
				Code:    store.FailureFailNode,
				NodeID:  "fail",
				Message: "workflow reached fail node",
				Cause:   runtime.ErrDeliberateFailure,
			},
		},
		{
			name: "wrapped deliberate refusal",
			err:  fmt.Errorf("runner: %w", runtime.ErrDeliberateFailure),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := classifyExecResult(c.err, "run-1")
			if out.action != actionAck {
				t.Errorf("action = %v, want actionAck — a NAK redelivers a refusal that can only repeat", out.action)
			}
			if out.finalStatus != "deliberate_failure" {
				t.Errorf("finalStatus = %q, want deliberate_failure", out.finalStatus)
			}
		})
	}
}

// A redelivery must not synthesise a resume for a run parked on a
// BOT-defined code. The runner has no allow-list of its own; the engine's
// vocabulary is the boundary (store.FailureCode.Reserved). Only an engine
// code may be auto-resumed — a deliberate refusal waits for a human who
// changed something.
func TestDispositionForStatus_BotCodeIsNotAutoResumed(t *testing.T) {
	t.Run("bot code drops the delivery", func(t *testing.T) {
		msg := &queue.RunMessage{RunID: "run-1"}
		run := &store.Run{
			ID:          "run-1",
			Status:      store.RunStatusFailedResumable,
			FailureCode: "PLAN_BUDGET_EXHAUSTED",
		}
		out := dispositionForStatus(msg, run)
		if out.proceed {
			t.Error("proceed = true — the runner re-ran a deliberate refusal against identical inputs")
		}
		if msg.Resume != nil {
			t.Error("a resume was synthesised for a bot-defined code")
		}
		if out.action != actionAck {
			t.Errorf("action = %v, want actionAck (the run stays failed_resumable for a human)", out.action)
		}
	})

	// The engine's own codes keep auto-resuming: that is what the
	// interrupted / drain redelivery path is built on.
	for _, code := range []store.FailureCode{store.FailureInterrupted, store.FailureExecutionFailed, ""} {
		t.Run("engine code "+string(code)+" still resumes", func(t *testing.T) {
			msg := &queue.RunMessage{RunID: "run-1"}
			run := &store.Run{ID: "run-1", Status: store.RunStatusFailedResumable, FailureCode: code}
			out := dispositionForStatus(msg, run)
			if !out.proceed {
				t.Fatalf("proceed = false for engine code %q — the auto-resume path is broken", code)
			}
			if msg.Resume == nil {
				t.Errorf("no resume synthesised for engine code %q", code)
			}
		})
	}

	// paused_operator shares the arm and never carries a failure code;
	// treating its empty code as "not reserved" would strand every
	// operator pause.
	t.Run("paused_operator still resumes", func(t *testing.T) {
		msg := &queue.RunMessage{RunID: "run-1"}
		run := &store.Run{ID: "run-1", Status: store.RunStatusPausedOperator}
		if out := dispositionForStatus(msg, run); !out.proceed || msg.Resume == nil {
			t.Errorf("paused_operator did not resume: proceed=%v resume=%v", out.proceed, msg.Resume)
		}
	})
}

// The DLQ park is what overwrote the typed code with DLQ_PARKED after
// MaxDeliver. With the ack in place the run never reaches it, but the
// logger wiring is what proves the outcome is dispatched as an ack and not
// as a nak that merely looks like one.
func TestDeliberateRefusalDispatchesAsAck(t *testing.T) {
	out := classifyExecResult(&runtime.RuntimeError{
		Code:  "LOT_NOT_ACTIONABLE",
		Cause: runtime.ErrDeliberateFailure,
	}, "run-1")
	d := &fakeDelivery{delivered: 1}
	dispatchExecOutcome(iterlog.Nop(), d, out, "run-1")
	if d.acks != 1 || d.naks != 0 || d.terms != 0 || len(d.nakDelays) != 0 {
		t.Fatalf("delivery transitions = %+v, want exactly one Ack", d)
	}
}
