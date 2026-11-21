package ir_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/valuecodec"
)

type materializedInt int64
type materializedFloat float64
type materializedString string
type materializedBool bool
type materializedBytes []byte
type materializedMap map[int]materializedInt
type materializedStringer int64

func (materializedStringer) String() string { panic("payload String method invoked") }

type materializedCallback int

func (materializedCallback) MarshalJSON() ([]byte, error) { panic("payload JSON method invoked") }

func TestExactVarTransportParity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, expression string
		input, want      any
	}{
		{"named zero", "!input.x", materializedInt(0), true},
		{"named fraction", "input.x < 1", materializedFloat(.5), true},
		{"named integer", "input.x + 1", materializedInt(9007199254740993), int64(9007199254740994)},
		{"named string", `join([input.x], ",")`, materializedString("hello"), "hello"},
		{"native zero", "!input.x", int64(0), true},
		{"native integer", "input.x + 1", int64(9007199254740993), int64(9007199254740994)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			native, err := ir.ResolveVarTextExact(t.Context(), tc.input, ir.VarJSON, nil)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := valuecodec.EncodeValue(t.Context(), native)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := valuecodec.DecodeValue(t.Context(), frame)
			if err != nil {
				t.Fatal(err)
			}
			ast := expr.MustParse(tc.expression)
			for _, value := range []any{native, replayed} {
				got, err := ast.Eval(&expr.Context{NumericMode: expr.ExactNumeric, Input: func([]string) any { return value }})
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("%T before/after transport: got %#v, %v; want %#v", value, got, err, tc.want)
				}
			}
		})
	}
}

func TestExactVarMaterialization(t *testing.T) {
	t.Parallel()
	n := materializedInt(9007199254740993)
	input := map[string]any{"pointer": &n, "bool": materializedBool(false), "map": materializedMap{1: 7}, "bytes": materializedBytes{0, 255}, "number": json.Number("9007199254740993.000"), "binary": float32(.1), "method": materializedStringer(3), "empty": []int64{}, "nil": []int64(nil)}
	frame, err := valuecodec.EncodeValue(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	want, err := valuecodec.DecodeValue(t.Context(), frame)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ir.ResolveVarTextExact(t.Context(), input, ir.VarJSON, func(string) string { t.Fatal("native value expanded"); return "" })
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("materialization: %#v, %v; want %#v", got, err, want)
	}
	got.(map[string]any)["bytes"].([]byte)[0] = 42
	if input["bytes"].(materializedBytes)[0] != 0 || n != 9007199254740993 {
		t.Fatal("materialized result aliases input")
	}
	if out, err := ir.ResolveVarTextExact(t.Context(), materializedCallback(1), ir.VarJSON, nil); out != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatalf("callback payload accepted: %#v, %v", out, err)
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	if out, err := ir.ResolveVarTextExact(t.Context(), cycle, ir.VarJSON, nil); out != nil || !errors.Is(err, valuecodec.ErrUnsupportedValue) {
		t.Fatalf("cycle accepted: %v", err)
	}
}

func TestExactVarCancellationStopsLookups(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  ir.VarType
		raw  string
	}{
		{"JSON leaves", ir.VarJSON, `["${A}","${B}","${C}"]`},
		{"JSON same leaf", ir.VarJSON, `"${A}${B}${C}"`},
		{"text braced", ir.VarString, `${A}${B}${C}`},
		{"text bare", ir.VarString, `$A $B $C`},
		{"string array", ir.VarStringArray, `["${A}","${B}","${C}"]`},
		{"nested default", ir.VarString, `${A:-${B:-x}}${C}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			out, err := ir.ResolveVarTextExact(ctx, tc.raw, tc.typ, func(key string) string { calls++; cancel(); return key })
			if out != nil || !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("cancellation: output=%#v err=%v calls=%d", out, err, calls)
			}
		})
	}
}
