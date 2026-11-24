package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"reflect"
	"testing"
)

func independentMongoOrigin(t *testing.T) store.Event {
	t.Helper()
	o, e := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", BirthID: "old", Format: 1}, "tenant", true)
	if e != nil {
		t.Fatal(e)
	}
	v, e := store.WithEventObservation(store.Event{RunID: "r", Seq: 10, Data: map[string]any{"kept": "old"}}, o)
	if e != nil {
		t.Fatal(e)
	}
	return v
}

type independentMongoCancel struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func (c *independentMongoCancel) Err() error {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestIndependentEventBSONAtomicity(t *testing.T) {
	t.Parallel()
	raw, e := bson.Marshal(bson.D{{Key: "run_id", Value: "replacement"}, {Key: "seq", Value: "bad"}})
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range []store.ValueFormat{1, 2} {
		before := independentMongoOrigin(t)
		dst := before
		if e := decodeValueBSON(t.Context(), f, store.ExactEvent, raw, &dst); e == nil {
			t.Fatal("bad BSON accepted")
		}
		if !reflect.DeepEqual(dst, before) {
			t.Fatalf("format%d partial decode assigned %#v", f, dst)
		}
	}
	valid, e := bson.Marshal(bson.D{{Key: "run_id", Value: "replacement"}, {Key: "seq", Value: int64(9)}})
	if e != nil {
		t.Fatal(e)
	}
	before := independentMongoOrigin(t)
	dst := before
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	boundary := &independentMongoCancel{Context: ctx, cancel: cancel}
	e = decodeValueBSON(boundary, 1, store.ExactEvent, valid, &dst)
	if !errors.Is(e, context.Canceled) || boundary.calls != 2 || !reflect.DeepEqual(dst, before) {
		t.Fatalf("cancel published: %v calls%d %#v", e, boundary.calls, dst)
	}
}
func TestIndependentEventBSONResetAndWire(t *testing.T) {
	t.Parallel()
	for _, f := range []store.ValueFormat{1, 2} {
		stamped := independentMongoOrigin(t)
		plain := store.Event{RunID: "r", Seq: 10, Data: map[string]any{"kept": "old"}}
		a, e := store.EncodeValueRecordBSON(t.Context(), f, store.ExactEvent, stamped)
		if e != nil {
			t.Fatal(e)
		}
		b, e := store.EncodeValueRecordBSON(t.Context(), f, store.ExactEvent, plain)
		if e != nil || !bytes.Equal(a, b) {
			t.Fatal("origin changed BSON wire", e)
		}
		value := store.Event{RunID: "new", Seq: 23, Data: map[string]any{"n": int64(7)}}
		if f == 2 {
			value.Data["n"] = json.Number("1e400")
		}
		raw, e := store.EncodeValueRecordBSON(t.Context(), f, store.ExactEvent, value)
		if e != nil {
			t.Fatal(e)
		}
		dst := stamped
		if e := decodeValueBSON(t.Context(), f, store.ExactEvent, raw, &dst); e != nil {
			t.Fatal(e)
		}
		if _, ok := dst.EventObservation(); ok {
			t.Fatal("BSON retained old origin")
		}
		if dst.RunID != "new" || dst.Seq != 23 {
			t.Fatal("new payload not assigned")
		}
		if f == 2 && dst.Data["n"] != json.Number("1e400") {
			t.Fatal("exact value changed")
		}
	}
	raw, e := bson.Marshal(bson.D{{Key: "seq", Value: int64(3)}})
	if e != nil {
		t.Fatal(e)
	}
	var got, want bson.M
	if e := decodeValueBSON(t.Context(), 1, store.ExactEvent, raw, &got); e != nil {
		t.Fatal(e)
	}
	dec := bson.NewDecoder(bson.NewDocumentReader(bytes.NewReader(raw)))
	dec.SetRegistry(Registry())
	if e := dec.Decode(&want); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("generic BSON registry changed")
	}
	var nilEvent *store.Event
	if e := decodeValueBSON(t.Context(), 1, store.ExactEvent, raw, nilEvent); e == nil {
		t.Fatal("typed nil accepted")
	}
	if e := decodeValueBSON(t.Context(), 1, store.ExactEvent, raw, nil); e == nil {
		t.Fatal("nil accepted")
	}
}
