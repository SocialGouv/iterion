package storetest

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// testSaveRunBirth pins what a run BORN through SaveRun keeps: an
// upsert-create with no stored row and version 0, the way the cloud
// publisher persists a launch it parks on CRED_SLOT_WAIT and `iterion
// migrate to-cloud` copies a run into its destination store.
//
// A birth is not a transition: it counts no outcome episode and carries no
// continuation, whatever the caller's copy says. It still owns what the run
// is born with. The failure code its document states lands on every status
// that carries one (without it, every reader of the persisted code reads
// UNKNOWN), and the run gets an execution epoch of its own, never the
// caller's and never none: an empty epoch is refused as an outcome
// preimage. Every status is driven, so a status outside CarriesFailureCode
// proves the code is normalized away on a birth exactly as on any other save.
func testSaveRunBirth(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	const callerEpoch = "caller-epoch"
	for _, status := range store.AllRunStatuses {
		t.Run(string(status), func(t *testing.T) {
			id := "run-born-" + string(status)
			born := &store.Run{
				ID:                id,
				WorkflowName:      "wf",
				Status:            status,
				FailureCode:       store.FailureCredSlotWait,
				EndReason:         store.RunEndReasonPRClosed,
				Error:             "parked: every slot of the key is held",
				OutcomeSeq:        7,
				ContinuationState: store.ContinuationRetryArmed,
				ExecutionEpoch:    callerEpoch,
			}
			if err := s.SaveRun(ctx, born); err != nil {
				t.Fatalf("SaveRun birth in %s: %v", status, err)
			}
			got := loadRunOrFatal(ctx, t, s, id)
			wantCode, wantEnd := store.FailureCode(""), store.RunEndReason("")
			if status.CarriesFailureCode() {
				wantCode, wantEnd = store.FailureCredSlotWait, store.RunEndReasonPRClosed
			}
			if got.FailureCode != wantCode {
				t.Fatalf("born %s: failure_code = %q, want %q (a birth keeps the code its document states)", status, got.FailureCode, wantCode)
			}
			if got.EndReason != wantEnd {
				t.Fatalf("born %s: end_reason = %q, want %q", status, got.EndReason, wantEnd)
			}
			if got.Error != born.Error {
				t.Fatalf("born %s: error = %q, want %q", status, got.Error, born.Error)
			}
			if got.OutcomeSeq != 0 || got.ContinuationState != "" {
				t.Fatalf("born %s: (outcome_seq %d, continuation %q), want (0, \"\") — a birth is not an outcome episode", status, got.OutcomeSeq, got.ContinuationState)
			}
			if got.ExecutionEpoch == "" || got.ExecutionEpoch == callerEpoch {
				t.Fatalf("born %s: execution_epoch = %q, want a fresh store-minted epoch", status, got.ExecutionEpoch)
			}

			// The born code is then the persisted one: a same-status copy
			// that states none does not erase it.
			stale := *got
			stale.FailureCode = ""
			if err := s.SaveRun(ctx, &stale); err != nil {
				t.Fatalf("same-status SaveRun after the birth: %v", err)
			}
			if again := loadRunOrFatal(ctx, t, s, id); again.FailureCode != wantCode || again.ExecutionEpoch != got.ExecutionEpoch {
				t.Fatalf("born %s, saved again: (failure_code %q, epoch %q), want (%q, %q)", status, again.FailureCode, again.ExecutionEpoch, wantCode, got.ExecutionEpoch)
			}
		})
	}
}
