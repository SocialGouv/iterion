package mongo

import (
	"bytes"
	"errors"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunMetadataChildrenPlansNotes(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	if _, e := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "r", Format: 2}); e != nil {
		t.Fatal(e)
	}
	snap := store.PlanSnapshot{NodeID: "n", Todos: []store.PlanTodo{{Content: "first", Status: "pending"}}}
	if got, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); e != nil || !ok || got.Seq != 0 {
		t.Fatal(got, ok, e)
	}
	before, e := s.LoadRun(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	if got, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); e != nil || ok || got.Seq != 0 {
		t.Fatal(got, ok, e)
	}
	after, e := s.LoadRun(ctx, "r")
	if e != nil || after.CASVersion != before.CASVersion {
		t.Fatal("dedup advanced parent", e)
	}
	if got, e := s.AppendRunNote(ctx, "r", store.RunNote{Body: "first"}); e != nil || got.Seq != 0 {
		t.Fatal(got, e)
	}
	readChildren := func(coll *mongo.Collection) []bson.Raw {
		t.Helper()
		cur, e := coll.Find(ctx, withTenantFilter(ctx, bson.M{"run_id": "r"}))
		if e != nil {
			t.Fatal(e)
		}
		defer cur.Close(ctx)
		var rows []bson.Raw
		if e = cur.All(ctx, &rows); e != nil {
			t.Fatal(e)
		}
		return rows
	}
	beforePlans, beforeNotes := readChildren(s.runPlans), readChildren(s.runNotes)
	if len(beforePlans) != 1 || len(beforeNotes) != 1 {
		t.Fatal("missing baseline child", beforePlans, beforeNotes)
	}
	if _, e = s.runs.UpdateOne(ctx, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); e != nil {
		t.Fatal(e)
	}
	raw, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	snap.Todos[0].Content = "second"
	if _, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); ok || !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(ok, e)
	}
	if _, e := s.AppendRunNote(ctx, "r", store.RunNote{Body: "second"}); !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	current, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil || !bytes.Equal(raw, current) {
		t.Fatal("refusal changed parent", e)
	}
	plans, e := s.ListPlanSnapshots(ctx, "r")
	if !errors.Is(e, store.ErrUnsupportedValueFormat) || len(plans) != 0 {
		t.Fatal(plans, e)
	}
	notes, e := s.ListRunNotes(ctx, "r")
	if !errors.Is(e, store.ErrUnsupportedValueFormat) || len(notes) != 0 {
		t.Fatal(notes, e)
	}
	if !reflect.DeepEqual(beforePlans, readChildren(s.runPlans)) || !reflect.DeepEqual(beforeNotes, readChildren(s.runNotes)) {
		t.Fatal("refused mutation changed physical children")
	}
}
