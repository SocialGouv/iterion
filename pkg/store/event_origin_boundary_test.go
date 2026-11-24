package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func independentOriginEvent(t *testing.T) Event {
	t.Helper()
	o, err := CaptureRunObservation(RunDataHeader{RunID: "r", BirthID: "original", Format: 1}, "tenant", true)
	if err != nil {
		t.Fatal(err)
	}
	e, err := WithEventObservation(Event{RunID: "r", Seq: 11, Data: map[string]any{"kept": "original"}}, o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type independentCancelOnSecondErr struct {
	context.Context
	cancel context.CancelFunc
	calls  int
}

func (c *independentCancelOnSecondErr) Err() error {
	c.calls++
	if c.calls == 2 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestIndependentEventJSONAtomicity(t *testing.T) {
	t.Parallel()
	for _, format := range []ValueFormat{1, 2} {
		before := independentOriginEvent(t)
		dst := before
		if err := DecodeValueRecordJSON(t.Context(), format, ExactEvent, []byte(`{"run_id":"replacement","seq":"bad"}`), &dst); err == nil {
			t.Fatal("malformed event accepted")
		}
		if !reflect.DeepEqual(dst, before) {
			t.Fatalf("format%d failed decode changed destination: %#v", format, dst)
		}
	}
	before := independentOriginEvent(t)
	dst := before
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	boundary := &independentCancelOnSecondErr{Context: ctx, cancel: cancel}
	err := DecodeValueRecordJSON(boundary, 1, ExactEvent, []byte(`{"run_id":"replacement","seq":9}`), &dst)
	if !errors.Is(err, context.Canceled) || boundary.calls != 2 || !reflect.DeepEqual(dst, before) {
		t.Fatalf("postdecode cancellation published: err=%v calls=%d dst=%#v", err, boundary.calls, dst)
	}
}

func TestIndependentEventJSONResetAndWire(t *testing.T) {
	t.Parallel()
	for _, format := range []ValueFormat{1, 2} {
		stamped := independentOriginEvent(t)
		payload := Event{RunID: "other", Seq: 22, Data: map[string]any{"n": json.Number("7")}}
		if format == 2 {
			payload.Data["n"] = json.Number("1e400")
		}
		raw, err := EncodeValueRecordJSON(t.Context(), format, ExactEvent, payload)
		if err != nil {
			t.Fatal(err)
		}
		dst := stamped
		if err := DecodeValueRecordJSON(t.Context(), format, ExactEvent, raw, &dst); err != nil {
			t.Fatal(err)
		}
		if _, ok := dst.EventObservation(); ok {
			t.Fatal("owned decode retained old origin")
		}
		if dst.RunID != "other" || dst.Seq != 22 {
			t.Fatal("decode did not assign new payload")
		}
		if format == 2 && dst.Data["n"] != json.Number("1e400") {
			t.Fatalf("exact number changed: %#v", dst.Data)
		}
		plain := stamped
		plain.observation = RunObservation{}
		a, err := EncodeValueRecordJSON(t.Context(), format, ExactEvent, stamped)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeValueRecordJSON(t.Context(), format, ExactEvent, plain)
		if err != nil || !bytes.Equal(a, b) {
			t.Fatal("origin changed wire", err)
		}
	}
	var got, want map[string]any
	raw := []byte(`{"seq":3}`)
	a := DecodeValueRecordJSON(t.Context(), 1, ExactEvent, raw, &got)
	b := json.Unmarshal(raw, &want)
	if a != nil || b != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("generic destination changed")
	}
	var nilEvent *Event
	if err := DecodeValueRecordJSON(t.Context(), 1, ExactEvent, raw, nilEvent); err == nil {
		t.Fatal("typed nil accepted")
	}
	if err := DecodeValueRecordJSON(t.Context(), 1, ExactEvent, raw, nil); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestIndependentEventPropagationCharacterization(t *testing.T) {
	t.Parallel()
	a := independentOriginEvent(t)
	copy := a
	oa, _ := a.EventObservation()
	oc, _ := copy.EventObservation()
	if oa != oc {
		t.Fatal("copy lost origin")
	}
	b, err := CaptureRunObservation(RunDataHeader{RunID: "r", BirthID: "successor", Format: 1}, "tenant", true)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := WithEventObservation(a, b)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := replaced.EventObservation()
	if !ok || got != b {
		t.Fatal("authoritative replacement not preserved")
	}
	original, _ := a.EventObservation()
	if original != oa {
		t.Fatal("replacement mutated input value")
	}
	t.Log("same-ID observation replacement accepted; caller must supply authoritative committed origin")
}

func TestIndependentEventAbsentExactCharacterization(t *testing.T) {
	t.Parallel()
	o, err := CaptureRunObservation(RunDataHeader{RunID: "r", Format: 2, Protocol: HistoricalRunData}, "tenant", false)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Observed() || o.ParentPresent() || o.Header().Format != 2 || o.Header().BirthID != "" {
		t.Fatal("absent exact observation changed")
	}
	t.Log("constructor accepts absent historical format2 without birth; not evidence that stores select exact format for missing metadata")
}
