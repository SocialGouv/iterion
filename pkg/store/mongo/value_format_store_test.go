package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type valueFormatTestBlob struct {
	*inMemoryBlob
	exact *inMemoryBlob
}

func (b *valueFormatTestBlob) space(f int) (*inMemoryBlob, error) {
	switch f {
	case 1:
		return b.inMemoryBlob, nil
	case 2:
		return b.exact, nil
	default:
		return nil, store.ErrUnsupportedValueFormat
	}
}
func (b *valueFormatTestBlob) PutArtifactForFormat(c context.Context, f int, r, n string, v int, p []byte) error {
	x, e := b.space(f)
	if e != nil {
		return e
	}
	return x.PutArtifact(c, r, n, v, p)
}
func (b *valueFormatTestBlob) GetArtifactForFormat(c context.Context, f int, r, n string, v int) ([]byte, error) {
	x, e := b.space(f)
	if e != nil {
		return nil, e
	}
	return x.GetArtifact(c, r, n, v)
}
func (b *valueFormatTestBlob) ListArtifactVersionsForFormat(c context.Context, f int, r, n string) ([]int, error) {
	x, e := b.space(f)
	if e != nil {
		return nil, e
	}
	return x.ListArtifactVersions(c, r, n)
}
func (b *valueFormatTestBlob) DeleteRunArtifactsForFormat(c context.Context, f int, r string) error {
	x, e := b.space(f)
	if e != nil {
		return e
	}
	return x.DeleteRun(c, r)
}
func valueFormatMongo(t *testing.T, capable bool) (*Store, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("requires Mongo conformance fixture")
	}
	ctx := store.WithIdentity(t.Context(), "format-tenant", "tester")
	cfg := Config{URI: uri, Database: "value_format_" + bsonNonce(t), Blob: newInMemoryBlob(), RunFilesScratchDir: filepath.Join(t.TempDir(), "runfiles")}
	if capable {
		cfg.Blob = &valueFormatTestBlob{newInMemoryBlob(), newInMemoryBlob()}
	}
	s, e := New(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.db.Drop(c)
		_ = s.Close(c)
	})
	return s, ctx
}
func mongoExactBirth(id string) store.RunCreateSpec {
	return store.RunCreateSpec{ID: id, WorkflowName: "wf", FilePath: "main.bot", BotID: "bot", Format: 2, Inputs: map[string]any{"n": json.Number("1e400"), "i": int64(9007199254740993)}}
}
func TestValueFormatMongoBirthAndSave(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	if _, e := s.CreateRun(ctx, "p", "parent", nil); e != nil {
		t.Fatal(e)
	}
	birth := mongoExactBirth("r")
	birth.ParentRunID = "p"
	r, e := store.CreateRunWithValueFormat(ctx, s, birth)
	if e != nil {
		t.Fatal(e)
	}
	if r.ParentRunID != "p" || r.FilePath != "main.bot" || r.BotID != "bot" || r.FormatVersion != 2 || r.Status != store.RunStatusQueued || r.QueuedAt == nil {
		t.Fatal(r)
	}
	raw, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	if raw.Lookup("inputs").Type != bson.TypeArray {
		t.Fatal("missing exact input frame")
	}
	for _, listing := range []bool{false, true} {
		var got *store.Run
		if listing {
			got, e = s.LoadRunForListing(ctx, "r")
		} else {
			got, e = s.LoadRun(ctx, "r")
		}
		if e != nil || got.Inputs["n"] != json.Number("1e400") || got.Inputs["i"] != int64(9007199254740993) {
			t.Fatal(listing, got, e)
		}
	}
	r, e = s.LoadRun(ctx, "r")
	if e != nil {
		t.Fatal(e)
	}
	r.Name = "renamed"
	if e = s.SaveRun(ctx, r); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateRunStatus(ctx, "r", store.RunStatusRunning, ""); e != nil {
		t.Fatal(e)
	}
	r, e = s.LoadRun(ctx, "r")
	if e != nil || r.FormatVersion != 2 || r.Inputs["n"] != json.Number("1e400") {
		t.Fatal(r, e)
	}
	before, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range []int{1, 99} {
		next := *r
		next.FormatVersion = f
		e = s.SaveRun(ctx, &next)
		want := store.ErrValueFormatChanged
		if f == 99 {
			want = store.ErrUnsupportedValueFormat
		}
		if !errors.Is(e, want) {
			t.Fatal(f, e)
		}
		after, e := s.runs.FindOne(ctx, bson.M{"_id": "r"}).Raw()
		if e != nil || !bytes.Equal(before, after) {
			t.Fatal("refusal changed record", e)
		}
	}
	if _, e = s.CreateRunWithValueFormat(ctx, mongoExactBirth("r")); e == nil {
		t.Fatal("duplicate birth accepted")
	}
	if e = s.SaveRun(ctx, &store.Run{ID: "absent", FormatVersion: 2}); !errors.Is(e, store.ErrValueFormatCapability) {
		t.Fatal(e)
	}
	n, e := s.runs.CountDocuments(ctx, bson.M{"_id": "absent"})
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestValueFormatMongoCapabilityAndLegacy(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, false)
	if _, e := s.CreateRunWithValueFormat(ctx, mongoExactBirth("r")); !errors.Is(e, store.ErrValueFormatCapability) {
		t.Fatal(e)
	}
	n, e := s.runs.CountDocuments(ctx, bson.M{})
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e = s.CreateRun(ctx, "legacy", "wf", map[string]any{"i": int32(7)}); e != nil {
		t.Fatal(e)
	}
	r, e := s.LoadRun(ctx, "legacy")
	if e != nil || r.FormatVersion != 1 || r.Inputs["i"] != int64(7) {
		t.Fatal(r, e)
	}
}
func TestValueFormatMongoFutureAndLegacyZero(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	for _, v := range []any{nil, 0, 1} {
		id := fmt.Sprint(v)
		if _, e := s.CreateRun(ctx, id, "wf", nil); e != nil {
			t.Fatal(e)
		}
		update := bson.M{"$set": bson.M{"format_version": v}}
		if v == nil {
			update = bson.M{"$unset": bson.M{"format_version": ""}}
		}
		if _, e := s.runs.UpdateOne(ctx, bson.M{"_id": id}, update); e != nil {
			t.Fatal(e)
		}
		r, e := s.LoadRun(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		old := r.FormatVersion
		r.Name = "metadata"
		if e = s.SaveRun(ctx, r); e != nil {
			t.Fatal(e)
		}
		r, e = s.LoadRun(ctx, id)
		if e != nil || r.FormatVersion != old {
			t.Fatal(r, e)
		}
	}
	r, e := s.CreateRun(ctx, "future", "wf", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.runs.UpdateOne(ctx, bson.M{"_id": "future"}, bson.M{"$set": bson.M{"format_version": 99, "inputs": bson.A{}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.LoadRun(ctx, "future"); !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	if _, e = s.LoadRunForListing(ctx, "future"); !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	if e = s.SaveRun(ctx, r); !errors.Is(e, store.ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
}

// The legacy marshaler is a barrier in a real SaveRun after its pre-image read.
// Exact codecs refuse such callbacks; this tests the old writer's publication.
type valueFormatBSONBarrier struct{ entered, release chan struct{} }

func (b valueFormatBSONBarrier) MarshalBSONValue() (byte, []byte, error) {
	close(b.entered)
	<-b.release
	kind, raw, e := bson.MarshalValue("old")
	return byte(kind), raw, e
}
func TestValueFormatMongoSaveCASFencesReplacement(t *testing.T) {
	t.Parallel()
	s, ctx := valueFormatMongo(t, true)
	other, e := New(ctx, Config{URI: os.Getenv("ITERION_TEST_MONGO_URI"), Database: s.db.Name(), Blob: s.blob})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	old, e := s.CreateRun(ctx, "r", "old", nil)
	if e != nil {
		t.Fatal(e)
	}
	bar := valueFormatBSONBarrier{make(chan struct{}), make(chan struct{})}
	old.Inputs = map[string]any{"gate": bar}
	done := make(chan error, 1)
	go func() { done <- s.SaveRun(ctx, old) }()
	<-bar.entered
	if e = other.DeleteRun(ctx, "r"); e != nil {
		close(bar.release)
		t.Fatal(e)
	}
	if _, e = other.PruneDeletionMarkers(ctx, time.Now().Add(time.Hour)); e != nil {
		close(bar.release)
		t.Fatal(e)
	}
	if _, e = other.CreateRunWithValueFormat(ctx, mongoExactBirth("r")); e != nil {
		close(bar.release)
		t.Fatal(e)
	}
	close(bar.release)
	if e = <-done; !errors.Is(e, store.ErrRunConflict) {
		t.Fatalf("old SaveRun crossed format CAS: %v", e)
	}
	got, e := other.LoadRun(ctx, "r")
	if e != nil || got.FormatVersion != 2 || got.WorkflowName != "wf" || got.Inputs["n"] != json.Number("1e400") {
		t.Fatal(got, e)
	}
}
