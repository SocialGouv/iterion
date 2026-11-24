package recordcodec

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestExactRecordTransportAndAtomicDecode(t *testing.T) {
	p := testPlan(t)
	for _, data := range []map[string]any{nil, {}, {"n": json.Number("9007199254740993.0"), "decimal": json.Number("0.123456789012345678901234567890"), "huge": json.Number("1e400"), "uint": uint64(18446744073709551615)}} {
		src := testRecord{Data: data, Child: &testLeaf{Data: data}, Branches: map[string]*testLeaf{"empty": {Data: map[string]any{}}, "null": nil, "value": {Data: data}}, Turns: []testLeaf{{}, {Data: data}}, Meta: json.RawMessage(`{"n":9007199254740993.0}`)}
		for _, pair := range []struct {
			name string
			enc  func(context.Context, any) ([]byte, error)
			dec  func(context.Context, []byte, any) error
		}{{"json", p.EncodeJSON, p.DecodeJSON}, {"bson", p.EncodeBSON, p.DecodeBSON}} {
			raw, e := pair.enc(t.Context(), src)
			if e != nil {
				t.Fatal(pair.name, e)
			}
			var dst testRecord
			if e = pair.dec(t.Context(), raw, &dst); e != nil || !reflect.DeepEqual(src, dst) {
				t.Fatalf("%s %#v => %#v (%v)", pair.name, src, dst, e)
			}
		}
	}
	// A late malformed field must not publish the metadata/earlier good fields.
	src := testRecord{Data: map[string]any{"ok": true}, Turns: []testLeaf{{}, {}}}
	raw, e := p.EncodeJSON(t.Context(), src)
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]json.RawMessage
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`["iterion.exact-values/999"]`), json.RawMessage(`{}`), json.RawMessage(`["iterion.exact-values/1",[["array",[]]]]`)} {
		if bad == nil {
			doc["turns"] = json.RawMessage(`[{}]`)
		} else {
			entry, _ := json.Marshal(map[string]json.RawMessage{"data": bad})
			doc["turns"] = append(append([]byte{'['}, entry...), ']')
		}
		wire, _ := json.Marshal(doc)
		dst := testRecord{Data: map[string]any{"sentinel": true}}
		err := p.DecodeJSON(t.Context(), wire, &dst)
		if err == nil || dst.Data["sentinel"] != true {
			t.Fatal("partial publish", err)
		}
		if string(bad) == `["iterion.exact-values/999"]` && !errors.Is(err, valuecodec.ErrUnsupportedVersion) {
			t.Fatal(err)
		}
	}
	raw, e = p.EncodeBSON(t.Context(), src)
	if e != nil {
		t.Fatal(e)
	}
	var bd bson.D
	if e = bson.Unmarshal(raw, &bd); e != nil {
		t.Fatal(e)
	}
	for i := range bd {
		if bd[i].Key == "data" {
			bd[i].Value = bson.A{"iterion.exact-values/999"}
		}
	}
	raw, e = bson.Marshal(bd)
	if e != nil {
		t.Fatal(e)
	}
	var dst testRecord
	if e = p.DecodeBSON(t.Context(), raw, &dst); !errors.Is(e, valuecodec.ErrUnsupportedVersion) {
		t.Fatal(e)
	}
}

type callsMetadata struct {
	Calls *int `json:"-" bson:"-"`
}

func (m callsMetadata) MarshalJSON() ([]byte, error) { *m.Calls++; return []byte(`"metadata"`), nil }
func TestExactRecordSharedTreeVisitsMetadataOnce(t *testing.T) {
	type leaf struct {
		Data  map[string]any `json:"data,omitempty"`
		Other map[string]any `json:"other,omitempty"`
		Meta  callsMetadata  `json:"meta"`
	}
	type record struct {
		Branches map[string]leaf `json:"branches"`
	}
	p, e := Compile(reflect.TypeFor[record](), "Branches/*/Data", "Branches/*/Other")
	if e != nil {
		t.Fatal(e)
	}
	for _, count := range []int{1, 100, 500} {
		calls := 0
		src := record{Branches: map[string]leaf{}}
		for i := 0; i < count; i++ {
			src.Branches[string(rune(i+256))] = leaf{Data: map[string]any{"n": json.Number("1e400")}, Meta: callsMetadata{&calls}}
		}
		if _, e = p.EncodeJSON(t.Context(), src); e != nil {
			t.Fatal(e)
		}
		if calls != count {
			t.Fatalf("metadata traversed %d times for %d branches", calls, count)
		}
	}
}

func TestExactRecordEmptyStructuralParents(t *testing.T) {
	p := testPlan(t)
	src := testRecord{Branches: map[string]*testLeaf{}, Turns: []testLeaf{}}
	for _, pair := range []struct {
		enc func(context.Context, any) ([]byte, error)
		dec func(context.Context, []byte, any) error
	}{{p.EncodeJSON, p.DecodeJSON}, {p.EncodeBSON, p.DecodeBSON}} {
		raw, e := pair.enc(t.Context(), src)
		if e != nil {
			t.Fatal(e)
		}
		var got testRecord
		if e = pair.dec(t.Context(), raw, &got); e != nil || !reflect.DeepEqual(got, src) {
			t.Fatalf("empty structural container lost: %#v %v", got, e)
		}
	}
}

type cancelsMetadata struct{ Cancel context.CancelFunc }

func (m *cancelsMetadata) MarshalJSON() ([]byte, error) { m.Cancel(); return []byte(`"canceled"`), nil }
func TestExactRecordMetadataCodecsAndFinalCancellation(t *testing.T) {
	type record struct {
		Data  map[string]any
		Meta  cancelsMetadata
		Count int `json:"count,string"`
	}
	p, e := Compile(reflect.TypeFor[record](), "Data")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, e = p.EncodeJSON(ctx, record{Meta: cancelsMetadata{cancel}, Count: 7}); !errors.Is(e, context.Canceled) {
		t.Fatal("pointer metadata codec or final cancellation lost", e)
	}
}

type recordKey string
type recordPayload map[recordKey]any
type recordRows map[string]recordPayload

func TestExactRecordNamedPayloadAndStructuralArray(t *testing.T) {
	type child struct {
		Data recordRows `json:"data" bson:"data"`
	}
	type record struct {
		Children [2]child `json:"children" bson:"children"`
	}
	p, e := Compile(reflect.TypeFor[record](), "Children/[]/Data")
	if e != nil {
		t.Fatal(e)
	}
	src := record{Children: [2]child{{Data: recordRows{"r": recordPayload{"n": json.Number("1e400")}}}, {Data: recordRows{}}}}
	for _, pair := range []struct {
		enc func(context.Context, any) ([]byte, error)
		dec func(context.Context, []byte, any) error
	}{{p.EncodeJSON, p.DecodeJSON}, {p.EncodeBSON, p.DecodeBSON}} {
		raw, e := pair.enc(t.Context(), src)
		if e != nil {
			t.Fatal(e)
		}
		var got record
		if e = pair.dec(t.Context(), raw, &got); e != nil || !reflect.DeepEqual(src, got) {
			t.Fatal(e, got)
		}
	}
	// A valid leaf value is still invalid for the declared map-of-map payload.
	frame, e := valuecodec.EncodeMap(t.Context(), map[string]any{"r": []any{1}})
	if e != nil {
		t.Fatal(e)
	}
	wire, e := json.Marshal(map[string]any{"children": []any{map[string]any{"data": frame}, map[string]any{"data": frame}}})
	if e != nil {
		t.Fatal(e)
	}
	var dst record
	if e = p.DecodeJSON(t.Context(), wire, &dst); e == nil {
		t.Fatal("wrong nested payload root accepted")
	}
}

func TestExactRecordStructuralArrayOmission(t *testing.T) {
	type child struct {
		Data map[string]any `json:"data" bson:"data"`
	}
	type zeroOmitted struct {
		Rows [0]child `json:"rows,omitempty" bson:"rows,omitempty"`
	}
	type zeroPresent struct {
		Rows [0]child `json:"rows" bson:"rows"`
	}
	type oneOmitted struct {
		Rows [1]child `json:"rows,omitempty" bson:"rows,omitempty"`
	}
	for _, src := range []any{zeroOmitted{}, zeroPresent{}, oneOmitted{}, oneOmitted{Rows: [1]child{{Data: map[string]any{"n": json.Number("1e400")}}}}} {
		p, err := Compile(reflect.TypeOf(src), "Rows/[]/Data")
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range []struct {
			name string
			enc  func(context.Context, any) ([]byte, error)
			dec  func(context.Context, []byte, any) error
		}{{"json", p.EncodeJSON, p.DecodeJSON}, {"bson", p.EncodeBSON, p.DecodeBSON}} {
			raw, err := pair.enc(t.Context(), src)
			if err != nil {
				t.Fatalf("%s structural array %T rejected: %v", pair.name, src, err)
			}
			dst := reflect.New(reflect.TypeOf(src))
			if err = pair.dec(t.Context(), raw, dst.Interface()); err != nil || !reflect.DeepEqual(src, dst.Elem().Interface()) {
				t.Fatalf("%s structural array %T changed: %#v %v", pair.name, src, dst.Elem().Interface(), err)
			}
		}
	}
}
