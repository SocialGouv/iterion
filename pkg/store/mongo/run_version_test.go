package mongo

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSaveRunLegacyVersion(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	if _, err := s.CreateRun(ctx, "legacy", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "legacy"}, bson.M{"$unset": bson.M{"version": ""}}); err != nil {
		t.Fatal(err)
	}
	run, err := s.LoadRun(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	stale := *run
	run.Name = "legacy renamed"
	if err := s.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if run.CASVersion != 1 {
		t.Fatalf("version=%d", run.CASVersion)
	}
	if err := s.SaveRun(ctx, &stale); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("stale legacy save=%v", err)
	}
}

// A stored row whose version is absent or 0 loads with CASVersion 0, yet a
// same-status save of it is not a birth: the stored failure code and
// execution epoch survive a stale copy that carries neither. Only the
// stored-row half of SaveRun's birth predicate tells the two apart, and the
// storetest conformance suite cannot build such a row through RunStore.
func TestSaveRunLegacyVersionSameStatusKeepsCodeAndEpoch(t *testing.T) {
	for _, shape := range []struct {
		name   string
		update bson.M
	}{
		{"version-absent", bson.M{"$unset": bson.M{"version": ""}}},
		{"version-zero", bson.M{"$set": bson.M{"version": 0}}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			s := retryTestStore(t)
			ctx := retryCtx()
			id := "legacy-" + shape.name
			if err := s.SaveRun(ctx, &store.Run{ID: id, WorkflowName: "wf", Status: store.RunStatusFailedResumable, FailureCode: store.FailureCredSlotWait}); err != nil {
				t.Fatalf("birth save: %v", err)
			}
			if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": id}, shape.update); err != nil {
				t.Fatalf("legacy shape: %v", err)
			}
			loaded, err := s.LoadRun(ctx, id)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if loaded.CASVersion != 0 || loaded.FailureCode != store.FailureCredSlotWait || loaded.ExecutionEpoch == "" {
				t.Fatalf("precondition: cas=%d code=%q epoch=%q", loaded.CASVersion, loaded.FailureCode, loaded.ExecutionEpoch)
			}
			epoch := loaded.ExecutionEpoch
			stale := *loaded
			stale.FailureCode = ""
			if err := s.SaveRun(ctx, &stale); err != nil {
				t.Fatalf("same-status save: %v", err)
			}
			got, err := s.LoadRun(ctx, id)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if got.FailureCode != store.FailureCredSlotWait {
				t.Errorf("failure_code = %q, want %q", got.FailureCode, store.FailureCredSlotWait)
			}
			if got.ExecutionEpoch != epoch {
				t.Errorf("execution_epoch re-minted on a same-status save: %q -> %q", epoch, got.ExecutionEpoch)
			}
		})
	}
}
