package store

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRunDataHeaderParsersAgreeWithoutDecodingPayloads(t *testing.T) {
	for _, tc := range []struct {
		name, json string
		doc        bson.D
		want       RunDataHeader
	}{
		{name: "historical absence", json: `{"id":"r","méta":{"format_verſion":2},"inputs":{"clé":"été","n":1e400}}`, doc: bson.D{{Key: "_id", Value: "r"}}, want: RunDataHeader{RunID: "r", Format: LegacyValueFormat}},
		{name: "historical with birth", json: `{"id":"r","format_version":2,"birth_id":"b","inputs":{"n":1e400}}`, doc: bson.D{{Key: "_id", Value: "r"}, {Key: "format_version", Value: int64(2)}, {Key: "birth_id", Value: "b"}}, want: RunDataHeader{RunID: "r", Format: ExactValueFormat, BirthID: "b"}},
		{name: "incarnation legacy numbers", json: `{"id":"r","data_protocol":1,"birth_id":"b","inputs":{"n":1e400}}`, doc: bson.D{{Key: "_id", Value: "r"}, {Key: "data_protocol", Value: int64(1)}, {Key: "birth_id", Value: "b"}}, want: RunDataHeader{RunID: "r", Format: LegacyValueFormat, Protocol: IncarnationRunData, BirthID: "b"}},
		{name: "incarnation exact numbers", json: `{"id":"r","format_version":2,"data_protocol":1,"birth_id":"b"}`, doc: bson.D{{Key: "_id", Value: "r"}, {Key: "format_version", Value: int32(2)}, {Key: "data_protocol", Value: int32(1)}, {Key: "birth_id", Value: "b"}}, want: RunDataHeader{RunID: "r", Format: ExactValueFormat, Protocol: IncarnationRunData, BirthID: "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RunDataHeaderFromJSON(t.Context(), []byte(tc.json))
			if err != nil || got != tc.want {
				t.Fatalf("JSON got=%+v err=%v", got, err)
			}
			raw, err := bson.Marshal(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			got, err = RunDataHeaderFromBSON(t.Context(), raw)
			if err != nil || got != tc.want {
				t.Fatalf("BSON got=%+v err=%v", got, err)
			}
		})
	}
}

func TestRunDataHeaderJSONRefusesAmbiguousSelectors(t *testing.T) {
	for _, raw := range []string{
		`{}`, `[]`, `{"id":"r"} {"id":"s"}`, `{"id":"r","id":"s"}`, `{"id":"r","ID":"s"}`,
		`{"id":"r","birth_id":null}`, `{"id":"r","birth_id":12}`, `{"id":"r","birth_id":"a","birth_id":"b"}`,
		`{"id":"r","data_protocol":null}`, `{"id":"r","data_protocol":1.0}`, `{"id":"r","data_protocol":"1"}`,
		`{"id":"r","data_protocol":0,"data_protocol":1}`, `{"id":"r","DATA_PROTOCOL":0}`,
		`{"id":"r","data_protocol":1}`, `{"id":"r","data_protocol":1,"birth_id":""}`,
		`{"id":"r","format_verſion":2}`, `{"id":"r","format_version":1,"format_verſion":2}`,
		`{"id":"r","format_version":null}`, `{"id":"r","format_version":1,"format_version":2}`,
	} {
		if h, err := RunDataHeaderFromJSON(t.Context(), []byte(raw)); err == nil {
			t.Errorf("accepted %s as %+v", raw, h)
		}
	}
	for _, raw := range []string{`{"id":"r","data_protocol":2}`, `{"id":"r","data_protocol":-1}`} {
		if _, err := RunDataHeaderFromJSON(t.Context(), []byte(raw)); !errors.Is(err, ErrUnsupportedRunDataProtocol) {
			t.Errorf("unknown protocol: %v", err)
		}
	}
}

func TestRunDataHeaderBSONRefusesAmbiguousSelectors(t *testing.T) {
	for _, fields := range []bson.D{
		{{Key: "_id", Value: "s"}}, {{Key: "_ID", Value: "s"}}, {{Key: "_İD", Value: "s"}}, {{Key: "BİRTH_ID", Value: "other"}}, {{Key: "format_versİon", Value: 2}},
		{{Key: "birth_id", Value: nil}}, {{Key: "birth_id", Value: 12}}, {{Key: "birth_id", Value: "a"}, {Key: "birth_id", Value: "b"}},
		{{Key: "data_protocol", Value: nil}}, {{Key: "data_protocol", Value: float64(1)}}, {{Key: "data_protocol", Value: "1"}},
		{{Key: "data_protocol", Value: 0}, {Key: "data_protocol", Value: 1}}, {{Key: "DATA_PROTOCOL", Value: 0}},
		{{Key: "data_protocol", Value: 1}}, {{Key: "data_protocol", Value: 1}, {Key: "birth_id", Value: ""}},
		{{Key: "format_version", Value: nil}}, {{Key: "format_version", Value: 1}, {Key: "format_version", Value: 2}},
	} {
		doc := append(bson.D{{Key: "_id", Value: "r"}}, fields...)
		raw, err := bson.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if h, err := RunDataHeaderFromBSON(t.Context(), raw); err == nil {
			t.Errorf("accepted %v as %+v", doc, h)
		}
	}
	for _, v := range []int64{-1, 2, 1 << 40} {
		raw, err := bson.Marshal(bson.D{{Key: "_id", Value: "r"}, {Key: "data_protocol", Value: v}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := RunDataHeaderFromBSON(t.Context(), raw); !errors.Is(err, ErrUnsupportedRunDataProtocol) {
			t.Errorf("unknown protocol %d: %v", v, err)
		}
	}
}

func TestRunDataHeaderParsingHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RunDataHeaderFromJSON(ctx, []byte(`{"id":"r"}`)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	raw, err := bson.Marshal(bson.D{{Key: "_id", Value: "r"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunDataHeaderFromBSON(ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
