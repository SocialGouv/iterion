package mongo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
)

type prURLAlias string

type prURLCodec struct{ calls *int }

func (v prURLCodec) MarshalBSONValue() (byte, []byte, error) {
	*v.calls++
	kind, data, err := bson.MarshalValue("pr")
	return byte(kind), data, err
}

func TestRunPRProjectionUsesEncodedInputs(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	alias := prURLAlias("pr")
	for _, f := range []store.ValueFormat{1, 2} {
		for name, value := range map[string]any{"native": "pr", "alias": alias, "pointer": &alias, "nil": nil, "empty": "", "number": 42, "bytes": []byte("pr"), "object": map[string]any{"v": "pr"}, "array": []any{"pr"}, "strings": []string{"pr"}} {
			id := fmt.Sprintf("%d-%s", f, name)
			r, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, Format: f, Inputs: map[string]any{"pr_url": value}})
			if err != nil {
				t.Fatal(id, err)
			}
			loaded, err := s.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := s.ListRunsForPR(ctx, "pr")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, got := range ids {
				if got == id {
					found = true
				}
			}
			text, ok := loaded.Inputs["pr_url"].(string)
			want := ok && text == "pr"
			if found != want {
				t.Fatalf("%s: loaded=%#v ids=%v", id, loaded.Inputs, ids)
			}
			if !want {
				continue
			}
			// Save from a loaded snapshot: clear, replace and set an alias again.
			loaded.Inputs = nil
			if err = s.SaveRun(ctx, loaded); err != nil {
				t.Fatal(err)
			}
			current, err := s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
			if err != nil {
				t.Fatal(err)
			}
			if current.Lookup("inputs_pr_url").Type != bson.TypeNull {
				t.Fatalf("%s retained projection: %v", id, current.Lookup("inputs_pr_url"))
			}
			reloaded, err := s.LoadRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			reloaded.Inputs = map[string]any{"pr_url": &alias}
			if err = s.SaveRun(ctx, reloaded); err != nil {
				t.Fatal(err)
			}
			current, err = s.runs.FindOne(ctx, bson.M{"_id": id}).Raw()
			if err != nil || current.Lookup("inputs_pr_url").StringValue() != "pr" {
				t.Fatal(id, err)
			}
			// Birth's returned object is now stale; it must not alter the projection.
			r.Inputs = map[string]any{"pr_url": "stale"}
			if err = s.SaveRun(ctx, r); !errors.Is(err, store.ErrRunConflict) {
				t.Fatal("stale SaveRun", err)
			}
		}
	}
	calls := 0
	if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "callback", Format: 1, Inputs: map[string]any{"pr_url": prURLCodec{&calls}}}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("projection re-ran caller codec", calls)
	}
	ids, err := s.ListRunsForPR(ctx, "pr")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		found = found || id == "callback"
	}
	if !found {
		t.Fatal(ids)
	}
}

func TestRunPRQuerySnapshotAndErrors(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"snapshot", "cancel-between", "cancel-commit", "future", "duplicate", "same-date"} {
		t.Run(mode, func(t *testing.T) {
			s, base := valueFormatMongo(t, true)
			ctx, cancel := context.WithTimeout(base, 20*time.Second)
			defer cancel()
			for _, id := range []string{"b", "a"} {
				f := store.LegacyValueFormat
				if id == "a" {
					f = 2
				}
				if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: id, Format: f, Inputs: map[string]any{"pr_url": "pr"}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.runs.UpdateMany(ctx, bson.M{}, bson.M{"$set": bson.M{"created_at": time.Unix(1, 0)}}); err != nil {
				t.Fatal(err)
			}
			queryctx, qcancel := context.WithCancel(ctx)
			defer qcancel()
			if mode == "future" {
				if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "a"}, bson.M{"$set": bson.M{"format_version": 99}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "duplicate" {
				if _, err := s.runs.DeleteOne(ctx, bson.M{"_id": "a"}); err != nil {
					t.Fatal(err)
				}
				doc := bson.D{{Key: "_id", Value: "a"}, {Key: "tenant_id", Value: "format-tenant"}, {Key: "inputs_pr_url", Value: "pr"}, {Key: "format_version", Value: 2}, {Key: "format_version", Value: 2}}
				if _, err := s.runs.InsertOne(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			var queries atomic.Int32
			var commits atomic.Int32
			reader := valueMonitoredClient(t, s, &event.CommandMonitor{Started: func(_ context.Context, e *event.CommandStartedEvent) {
				if e.CommandName == "commitTransaction" {
					commits.Add(1)
					if mode == "cancel-commit" {
						qcancel()
					}
				}
				if e.CommandName != "aggregate" || e.Command.Lookup("aggregate").StringValue() != colRuns {
					return
				}
				if queries.Add(1) != 2 {
					return
				}
				if mode == "snapshot" {
					if _, err := s.CreateRunWithValueFormat(ctx, store.RunCreateSpec{ID: "later", Format: 2, Inputs: map[string]any{"pr_url": "pr"}}); err != nil {
						t.Error(err)
					}
				}
				if mode == "cancel-between" {
					qcancel()
				}
			}})
			ids, err := reader.ListRunsForPR(queryctx, "pr")
			switch mode {
			case "future":
				if !errors.Is(err, store.ErrUnsupportedValueFormat) || ids != nil {
					t.Fatal(ids, err)
				}
			case "duplicate":
				if !errors.Is(err, store.ErrInvalidValueFormat) || ids != nil {
					t.Fatal(ids, err)
				}
			case "cancel-between", "cancel-commit":
				if !errors.Is(err, context.Canceled) || ids != nil {
					t.Fatal(ids, err)
				}
				if mode == "cancel-commit" && commits.Load() == 0 {
					t.Fatal("never reached commit")
				}
			default:
				if err != nil || !reflect.DeepEqual(ids, []string{"a", "b"}) {
					t.Fatal(ids, err)
				}
			}
			if mode == "snapshot" {
				next, err := s.ListRunsForPR(ctx, "pr")
				if err != nil || !reflect.DeepEqual(next, []string{"a", "b", "later"}) {
					t.Fatal(next, err)
				}
			}
		})
	}
}
