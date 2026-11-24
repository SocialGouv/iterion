package model

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestExactNumericValidationAndConformance(t *testing.T) {
	t.Parallel()
	ints := &ir.Schema{Fields: []*ir.SchemaField{{Name: "n", Type: ir.FieldTypeInt}}}
	for raw, want := range map[string]int64{"9007199254740993.0": 9007199254740993, "9223372036854775807": math.MaxInt64, "-9223372036854775808.00": math.MinInt64, "10e-1": 1} {
		out := map[string]any{"n": json.Number(raw)}
		if err := ValidateOutput(out, ints); err != nil {
			t.Fatal(err)
		}
		if err := ConformComputeOutput(out, ints); err != nil || out["n"] != want {
			t.Fatalf("%s: %v %v", raw, out, err)
		}
	}
	for _, raw := range []string{"1.00000000000000000001", "1e-4000", "9223372036854775808", "1e400", "01"} {
		out := map[string]any{"n": json.Number(raw)}
		if err := ValidateOutput(out, ints); err == nil {
			t.Fatalf("integer accepted %s", raw)
		}
		if err := ConformComputeOutput(out, ints); err == nil {
			t.Fatalf("integer conformed %s", raw)
		}
	}
	floats := &ir.Schema{Fields: []*ir.SchemaField{{Name: "n", Type: ir.FieldTypeFloat}}}
	for _, v := range []any{math.NaN(), math.Inf(1), json.Number("1e400")} {
		if err := ValidateOutput(map[string]any{"n": v}, floats); err == nil {
			t.Fatalf("non-finite accepted: %v", v)
		}
	}
	for _, v := range []any{json.Number("0.1"), uint64(math.MaxUint64), json.Number("-1e-4000")} {
		out := map[string]any{"n": v}
		if err := ConformComputeOutput(out, floats); err != nil {
			t.Fatal(err)
		}
		if _, ok := out["n"].(float64); !ok {
			t.Fatalf("float domain %T", out["n"])
		}
	}
}
