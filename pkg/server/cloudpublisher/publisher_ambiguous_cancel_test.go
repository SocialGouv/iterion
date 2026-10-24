package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A failure can land between the publisher's read and CAS. The store owns
// that race; the publisher must also accept its no-op without asking to retry.
type failureDuringCancelStore struct {
	store.RunStore
	beforeCancel func()
}

func (s failureDuringCancelStore) UpdateRunOutcome(ctx context.Context, id string, status store.RunStatus, msg string, meta store.RunOutcomeMeta, from []store.RunStatus) (bool, error) {
	s.beforeCancel()
	return s.RunStore.UpdateRunOutcome(ctx, id, status, msg, meta, from)
}

// Mutations: restore the cancelled-wins guard, drop the cancel failure-code
// CAS, or drop the publisher's AMBIGUOUS_EFFECT no-op on the post-CAS re-read.
func TestCloudCancelPreservesUndecidedFailure(t *testing.T) {
	for _, order := range []string{"cancel-first", "failure-first", "failure-during-cancel"} {
		t.Run(order, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CreateRun(ctx, "r", "wf", nil); err != nil {
				t.Fatal(err)
			}
			fail := func() {
				t.Helper()
				if err := s.FailRunResumable(ctx, "r", &store.Checkpoint{NodeID: "effect"}, "command may still run", store.FailureAmbiguousEffect); err != nil {
					t.Fatal(err)
				}
			}
			p := &Publisher{store: s, cancelRun: func(string, store.LeaseIdentity) error { return nil }}
			switch order {
			case "failure-first":
				fail()
			case "failure-during-cancel":
				p.store = failureDuringCancelStore{RunStore: s, beforeCancel: fail}
			}
			if err := p.CancelRun(ctx, "r"); err != nil {
				t.Fatal(err)
			}
			if order == "cancel-first" {
				fail()
			}
			r, err := s.LoadRun(ctx, "r")
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureAmbiguousEffect || r.Checkpoint == nil || r.Checkpoint.NodeID != "effect" || r.Error != "command may still run" {
				t.Fatalf("cloud cancel hides the effect: status=%s code=%s checkpoint=%+v error=%q", r.Status, r.FailureCode, r.Checkpoint, r.Error)
			}
		})
	}
}
