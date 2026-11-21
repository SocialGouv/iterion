package valuecodec_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type namedString string
type namedByte uint8
type namedInt int64
type customValue struct{ Calls *int }

func (v customValue) MarshalJSON() ([]byte, error) { *v.Calls++; return []byte(`{}`), nil }

type customKey string

func (k customKey) MarshalText() ([]byte, error) { panic("custom key must not execute") }

func TestExactFrameNumericAndTransportTwins(t *testing.T) {
	value := map[string]any{"numbers": []any{int(-3), int8(-128), int16(-32768), int32(math.MinInt32), int64(math.MinInt64), uint(3), uint8(255), uint16(65535), uint32(math.MaxUint32), uint64(math.MaxUint64), uintptr(5), float32(.125), float64(.1), json.Number("9007199254740993.0"), json.Number("0.100000000000000005"), json.Number("1e400"), json.Number("-0"), json.Number("2.00")}, "bytes": []byte{0xff, 0, 0x80}, "emptyBytes": []byte{}, "text": "9007199254740993.0", "user": map[string]any{"iterion.exact-values/1": "format", "nodes": []any{nil, true, "x"}, "a/b~c": map[string]any{"0": json.Number("2")}}}
	frame, err := valuecodec.EncodeMap(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, frame any) {
		t.Helper()
		got, err := valuecodec.DecodeMap(t.Context(), frame)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, value) {
			t.Fatalf("roundtrip got=%#v want=%#v", got, value)
		}
	}
	check(t, frame)
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	check(t, decoded)
	exact, err := valuecodec.DecodeJSON(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	check(t, exact)
	bsonBytes, err := bson.Marshal(bson.M{"data": frame})
	if err != nil {
		t.Fatal(err)
	}
	var native bson.M
	if err := bson.Unmarshal(bsonBytes, &native); err != nil {
		t.Fatal(err)
	}
	check(t, native["data"])
	// The barrier is the owned map's shape, not an object magic key.
	var old struct {
		Data map[string]any `json:"data" bson:"data"`
	}
	oldJSON, err := json.Marshal(map[string]any{"data": frame})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(oldJSON, &old); err == nil {
		t.Fatal("old map JSON reader accepted frame")
	}
	if err := bson.Unmarshal(bsonBytes, &old); err == nil {
		t.Fatal("old map BSON reader accepted frame")
	}
	for _, n := range []any{math.Copysign(0, -1), float32(math.Copysign(0, -1))} {
		f, err := valuecodec.EncodeValue(t.Context(), n)
		if err != nil {
			t.Fatal(err)
		}
		v, err := valuecodec.DecodeValue(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		if !math.Signbit(reflect.ValueOf(v).Float()) {
			t.Fatal("negative float zero lost")
		}
	}
}

func TestExactFrameCanonicalGoldenAndReindex(t *testing.T) {
	value := map[string]any{"\U00010000": "astral", "\ue000": "<&>\u2028\u2029\x00\b\t\n\f\r\\\"é"}
	got, err := valuecodec.CanonicalJSON(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	want := `["iterion.exact-values/1",[["object",[["",1],["𐀀",2]]],["string","<&>\u2028\u2029\u0000\b\t\n\f\r\\\"é"],["string","astral"]]]`
	if string(got) != want {
		t.Fatalf("canonical bytes=%s want=%s", got, want)
	}
	a := `["iterion.exact-values/1",[["object",[["a",1],["b",2]]],["object",[["n",4]]],["object",[["n",3]]],["string","text"],["number","9007199254740993.0"]]]`
	b := `["iterion.exact-values/1",[["object",[["b",2],["a",1]]],["object",[["n",3]]],["object",[["n",4]]],["number","9007199254740993.0"],["string","text"]]]`
	want = `["iterion.exact-values/1",[["object",[["a",1],["b",3]]],["object",[["n",2]]],["number","9007199254740993.0"],["object",[["n",4]]],["string","text"]]]`
	for _, wire := range []string{a, b} {
		v, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(wire))
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := valuecodec.CanonicalJSON(t.Context(), v)
		if err != nil {
			t.Fatal(err)
		}
		if string(canonical) != want {
			t.Fatalf("numbering/order not normalized: %s", canonical)
		}
	}
	distinct := []any{json.Number("-0"), math.Copysign(0, -1), int(0), json.Number("2.00"), json.Number("2"), nil, []any{}, map[string]any{}, []byte{}}
	seen := map[string]bool{}
	for _, value := range distinct {
		encoded, err := valuecodec.CanonicalJSON(t.Context(), value)
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(encoded)] {
			t.Fatalf("distinct representation collapsed: %T %s", value, encoded)
		}
		seen[string(encoded)] = true
	}
}

func TestExactFrameNativeTreesAliasesAndCycles(t *testing.T) {
	leaf := map[string]any{"n": json.Number("9007199254740993.0")}
	str := namedString("é")
	value := map[string]any{"a": leaf, "b": leaf, "map": map[namedInt][]namedString{7: {"x", "y"}}, "p": &str, "bytes": []namedByte{0xff, 0x80}, "empty": []any{}, "nil": []any(nil)}
	frame, err := valuecodec.EncodeMap(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := valuecodec.DecodeMap(t.Context(), frame)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["p"] != "é" || !reflect.DeepEqual(decoded["map"], map[string]any{"7": []any{"x", "y"}}) || !reflect.DeepEqual(decoded["bytes"], []byte{0xff, 0x80}) || decoded["nil"] != nil {
		t.Fatalf("native normalization=%#v", decoded)
	}
	decoded["a"].(map[string]any)["n"] = "changed"
	if decoded["b"].(map[string]any)["n"] != json.Number("9007199254740993.0") || leaf["n"] != json.Number("9007199254740993.0") {
		t.Fatal("shared subtree or input aliased")
	}
	cycleMap := map[string]any{}
	cycleMap["self"] = cycleMap
	cycleSlice := make([]any, 1)
	cycleSlice[0] = cycleSlice
	var cyclePointer any
	cyclePointer = &cyclePointer
	calls := 0
	invalid := []any{cycleMap, cycleSlice, cyclePointer, customValue{&calls}, map[customKey]int{"k": 1}, struct{ N int }{1}, math.Inf(1), math.NaN(), json.Number("01"), json.Number("1 "), string([]byte{0xff}), map[string]any{string([]byte{0xff}): 1}, json.RawMessage(`{"n":1}`)}
	for i, v := range invalid {
		if got, err := valuecodec.EncodeValue(t.Context(), v); got != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
			t.Fatalf("case%d %T accepted=%v err=%v", i, v, got, err)
		}
	}
	if calls != 0 {
		t.Fatal("custom callback invoked")
	}
	// A seen set keyed only by the first element would skip the invalid longer alias.
	shared := []any{"valid", string([]byte{0xff})}
	for _, v := range []any{[]any{shared[:1], shared}, []any{shared, shared[:1]}} {
		if got, err := valuecodec.EncodeValue(t.Context(), v); got != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
			t.Fatalf("long shared alias accepted=%v err=%v", got, err)
		}
	}
}

func TestExactFrameDeepAndCancellation(t *testing.T) {
	var value any = json.Number("9007199254740993.0")
	for i := 0; i < 20000; i++ {
		value = []any{value}
	}
	frame, err := valuecodec.EncodeValue(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	got, err := valuecodec.DecodeFrameJSON(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		got = got.([]any)[0]
	}
	if got != json.Number("9007199254740993.0") {
		t.Fatalf("deep leaf=%#v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if f, err := valuecodec.EncodeValue(ctx, value); f != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("encode cancellation=%v %v", f, err)
	}
	if v, err := valuecodec.DecodeValue(ctx, frame); v != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation=%v %v", v, err)
	}
	// A deeply nested wire value keeps the same canonical bytes after transport.
	second, err := valuecodec.CanonicalJSON(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	third, err := valuecodec.DecodeFrameJSON(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := valuecodec.CanonicalJSON(t.Context(), third)
	if err != nil || !bytes.Equal(second, fourth) {
		t.Fatalf("deep canon changed: %v", err)
	}
}

func TestExactFrameStructuralIndicesStayExact(t *testing.T) {
	for _, index := range []string{"1", "1.0", "1e0", "100e-2", "0.001e3"} {
		wire := `["iterion.exact-values/1",[["array",[` + index + `]],["number","2.00"]]]`
		value, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(wire))
		if err != nil || !reflect.DeepEqual(value, []any{json.Number("2.00")}) {
			t.Fatalf("exact index %s value=%#v err=%v", index, value, err)
		}
	}
	for _, index := range []string{"1.00000000000000000000001", "1e9999999999999999999999999999", "1e-9999999999999999999999999999", "-1", "10000000000000000000000001e-25"} {
		wire := `["iterion.exact-values/1",[["array",[` + index + `]],["null",null]]]`
		if value, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(wire)); value != nil || !errors.Is(err, valuecodec.ErrInvalidFrame) {
			t.Fatalf("inexact index %s value=%#v err=%v", index, value, err)
		}
	}
}

func TestExactFrameRejectsMalformedWithoutPartialValue(t *testing.T) {
	bad := []string{`null`, `{}`, `[]`, `[1]`, `["iterion.exact-values/1"]`, `["iterion.exact-values/1",[]]`, `["iterion.exact-values/1",[["null",1]]]`, `["iterion.exact-values/1",[["array",[0]]]]`, `["iterion.exact-values/1",[["array",[1,1]],["null",null]]]`, `["iterion.exact-values/1",[["null",null],["null",null]]]`, `["iterion.exact-values/1",[["array",[1.5]],["null",null]]]`, `["iterion.exact-values/1",[["array",[9999999999999999999999]],["null",null]]]`, `["iterion.exact-values/1",[["object",[["a",1],["a",2]]],["null",null],["null",null]]]`, `["iterion.exact-values/1",[["int8","128"]]]`, `["iterion.exact-values/1",[["uint64","18446744073709551616"]]]`, `["iterion.exact-values/1",[["int","+1"]]]`, `["iterion.exact-values/1",[["int","01"]]]`, `["iterion.exact-values/1",[["number","1 "]]]`, `["iterion.exact-values/1",[["number",1]]]`, `["iterion.exact-values/1",[["float64","NaN"]]]`, `["iterion.exact-values/1",[["float64","1e400"]]]`, `["iterion.exact-values/1",[["bytes","Zg==\n"]]]`, `["iterion.exact-values/1",[["newkind","x"]]]`}
	for _, wire := range bad {
		if value, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(wire)); value != nil || !errors.Is(err, valuecodec.ErrInvalidFrame) {
			t.Fatalf("malformed wire %s value=%#v err=%v", wire, value, err)
		}
	}
	for _, wire := range []string{`["iterion.exact-values/2"]`, `["iterion.exact-values/999",null,1]`} {
		if value, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(wire)); value != nil || !errors.Is(err, valuecodec.ErrUnsupportedVersion) {
			t.Fatalf("future version value=%#v err=%v", value, err)
		}
	}
	if value, err := valuecodec.DecodeFrameJSON(t.Context(), []byte(`["iterion.exact-values/1",[["string","\ud800"]]]`)); value != nil || !errors.Is(err, valuecodec.ErrInvalidJSON) {
		t.Fatalf("Unicode before wire got=%#v err=%v", value, err)
	}
	frame, err := valuecodec.EncodeValue(t.Context(), []any{1})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := valuecodec.DecodeMap(t.Context(), frame); v != nil || !errors.Is(err, valuecodec.ErrInvalidFrame) {
		t.Fatalf("map root accepted array: %#v %v", v, err)
	}
	for _, value := range []map[string]any{nil, {}} {
		frame, err := valuecodec.EncodeMap(t.Context(), value)
		if err != nil {
			t.Fatal(err)
		}
		got, err := valuecodec.DecodeMap(t.Context(), frame)
		if err != nil || !reflect.DeepEqual(got, value) {
			t.Fatalf("nil/empty got=%#v want=%#v err=%v", got, value, err)
		}
	}
}

func TestExactFrameLexicalNumbers(t *testing.T) {
	for _, text := range []string{"9007199254740993.0", "0.100000000000000005", "1e400", "-0", "2.00", "9223372036854775808"} {
		t.Run(text, func(t *testing.T) {
			frame, err := valuecodec.EncodeValue(t.Context(), json.Number(text))
			if err != nil {
				t.Fatal(err)
			}
			got, err := valuecodec.DecodeValue(t.Context(), frame)
			if err != nil || got != json.Number(text) {
				t.Fatalf("lexeme changed got=%#v err=%v", got, err)
			}
		})
	}
}

type cancellingContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancellingContext) Err() error {
	if c.calls.Add(1) == 17 {
		c.cancel()
	}
	return c.Context.Err()
}
func midwayContext(t *testing.T) *cancellingContext {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &cancellingContext{Context: ctx, cancel: cancel}
}
func TestExactFrameCancellationDuringWalk(t *testing.T) {
	value := make([]any, 1000)
	for i := range value {
		value[i] = map[string]any{"value": i}
	}
	frame, err := valuecodec.EncodeValue(t.Context(), value)
	if err != nil {
		t.Fatal(err)
	}
	encctx := midwayContext(t)
	if f, err := valuecodec.EncodeValue(encctx, value); f != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("encode ignored cancellation during walk calls=%d err=%v", encctx.calls.Load(), err)
	}
	decctx := midwayContext(t)
	if v, err := valuecodec.DecodeValue(decctx, frame); v != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("decode ignored cancellation during walk calls=%d err=%v", decctx.calls.Load(), err)
	}
}

// Every custom codec spelling is refused before a scalar, key or populated
// byte slice can normalize it. Empty containers contain no element to inspect.
type byteJSONValue uint8
type byteJSONPointer uint8
type byteTextValue uint8
type byteTextPointer uint8

var byteCodecCalls atomic.Int64

func (b byteJSONValue) MarshalJSON() ([]byte, error) {
	byteCodecCalls.Add(1)
	return []byte(`"custom"`), nil
}
func (b *byteJSONPointer) MarshalJSON() ([]byte, error) {
	byteCodecCalls.Add(1)
	return []byte(`"custom"`), nil
}
func (b byteTextValue) MarshalText() ([]byte, error) {
	byteCodecCalls.Add(1)
	return []byte("custom"), nil
}
func (b *byteTextPointer) MarshalText() ([]byte, error) {
	byteCodecCalls.Add(1)
	return []byte("custom"), nil
}

func TestExactFrameByteElementsRespectCustomCodecs(t *testing.T) {
	cases := []struct {
		name                                                          string
		scalar, slice, array, nested, key, empty, nilSlice, zeroArray any
	}{
		{"json-value", byteJSONValue(1), []byteJSONValue{1}, [1]byteJSONValue{1}, []any{[]byteJSONValue{1}}, map[byteJSONValue]int{1: 2}, []byteJSONValue{}, []byteJSONValue(nil), [0]byteJSONValue{}},
		{"json-pointer", byteJSONPointer(1), []byteJSONPointer{1}, [1]byteJSONPointer{1}, []any{[]byteJSONPointer{1}}, map[byteJSONPointer]int{1: 2}, []byteJSONPointer{}, []byteJSONPointer(nil), [0]byteJSONPointer{}},
		{"text-value", byteTextValue(1), []byteTextValue{1}, [1]byteTextValue{1}, []any{[]byteTextValue{1}}, map[byteTextValue]int{1: 2}, []byteTextValue{}, []byteTextValue(nil), [0]byteTextValue{}},
		{"text-pointer", byteTextPointer(1), []byteTextPointer{1}, [1]byteTextPointer{1}, []any{[]byteTextPointer{1}}, map[byteTextPointer]int{1: 2}, []byteTextPointer{}, []byteTextPointer(nil), [0]byteTextPointer{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for kind, value := range map[string]any{"scalar": tc.scalar, "slice": tc.slice, "array": tc.array, "nested": tc.nested, "key": tc.key} {
				t.Run(kind, func(t *testing.T) {
					before := byteCodecCalls.Load()
					frame, err := valuecodec.EncodeValue(t.Context(), value)
					if frame != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
						t.Fatalf("custom %T silently normalized: %v %v", value, frame, err)
					}
					if byteCodecCalls.Load() != before {
						t.Fatal("custom callback invoked")
					}
				})
			}
			for kind, value := range map[string]any{"empty": tc.empty, "nil": tc.nilSlice, "zero-array": tc.zeroArray} {
				t.Run(kind, func(t *testing.T) {
					before := byteCodecCalls.Load()
					frame, err := valuecodec.EncodeValue(t.Context(), value)
					if err != nil {
						t.Fatal(err)
					}
					got, err := valuecodec.DecodeValue(t.Context(), frame)
					if err != nil {
						t.Fatal(err)
					}
					if kind == "nil" && got != nil {
						t.Fatalf("nil became %#v", got)
					}
					if kind == "empty" && !reflect.DeepEqual(got, []byte{}) {
						t.Fatalf("empty bytes became %#v", got)
					}
					if kind == "zero-array" && !reflect.DeepEqual(got, []any{}) {
						t.Fatalf("empty array became %#v", got)
					}
					if byteCodecCalls.Load() != before {
						t.Fatal("empty container invoked callback")
					}
				})
			}
		})
	}
}
