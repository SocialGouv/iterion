package store

import (
	"encoding/json"
	"errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

func TestRunFormatHeadersBeforePayload(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"format_version":0}`, `{"format_version":1}`} {
		if f, e := RunValueFormatFromJSON(t.Context(), []byte(raw)); e != nil || f != LegacyValueFormat {
			t.Fatal(raw, f, e)
		}
	}
	if f, e := RunValueFormatFromJSON(t.Context(), []byte(`{"format_version":2,"inputs":{"n":1e400}}`)); e != nil || f != 2 {
		t.Fatal(f, e)
	}
	for _, raw := range []string{`{"format_version":1,"format_version":2}`, `{"FORMAT_VERSION":2}`, `{"format_version":null}`, `{"format_version":2.0}`, `[]`} {
		if _, e := RunValueFormatFromJSON(t.Context(), []byte(raw)); !errors.Is(e, ErrInvalidValueFormat) {
			t.Fatal(raw, e)
		}
	}
	if _, e := RunValueFormatFromJSON(t.Context(), []byte(`{"format_version":99,"inputs":[]}`)); !errors.Is(e, ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	for _, v := range []any{int32(2), int64(2)} {
		raw, e := bson.Marshal(bson.D{{Key: "format_version", Value: v}})
		if e != nil {
			t.Fatal(e)
		}
		if f, e := RunValueFormatFromBSON(t.Context(), raw); e != nil || f != 2 {
			t.Fatal(f, e)
		}
	}
	for _, d := range []bson.D{{{Key: "FORMAT_VERSION", Value: 2}}, {{Key: "format_versİon", Value: 2}}, {{Key: "format_verſion", Value: 2}}, {{Key: "format_version", Value: 1}, {Key: "FORMAT_VERSION", Value: 2}}, {{Key: "format_version", Value: 1}, {Key: "format_version", Value: 2}}, {{Key: "format_version", Value: 2.0}}, {{Key: "format_version", Value: nil}}} {
		raw, e := bson.Marshal(d)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = RunValueFormatFromBSON(t.Context(), raw); !errors.Is(e, ErrInvalidValueFormat) {
			t.Fatal(d, e)
		}
	}
}
func TestRunFormatCodecPreservesExactAndLegacy(t *testing.T) {
	t.Parallel()
	r := &Run{ID: "r", FormatVersion: 2, Inputs: map[string]any{"n": json.Number("1e400"), "i": int64(9007199254740993)}}
	raw, e := EncodeValueRecordJSON(t.Context(), 2, ExactRun, r)
	if e != nil {
		t.Fatal(e)
	}
	var out Run
	if e = DecodeValueRecordJSON(t.Context(), 2, ExactRun, raw, &out); e != nil {
		t.Fatal(e)
	}
	if out.Inputs["n"] != json.Number("1e400") || out.Inputs["i"] != int64(9007199254740993) {
		t.Fatal(out.Inputs)
	}
	raw, e = EncodeValueRecordBSON(t.Context(), 2, ExactRun, r)
	if e != nil {
		t.Fatal(e)
	}
	codec, e := ExactRecordCodec(ExactRun)
	if e != nil {
		t.Fatal(e)
	}
	if e = codec.DecodeBSON(t.Context(), raw, &out); e != nil {
		t.Fatal(e)
	}
	if out.Inputs["n"] != json.Number("1e400") || out.Inputs["i"] != int64(9007199254740993) {
		t.Fatal(out.Inputs)
	}
	legacy := &Run{ID: "r", FormatVersion: 1, Inputs: map[string]any{"i": int64(9007199254740993)}}
	raw, e = EncodeValueRecordJSON(t.Context(), 1, ExactRun, legacy)
	if e != nil {
		t.Fatal(e)
	}
	var got Run
	if e = DecodeValueRecordJSON(t.Context(), 1, ExactRun, raw, &got); e != nil {
		t.Fatal(e)
	}
	if got.Inputs["i"] != float64(9007199254740992) {
		t.Fatal("legacy contract changed", got.Inputs)
	}
}
