package storetest

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// testAmbiguousCancellation exercises both orderings of the cloud cancel and
// failure writes using the same seam on FS and Mongo. Mutation: restore the
// unconditional cancelled-wins guard or remove the cancel's failure-code CAS.
func testAmbiguousCancellation(t *testing.T, s store.RunStore) {
	ctx := testCtx()
	for _, order := range []string{"cancel-first", "failure-first"} {
		for _, code := range []store.FailureCode{store.FailureAmbiguousEffect, store.FailureInterrupted} {
			for _, writer := range []string{"outcome", "status-if", "status"} {
				t.Run(order+"/"+string(code)+"/"+writer, func(t *testing.T) {
					id := order + "-" + string(code) + "-" + writer
					if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
						t.Fatal(err)
					}
					if err := s.UpdateRunStatus(ctx, id, store.RunStatusRunning, ""); err != nil {
						t.Fatal(err)
					}
					cp := &store.Checkpoint{NodeID: "effect"}
					fail := func() {
						t.Helper()
						if err := s.FailRunResumable(ctx, id, cp, "unsettled command", code); err != nil {
							t.Fatal(err)
						}
					}
					cancel := func() {
						t.Helper()
						var changed bool
						var err error
						var from []store.RunStatus
						for _, status := range store.AllRunStatuses {
							if status.CanBeCancelled() {
								from = append(from, status)
							}
						}
						switch writer {
						case "outcome":
							changed, err = s.UpdateRunOutcome(ctx, id, store.RunStatusCancelled, "operator stop", store.RunOutcomeMeta{Code: store.FailureCancelled, EndReason: store.RunEndReasonOperator}, from)
						case "status-if":
							changed, err = s.UpdateRunStatusIfCoded(ctx, id, store.RunStatusCancelled, "operator stop", store.FailureCancelled, from)
						case "status":
							err = s.UpdateRunStatusCoded(ctx, id, store.RunStatusCancelled, "operator stop", store.FailureCancelled)
						}
						if err != nil {
							t.Fatal(err)
						}
						wantChanged := order != "failure-first" || code != store.FailureAmbiguousEffect
						if writer != "status" && changed != wantChanged {
							t.Errorf("cancel changed=%v, want %v", changed, wantChanged)
						}
					}
					if order == "cancel-first" {
						cancel()
						fail()
					} else {
						fail()
						cancel()
					}
					r, err := s.LoadRun(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if code == store.FailureAmbiguousEffect {
						if r.Status != store.RunStatusFailedResumable || r.FailureCode != code || r.Checkpoint == nil || r.Checkpoint.NodeID != "effect" || r.Error != "unsettled command" {
							t.Fatalf("cancel hides the undecided effect: %s/%s cp=%+v error=%q", r.Status, r.FailureCode, r.Checkpoint, r.Error)
						}
					} else if r.Status != store.RunStatusCancelled || r.FailureCode != store.FailureCancelled || r.Error != "operator stop" {
						t.Fatalf("ordinary failure overrides the cancel: %+v", r)
					}
				})
			}
		}
	}
}
