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
