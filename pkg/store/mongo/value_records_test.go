package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
	"reflect"
	"sync"
	"testing"
	"time"
)

func mongoValueBind(t *testing.T, ctx context.Context, id string, f store.ValueFormat) context.Context {
	t.Helper()
	c, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: id, Format: f})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func mongoValueStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, c := valueFormatMongo(t, true)
	if _, e := s.CreateRunWithValueFormat(c, mongoExactBirth("r")); e != nil {
		t.Fatal(e)
	}
	return s, mongoValueBind(t, c, "r", 2)
}
func mongoValuePayload() map[string]any {
	return map[string]any{"i": int64(9007199254740993), "n": json.Number("1e400"), "tiny": json.Number("1e-4000"), "bytes": []byte{0, 255}, "empty": []any{}, "nil": []any(nil)}
}
func requireMongoValuePayload(t *testing.T, got map[string]any) {
	t.Helper()
	want, e := valuecodec.CanonicalJSON(t.Context(), mongoValuePayload())
	if e != nil {
		t.Fatal(e)
	}
	actual, e := valuecodec.CanonicalJSON(t.Context(), got)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(actual, want) {
		t.Fatalf("payload lost: %#v", got)
	}
}
func TestValueRecordsMongoRoundtrip(t *testing.T) {
	t.Parallel()
	s, ctx := mongoValueStore(t)
	if e := s.WriteArtifact(ctx, &store.Artifact{RunID: "r", NodeID: "n", Version: 1, Data: mongoValuePayload()}); e != nil {
		t.Fatal(e)
	}
	for _, load := range []func() (*store.Artifact, error){func() (*store.Artifact, error) { return s.LoadArtifact(ctx, "r", "n", 1) }, func() (*store.Artifact, error) { return s.LoadLatestArtifact(ctx, "r", "n") }} {
		a, e := load()
		if e != nil {
			t.Fatal(e)
		}
		requireMongoValuePayload(t, a.Data)
	}
	if vs, e := s.ListArtifactVersions(ctx, "r", "n"); e != nil || len(vs) != 1 || vs[0].Version != 1 {
		t.Fatal(vs, e)
	}
	if _, e := s.AppendEvent(ctx, "r", store.Event{Type: "value_test", Data: mongoValuePayload()}); e != nil {
		t.Fatal(e)
	}
	es, e := s.LoadEvents(ctx, "r")
	if e != nil || len(es) != 2 {
		t.Fatal(es, e)
	}
	requireMongoValuePayload(t, es[1].Data)
	// Visitors may call writers: ScanEvents must release the stable lock before visit.
	if e = s.ScanEvents(ctx, "r", func(*store.Event) bool {
		if e := s.WriteArtifact(ctx, &store.Artifact{RunID: "r", NodeID: "from-visitor", Version: 1, Data: mongoValuePayload()}); e != nil {
			t.Error(e)
		}
		return false
	}); e != nil {
		t.Fatal(e)
	}
	if e = s.WriteInteraction(ctx, &store.Interaction{RunID: "r", ID: "q", Questions: mongoValuePayload()}); e != nil {
		t.Fatal(e)
	}
	i, e := store.AnswerInteraction(ctx, s, "r", "q", mongoValuePayload())
	if e != nil {
		t.Fatal(e)
	}
	requireMongoValuePayload(t, i.Answers)
	i, e = s.LoadInteraction(ctx, "r", "q")
	if e != nil {
		t.Fatal(e)
	}
	requireMongoValuePayload(t, i.Questions)
	requireMongoValuePayload(t, i.Answers)
	for _, write := range []func(*store.Checkpoint) error{func(cp *store.Checkpoint) error { return s.SaveCheckpoint(ctx, "r", cp) }, func(cp *store.Checkpoint) error { return s.PauseRun(ctx, "r", cp) }, func(cp *store.Checkpoint) error {
		return s.FailRunResumable(ctx, "r", cp, "fail", store.FailureCode(""))
	}, func(cp *store.Checkpoint) error {
		return s.FailRunTerminal(ctx, "r", cp, "fail", store.FailureCode(""))
	}} {
		if e = write(&store.Checkpoint{Outputs: map[string]map[string]any{"n": mongoValuePayload()}, Vars: mongoValuePayload()}); e != nil {
			t.Fatal(e)
		}
		r, e := s.LoadRun(ctx, "r")
		if e != nil {
			t.Fatal(e)
		}
		requireMongoValuePayload(t, r.Checkpoint.Outputs["n"])
		requireMongoValuePayload(t, r.Checkpoint.Vars)
	}
}

func TestValueRecordsMongoRefusalNoEffects(t *testing.T) {
	t.Parallel()
	s, exact := mongoValueStore(t)
	if e := s.WriteInteraction(exact, &store.Interaction{RunID: "r", ID: "q"}); e != nil {
		t.Fatal(e)
	}
	writers := map[string]func(context.Context) error{
		"artifact": func(c context.Context) error {
			return s.WriteArtifact(c, &store.Artifact{RunID: "r", NodeID: "n", Version: 1, Data: mongoValuePayload()})
		},
		"event": func(c context.Context) error {
			_, e := s.AppendEvent(c, "r", store.Event{Type: "test", Data: mongoValuePayload()})
			return e
		},
		"interaction": func(c context.Context) error {
			return s.WriteInteraction(c, &store.Interaction{RunID: "r", ID: "bad", Questions: mongoValuePayload()})
		},
		"answer": func(c context.Context) error {
			_, e := s.AnswerInteractionCAS(c, "r", "q", mongoValuePayload())
			return e
		},
		"checkpoint": func(c context.Context) error {
			return s.SaveCheckpoint(c, "r", &store.Checkpoint{Vars: mongoValuePayload()})
		},
		"pause": func(c context.Context) error { return s.PauseRun(c, "r", &store.Checkpoint{Vars: mongoValuePayload()}) },
		"fail-resumable": func(c context.Context) error {
			return s.FailRunResumable(c, "r", &store.Checkpoint{Vars: mongoValuePayload()}, "bad", "")
		},
		"fail-terminal": func(c context.Context) error {
			return s.FailRunTerminal(c, "r", &store.Checkpoint{Vars: mongoValuePayload()}, "bad", "")
		},
	}
	snapshot := func() map[string][]bson.Raw {
		out := map[string][]bson.Raw{}
		for _, name := range []string{"runs", "events", "interactions", "run_seq"} {
			cur, e := s.db.Collection(name).Find(exact, bson.M{})
			if e != nil {
				t.Fatal(e)
			}
			var rows []bson.Raw
			if e = cur.All(exact, &rows); e != nil {
				t.Fatal(e)
			}
			out[name] = rows
		}
		return out
	}
	before := snapshot()
	for name, w := range writers {
		t.Run(name, func(t *testing.T) {
			legacy := store.WithIdentity(t.Context(), "format-tenant", "tester")
			if e := w(legacy); !errors.Is(e, store.ErrValueFormatChanged) {
				t.Fatal("unbound writer accepted", e)
			}
			cancelled, cancel := context.WithCancel(exact)
			cancel()
			if e := w(cancelled); !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("refusal changed database")
			}
			vs, e := s.listValueArtifacts(exact, 2, "r", "n")
			if (e != nil && !errors.Is(e, blob.ErrArtifactNotFound)) || len(vs) != 0 {
				t.Fatal(vs, e)
			}
		})
	}
}
func TestValueRecordsMongoOrphansAnswerRaceAndSequence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"event", "interaction", "artifact"} {
		t.Run(kind, func(t *testing.T) {
			s, c := valueFormatMongo(t, true)
			var e error
			switch kind {
			case "event":
				_, e = s.AppendEvent(c, "r", store.Event{Type: "orphan"})
			case "interaction":
				e = s.WriteInteraction(c, &store.Interaction{RunID: "r", ID: "q"})
			case "artifact":
				e = s.WriteArtifact(c, &store.Artifact{RunID: "r", NodeID: "n", Version: 1, Data: map[string]any{"s": "v"}})
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRunWithValueFormat(c, mongoExactBirth("r")); !errors.Is(e, store.ErrInvalidValueFormat) {
				t.Fatal("exact birth accepted legacy child", e)
			}
			n, e := s.runs.CountDocuments(c, bson.M{})
			if e != nil || n != 0 {
				t.Fatal("reservation leaked", n, e)
			}
		})
	}
	s, c := mongoValueStore(t)
	if e := s.WriteInteraction(c, &store.Interaction{RunID: "r", ID: "q"}); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, e := s.AnswerInteractionCAS(c, "r", "q", mongoValuePayload()); errs <- e })
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for e := range errs {
		if e == nil {
			wins++
		} else if errors.Is(e, store.ErrInteractionAlreadyAnswered) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	for i := int64(0); i < 4; i++ {
		if _, e := s.runSeq.DeleteMany(c, bson.M{"_id.run_id": "r"}); e != nil {
			t.Fatal(e)
		}
		ev, e := s.AppendEvent(c, "r", store.Event{Type: "sequence"})
		if e != nil || ev.Seq != i {
			t.Fatal("counter recovery", ev, e)
		}
	}
	for _, f := range []store.ValueFormat{1, 2, 1} {
		if e := s.DeleteRun(c, "r"); e != nil {
			t.Fatal(e)
		}
		if n, e := s.PruneDeletionMarkers(c, time.Now().Add(time.Hour)); e != nil || n != 1 {
			t.Fatal(n, e)
		}
		unbound := store.WithIdentity(t.Context(), "format-tenant", "tester")
		if _, e := s.CreateRunWithValueFormat(unbound, store.RunCreateSpec{ID: "r", Format: f}); e != nil {
			t.Fatal(e)
		}
		c = mongoValueBind(t, unbound, "r", f)
		ev, e := s.AppendEvent(c, "r", store.Event{Type: "fresh"})
		if e != nil || ev.Seq != 0 {
			t.Fatal(ev, e)
		}
	}
}
func TestValueRecordsMongoFutureAndMalformedTerminal(t *testing.T) {
	t.Parallel()
	s, c := mongoValueStore(t)
	if _, e := s.events.InsertOne(c, bson.M{"run_id": "r", "tenant_id": "format-tenant", "seq": 0, "data": bson.M{}}); e != nil {
		t.Fatal(e)
	}
	if es, e := s.LoadEvents(c, "r"); e == nil || len(es) != 0 {
		t.Fatal("missing frame accepted", es, e)
	}
	if _, e := s.runs.UpdateOne(c, bson.M{"_id": "r"}, bson.M{"$set": bson.M{"format_version": 99}}); e != nil {
		t.Fatal(e)
	}
	for _, r := range []func() error{func() error { _, e := s.LoadArtifact(c, "r", "n", 1); return e }, func() error { _, e := s.LoadLatestArtifact(c, "r", "n"); return e }, func() error { _, e := s.ListArtifactVersions(c, "r", "n"); return e }, func() error { _, e := s.LoadEvents(c, "r"); return e }, func() error { _, e := s.LoadInteraction(c, "r", "q"); return e }, func() error { _, e := s.AppendEvent(c, "r", store.Event{}); return e }, func() error { return s.DeleteRun(c, "r") }} {
		if e := r(); !errors.Is(e, store.ErrUnsupportedValueFormat) {
			t.Fatal("future swallowed", e)
		}
	}
}
func TestValueRecordsMongoPRProjection(t *testing.T) {
	t.Parallel()
	s, c := valueFormatMongo(t, true)
	for _, f := range []store.ValueFormat{1, 2} {
		id := fmt.Sprint(f)
		if _, e := s.CreateRunWithValueFormat(c, store.RunCreateSpec{ID: id, Format: f, Inputs: map[string]any{"pr_url": "https://example.test/pr/1"}}); e != nil {
			t.Fatal(e)
		}
	}
	ids, e := s.ListRunsForPR(c, "https://example.test/pr/1")
	if e != nil || len(ids) != 2 {
		t.Fatal(ids, e)
	}
	r, e := s.LoadRun(c, "2")
	if e != nil {
		t.Fatal(e)
	}
	r.Inputs = map[string]any{"pr_url": "https://example.test/pr/2"}
	if e = s.SaveRun(c, r); e != nil {
		t.Fatal(e)
	}
	ids, e = s.ListRunsForPR(c, "https://example.test/pr/1")
	if e != nil || len(ids) != 1 || ids[0] != "1" {
		t.Fatal(ids, e)
	}
	ids, e = s.ListRunsForPR(c, "https://example.test/pr/2")
	if e != nil || len(ids) != 1 || ids[0] != "2" {
		t.Fatal(ids, e)
	}
	r, e = s.LoadRun(c, "2")
	if e != nil {
		t.Fatal(e)
	}
	r.Inputs = nil
	if e = s.SaveRun(c, r); e != nil {
		t.Fatal(e)
	}
	ids, e = s.ListRunsForPR(c, "https://example.test/pr/2")
	if e != nil || len(ids) != 0 {
		t.Fatal("stale projection", ids, e)
	}
}

func TestValueRecordsMongoCheckpointPointerAndNoop(t *testing.T) {
	t.Parallel()
	s, c := mongoValueStore(t)
	cp := &store.Checkpoint{Vars: mongoValuePayload(), InteractionID: "q", InteractionQuestions: mongoValuePayload()}
	if e := s.PauseRun(c, "r", cp); e != nil {
		t.Fatal(e)
	}
	r, e := s.LoadRun(c, "r")
	if e != nil || r.Checkpoint.InteractionID != "q" {
		t.Fatal(r, e)
	}
	requireMongoValuePayload(t, r.Checkpoint.InteractionQuestions)
	if e = s.SaveCheckpoint(c, "r", cp); e != nil {
		t.Fatal(e)
	}
	r, e = s.LoadRun(c, "r")
	if e != nil || r.Checkpoint.InteractionID != "q" {
		t.Fatal(r, e)
	}
	if e = s.UpdateRunStatus(c, "r", store.RunStatusRunning, ""); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveCheckpoint(c, "r", cp); e != nil {
		t.Fatal(e)
	}
	r, e = s.LoadRun(c, "r")
	if e != nil || r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) != 0 {
		t.Fatal(r, e)
	}
	requireMongoValuePayload(t, r.Checkpoint.Vars)
	if cp.InteractionID != "q" {
		t.Fatal("caller checkpoint mutated")
	}
	requireMongoValuePayload(t, cp.InteractionQuestions)
	if e = s.UpdateRunStatus(c, "r", store.RunStatusCancelled, ""); e != nil {
		t.Fatal(e)
	}
	before, e := s.runs.FindOne(c, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FailRunResumable(c, "r", cp, "late", ""); e != nil {
		t.Fatal(e)
	}
	after, e := s.runs.FindOne(c, bson.M{"_id": "r"}).Raw()
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("no-op failure still changed parent", e)
	}
	if e = s.FailRunTerminal(c, "r", cp, "ambiguous", store.FailureAmbiguousEffect); e != nil {
		t.Fatal(e)
	}
	r, e = s.LoadRun(c, "r")
	if e != nil || r.Status != store.RunStatusFailed || r.Checkpoint.InteractionID != "" {
		t.Fatal(r, e)
	}
	requireMongoValuePayload(t, r.Checkpoint.Vars)
}
