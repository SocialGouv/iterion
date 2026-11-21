package ir

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/valuecodec"
)

func TestExactVarTextNumbersAndExpansion(t *testing.T) {
	t.Parallel()
	lookup := func(k string) string {
		if k == "PROJECT_DIR" {
			return "/worker/\"new\""
		}
		return ""
	}
	raw := `{"n":9007199254740993.0,"huge":1e400,"small":1e-4000,"path":"${PROJECT_DIR}/x","cost":"$5","nested":[-0,2.00]}`
	got, err := ResolveVarTextExact(t.Context(), raw, VarJSON, lookup)
	want := map[string]any{"n": json.Number("9007199254740993.0"), "huge": json.Number("1e400"), "small": json.Number("1e-4000"), "path": "/worker/\"new\"/x", "cost": "$5", "nested": []any{json.Number("-0"), json.Number("2.00")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, %v", got, err)
	}
	legacy, err := ResolveVarText(`{"n":9007199254740993.0}`, VarJSON, lookup)
	if err != nil || legacy.(map[string]any)["n"] != float64(9007199254740992) {
		t.Fatalf("legacy changed: %#v %v", legacy, err)
	}
	view, err := ResolveVarTextExactAsWritten(t.Context(), `{"n":2.00,"path":"${PROJECT_DIR}/x","fallback":"${MISSING:-a,b}"}`, VarJSON)
	if err != nil || !reflect.DeepEqual(view, map[string]any{"n": json.Number("2.00"), "path": "${PROJECT_DIR}/x", "fallback": "a,b"}) {
		t.Fatalf("view %#v %v", view, err)
	}
}

func TestExactVarTextNativeReadingParity(t *testing.T) {
	t.Parallel()
	lookup := func(k string) string {
		if k == "LIST" {
			return "a,b"
		}
		return ""
	}
	for _, tc := range []struct {
		v    any
		typ  VarType
		want any
	}{
		{"${LIST:-x,y}", VarStringArray, []any{"a", "b"}},
		{"[bad,json]", VarStringArray, []any{"[bad", "json]"}},
		{"not-json ${MISSING:-ok}", VarJSON, "not-json ok"},
		{`"${LIST}"`, VarJSON, "a,b"},
		{"true", VarBool, true}, {"9223372036854775807", VarInt, int64(math.MaxInt64)},
		{"0.1", VarFloat, float64(.1)}, {"1e-4000", VarFloat, float64(0)},
		{map[string]any{"n": json.Number("1e400")}, VarJSON, map[string]any{"n": json.Number("1e400")}},
	} {
		got, err := ResolveVarTextExact(t.Context(), tc.v, tc.typ, lookup)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%#v %s: %#v %v", tc.v, tc.typ, got, err)
		}
	}
	got, err := ResolveVarTextExact(t.Context(), `[9007199254740993.0,"${LIST}"]`, VarStringArray, lookup)
	if err != nil || !reflect.DeepEqual(got, []any{json.Number("9007199254740993.0"), "a,b"}) {
		t.Fatalf("list %#v %v", got, err)
	}
}

func TestExactVarTextRefusesFidelityLoss(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v   any
		typ VarType
	}{
		{`{"x":1,"x":2}`, VarJSON}, {`{"x":"\ud800"}`, VarJSON},
		{`[{"x":1,"x":2}]`, VarStringArray}, {`["\udc00"]`, VarStringArray},
		{"NaN", VarFloat}, {"Inf", VarFloat}, {math.Inf(1), VarJSON},
		{json.Number("01"), VarJSON}, {string([]byte{0xff}), VarString},
	} {
		got, err := ResolveVarTextExact(t.Context(), tc.v, tc.typ, func(string) string { return "" })
		if err == nil || got != nil {
			t.Fatalf("accepted %#v: %#v %v", tc.v, got, err)
		}
	}
	got, err := ResolveVarTextExact(t.Context(), `{"x":"${BAD}"}`, VarJSON, func(string) string { return string([]byte{0xff}) })
	if got != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatalf("expanded invalid Unicode: %#v %v", got, err)
	}
}

func TestExactVarTextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	if v, err := ResolveVarTextExact(ctx, "${KEY}", VarString, func(string) string { calls++; return "x" }); v != nil || !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("pre-cancel: %#v %v calls=%d", v, err, calls)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	if v, err := ResolveVarTextExact(ctx, `{"x":"${KEY}"}`, VarJSON, func(string) string { cancel(); return "x" }); v != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation: %#v %v", v, err)
	}
}
