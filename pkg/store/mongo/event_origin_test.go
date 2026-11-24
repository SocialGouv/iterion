package mongo

import (
	"bytes"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestEventOriginOwnedBSONDecodeResetsDestination(t *testing.T) {
	t.Parallel()
	o, err := store.CaptureRunObservation(store.RunDataHeader{RunID: "r", BirthID: "a", Format: 1}, "tenant", true)
	if err != nil {
		t.Fatal(err)
	}
	plain := store.Event{RunID: "r", Type: store.EventNodeStarted, Data: map[string]any{"n": int64(7)}}
	e, err := store.WithEventObservation(plain, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []store.ValueFormat{1, 2} {
		raw, err := store.EncodeValueRecordBSON(t.Context(), format, store.ExactEvent, e)
		if err != nil {
			t.Fatal(err)
		}
		base, err := store.EncodeValueRecordBSON(t.Context(), format, store.ExactEvent, plain)
		if err != nil || !bytes.Equal(raw, base) {
			t.Fatal("origin entered BSON", err)
		}
		for _, stamped := range []bool{false, true} {
			var dst store.Event
			if stamped {
				dst = e
			}
			if err := decodeValueBSON(t.Context(), format, store.ExactEvent, raw, &dst); err != nil {
				t.Fatal(err)
			}
			if _, ok := dst.EventObservation(); ok {
				t.Fatalf("format %d retained old origin", format)
			}
			if dst.RunID != "r" || dst.Data["n"] == nil {
				t.Fatal("payload lost")
			}
		}
	}
	raw, err := bson.Marshal(bson.M{"seq": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	var generic bson.M
	if err := decodeValueBSON(t.Context(), 1, store.ExactEvent, raw, &generic); err != nil || generic["seq"] != int64(3) {
		t.Fatal("legacy generic destination changed", generic, err)
	}
}
