package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestExactNativeIntegerDomain(t *testing.T) {
	if n, ok, err := exactNativeInt(nil, json.Number("2"), false); err != nil || !ok || n != 2 {
		t.Fatalf("nil caller context: %d %v %v", n, ok, err)
	}
	for _, v := range []any{json.Number("1.0000000000000000000000001"), json.Number("1e-4000"), json.Number("9223372036854775808"), json.Number("1e999999999999999999999999"), json.Number("01"), 1.9, math.Inf(1), math.NaN(), uint64(math.MaxUint64), nil, true} {
		if n, ok, err := exactNativeInt(t.Context(), v, true); err != nil || ok {
			t.Fatalf("accepted %T(%v): %d %v %v", v, v, n, ok, err)
		}
	}
	for _, v := range []any{json.Number("2.00"), json.Number("20e-1"), float64(2), int8(2), uint8(2), "2"} {
		if n, ok, err := exactNativeInt(t.Context(), v, true); err != nil || !ok || n != 2 {
			t.Fatalf("refused %T(%v): %d %v %v", v, v, n, ok, err)
		}
	}
	if strconv.IntSize == 64 {
		if n, ok, err := exactNativeInt(t.Context(), json.Number("9007199254740993.0"), false); err != nil || !ok || int64(n) != 9007199254740993 {
			t.Fatalf("integer rounded: %d %v %v", n, ok, err)
		}
	}
	if _, ok, _ := exactNativeInt(t.Context(), "2", false); ok {
		t.Fatal("implicit text tolerance")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := exactNativeInt(ctx, json.Number("2"), true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
