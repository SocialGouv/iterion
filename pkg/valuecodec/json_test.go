package valuecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestDecodeJSONExactTree(t *testing.T) {
	raw := []byte(`{"count":9007199254740993.0,"fraction":0.100000000000000005,"array":[1e400,-0,2.00,{"a/b~c":"9007199254740993.0"}],"bool":true,"nil":null,"empty":{},"list":[],"unicode":"é\ud83d\ude00�","escape":"\\uD800"}`)
	before := bytes.Clone(raw)
	got, err := DecodeJSON(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"count": json.Number("9007199254740993.0"), "fraction": json.Number("0.100000000000000005"),
		"array": []any{json.Number("1e400"), json.Number("-0"), json.Number("2.00"), map[string]any{"a/b~c": "9007199254740993.0"}},
		"bool":  true, "nil": nil, "empty": map[string]any{}, "list": []any{}, "unicode": "é😀�", "escape": `\uD800`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded tree=%#v want=%#v", got, want)
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("input mutated")
	}
	for _, raw := range []string{`null`, `true`, `false`, `"x"`, `0`, `[]`, `{}`, `[{},[],[1]]`} {
		t.Run(raw, func(t *testing.T) {
			v, err := DecodeJSON(t.Context(), []byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			d := json.NewDecoder(strings.NewReader(raw))
			d.UseNumber()
			var w any
			if err := d.Decode(&w); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(v, w) {
				t.Fatalf("got=%#v want=%#v", v, w)
			}
		})
	}
}

func TestDecodeJSONRefusesBeforeResult(t *testing.T) {
	inputs := []string{"", " ", "[", "{", `{"a":}`, `{"a" 1}`, `[1,]`, `true false`, `0 1`, `{"a":null,"a":1}`, `{"a":{},"\u0061":2}`, `{"n":{"x":1,"x":2}}`, `{"a":1}[]`, `01`, `NaN`, `"\ud800"`, `"\udfff"`, `"\ud800\ud800"`, `{"\ud800":1}`, `"\ud800x\udc00"`, string([]byte{'"', 0xff, '"'})}
	for i, raw := range inputs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			got, err := DecodeJSON(t.Context(), []byte(raw))
			if got != nil || !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("raw=%q got=%#v err=%v", raw, got, err)
			}
		})
	}
	// The same key in different objects is valid; escapes equivalent to the same
	// decoded key in one object are not. No global key registry is involved.
	if _, err := DecodeJSON(t.Context(), []byte(`{"a":{"a":1},"b":{"a":2}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJSONUnicodeUnits(t *testing.T) {
	for n := 0; n <= 0xffff; n++ {
		raw := fmt.Sprintf(`"\u%04x"`, n)
		got, err := DecodeJSON(t.Context(), []byte(raw))
		if n >= 0xd800 && n <= 0xdfff {
			if got != nil || !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("surrogate admitted %s=%#v %v", raw, got, err)
			}
			continue
		}
		if err != nil || got != string(rune(n)) {
			t.Fatalf("scalar %s=%#v %v", raw, got, err)
		}
	}
	for high := 0xd800; high <= 0xdbff; high++ {
		for _, low := range []int{0xdc00, 0xde00, 0xdfff} {
			raw := fmt.Sprintf(`"\u%04X\u%04x"`, high, low)
			got, err := DecodeJSON(t.Context(), []byte(raw))
			want := string(utf16.DecodeRune(rune(high), rune(low)))
			if err != nil || got != want {
				t.Fatalf("pair %s=%#v want%q %v", raw, got, want, err)
			}
		}
	}
	for _, raw := range []string{`"\\uD800"`, `"\"\\uD800"`, `"\\\"x"`, `"\u005cud800"`, `"\ufffd"`, `"\ud83d\ude00"`} {
		if _, err := DecodeJSON(t.Context(), []byte(raw)); err != nil {
			t.Fatalf("valid escape %s: %v", raw, err)
		}
	}
}

func TestDecodeJSONCancellationAndDepth(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := DecodeJSON(ctx, []byte(`{"a":1}`)); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel got=%#v err=%v", got, err)
	}
	raw := strings.Repeat("[", 1000) + "9007199254740993.0" + strings.Repeat("]", 1000)
	got, err := DecodeJSON(t.Context(), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		got = got.([]any)[0]
	}
	if got != json.Number("9007199254740993.0") {
		t.Fatalf("deep number=%#v", got)
	}
	// Token decoding does not inherit Unmarshal's recursive nesting limit.
	raw = strings.Repeat("[", 20000) + "9007199254740993.0" + strings.Repeat("]", 20000)
	got, err = DecodeJSON(t.Context(), []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		got = got.([]any)[0]
	}
	if got != json.Number("9007199254740993.0") {
		t.Fatalf("deep number=%#v", got)
	}
	if got, err := DecodeJSON(t.Context(), []byte(raw[:len(raw)-1])); got != nil || !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("incomplete deep tree got=%T err=%v", got, err)
	}
}
