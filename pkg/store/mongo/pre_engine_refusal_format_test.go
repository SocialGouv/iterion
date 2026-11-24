package mongo

import (
	"bytes"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
	"time"
)

func TestPreEngineRefusalRejectsFutureFormat(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	_, e := s.runs.InsertOne(ctx, bson.D{{Key: "_id", Value: "r"}, {Key: "tenant_id", Value: "format-tenant"}, {Key: "format_version", Value: 99}, {Key: "status", Value: "queued"}, {Key: "version", Value: 1}})
	if e != nil {
		t.Fatal(e)
	}
	before, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.RecordPreEngineRefusal(ctx, "r", store.PreEngineRefusalRequest{Phase: "executor", Code: store.FailureExecutionFailed, Message: "unsupported", PublishedAt: time.Now(), Disposition: store.PreEngineRetryPending})
	after, read := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if read != nil {
		t.Fatal(read)
	}
	if result.Changed || !errors.Is(e, store.ErrUnsupportedValueFormat) || !bytes.Equal(before, after) {
		t.Fatalf("future format mutated: result=%+v err=%v bytesChanged=%v", result, e, !bytes.Equal(before, after))
	}
}
func TestPreEngineRefusalHonorsOriginalFormat(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	r, e := s.CreateRunWithValueFormat(ctx, mongoExactBirth("r"))
	if e != nil {
		t.Fatal(e)
	}
	stale, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: r.ID, Format: store.LegacyValueFormat})
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.RecordPreEngineRefusal(stale, r.ID, store.PreEngineRefusalRequest{Phase: "executor", Code: store.FailureExecutionFailed, Message: "stale", PublishedAt: time.Now().Add(time.Second), Disposition: store.PreEngineRetryPending})
	after, read := s.LoadRun(ctx, r.ID)
	if read != nil {
		t.Fatal(read)
	}
	if result.Changed || !errors.Is(e, store.ErrValueFormatChanged) || after.PreEngineRefusal != nil {
		t.Fatalf("stale format mutated: result=%+v err=%v refusal=%+v", result, e, after.PreEngineRefusal)
	}
}
func TestExactPreEngineRefusalLifecycle(t *testing.T) {
	s, ctx := valueFormatMongo(t, true)
	r, e := s.CreateRunWithValueFormat(ctx, mongoExactBirth("lifecycle"))
	if e != nil {
		t.Fatal(e)
	}
	q := store.PreEngineRefusalRequest{Phase: "executor", Code: store.FailureExecutionFailed, Message: "$exact refusal", PublishedAt: time.Now().Add(time.Second), Disposition: store.PreEngineRetryPending}
	result, e := s.RecordPreEngineRefusal(ctx, r.ID, q)
	if e != nil || !result.Changed {
		t.Fatal(result, e)
	}
	saved, e := s.LoadRun(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if saved.ExecutionEpoch != r.ExecutionEpoch {
		t.Fatal("queued diagnostic changed execution ownership")
	}
	saved.Name = "rename"
	saved.PreEngineRefusal = nil
	saved.Error = ""
	if e = s.SaveRun(ctx, saved); e != nil {
		t.Fatal(e)
	}
	got, e := s.LoadRun(ctx, r.ID)
	if e != nil || got.Error != q.Message || got.PreEngineRefusal == nil || got.ExecutionEpoch != r.ExecutionEpoch {
		t.Fatal("metadata save lost refusal or epoch", got, e)
	}
	ctx, e = store.BindRunValueFormat(ctx, got)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PauseRun(ctx, r.ID, &store.Checkpoint{NodeID: "waiting", InteractionID: "q"}); e != nil {
		t.Fatal(e)
	}
	paused, e := s.LoadRun(ctx, r.ID)
	if e != nil || paused.PreEngineRefusal != nil || paused.Checkpoint == nil || paused.Checkpoint.InteractionID != "q" || paused.ExecutionEpoch == r.ExecutionEpoch {
		t.Fatal("pause composition lost", paused, e)
	}
	if e = s.UpdateRunStatus(ctx, r.ID, store.RunStatusQueued, ""); e != nil {
		t.Fatal(e)
	}
	q.PublishedAt = time.Now().Add(time.Second)
	if result, e = s.RecordPreEngineRefusal(ctx, r.ID, q); e != nil || !result.Changed {
		t.Fatal("pre-delete refusal not recorded", result, e)
	}
	if e = s.DeleteRun(ctx, r.ID); e != nil {
		t.Fatal(e)
	}
	raw, e := s.runs.FindOne(ctx, bson.M{"_id": r.ID}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = raw.LookupErr("pre_engine_refusal"); e == nil {
		t.Fatal("deleted refusal retained")
	}
}
