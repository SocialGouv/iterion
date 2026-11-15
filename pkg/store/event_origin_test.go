package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func originFixture(t *testing.T) (Event, RunObservation) {
	t.Helper()
	o, err := CaptureRunObservation(RunDataHeader{RunID: "r", BirthID: "a", Format: 1}, "tenant", true)
	if err != nil {
		t.Fatal(err)
	}
	e, err := WithEventObservation(Event{RunID: "r", Type: EventNodeStarted, Data: map[string]any{"n": float64(7)}}, o)
	if err != nil {
		t.Fatal(err)
	}
	return e, o
}

func TestEventOriginCopiesAndAbsence(t *testing.T) {
	t.Parallel()
	e, o := originFixture(t)
	copy := e
	got, ok := copy.EventObservation()
	if !ok || got != o {
		t.Fatal("value copy lost original")
	}
	header := got.Header()
	header.BirthID = "b"
	if got.Header().BirthID != "a" {
		t.Fatal("header accessor aliases original")
	}
	absent, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 1}, "tenant", false)
	if err != nil || !absent.Observed() || absent.ParentPresent() {
		t.Fatal(absent, err)
	}
	present, err := CaptureRunObservation(absent.Header(), "tenant", true)
	if err != nil || present == absent || absent == (RunObservation{}) {
		t.Fatal("presence collapsed", err)
	}
	if _, err := CaptureRunObservation(o.Header(), "tenant", false); !errors.Is(err, ErrInvalidRunDataIdentity) {
		t.Fatal("absent birth accepted", err)
	}
	e.RunID = "other"
	if _, err := WithEventObservation(e, o); !errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatal("different run accepted", err)
	}
	if _, err := WithEventObservation(e, RunObservation{}); !errors.Is(err, ErrRunDataBindingRequired) {
		t.Fatal("unknown original accepted", err)
	}
}

func TestEventOriginOwnedJSONDecodeResetsDestination(t *testing.T) {
	t.Parallel()
	for _, format := range []ValueFormat{1, 2} {
		for _, stamped := range []bool{false, true} {
			e, _ := originFixture(t)
			plain := e
			plain.observation = RunObservation{}
			raw, err := EncodeValueRecordJSON(t.Context(), format, ExactEvent, e)
			if err != nil {
				t.Fatal(err)
			}
			base, err := EncodeValueRecordJSON(t.Context(), format, ExactEvent, plain)
			if err != nil || !bytes.Equal(raw, base) {
				t.Fatal("origin entered JSON", err)
			}
			var dst Event
			if stamped {
				dst = e
			}
			if err := DecodeValueRecordJSON(t.Context(), format, ExactEvent, raw, &dst); err != nil {
				t.Fatal(err)
			}
			if _, ok := dst.EventObservation(); ok {
				t.Fatalf("format %d retained old observation", format)
			}
			if dst.RunID != "r" || dst.Data["n"] == nil {
				t.Fatal("payload lost")
			}
		}
	}
	var generic map[string]any
	if err := DecodeValueRecordJSON(t.Context(), 1, ExactEvent, []byte(`{"seq":3}`), &generic); err != nil || generic["seq"] != float64(3) {
		t.Fatal("legacy generic destination changed", err)
	}
}

func TestEventOriginExactBSONAndLargeNumber(t *testing.T) {
	t.Parallel()
	e, _ := originFixture(t)
	e.Data["n"] = json.Number("1e400")
	codec, err := ExactRecordCodec(ExactEvent)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := codec.EncodeBSON(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	dst := e
	if err := codec.DecodeBSON(t.Context(), raw, &dst); err != nil {
		t.Fatal(err)
	}
	if _, ok := dst.EventObservation(); ok {
		t.Fatal("exact BSON retained origin")
	}
	if dst.Data["n"] != json.Number("1e400") {
		t.Fatalf("exact number changed: %#v", dst.Data["n"])
	}
	before := dst
	if err := codec.DecodeBSON(t.Context(), []byte("bad"), &dst); err == nil {
		t.Fatal("invalid BSON accepted")
	}
	if dst.RunID != before.RunID || dst.Data["n"] != before.Data["n"] {
		t.Fatal("failed decode published partial event")
	}
}
