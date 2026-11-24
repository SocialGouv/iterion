package expr

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func numericContext(mode NumericMode, values map[string]any) *Context {
	return &Context{NumericMode: mode, Input: func(path []string) any { return descendPath(values, path) }}
}

func TestExactNumericModeSameAST(t *testing.T) {
	for _, src := range []string{"01.0", "000.100", "0.10000000000000000001"} {
		a, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		before := ToSnapshot(a)
		legacy, err := a.Eval(numericContext(LegacyNumeric, nil))
		if err != nil {
			t.Fatal(err)
		}
		exact, err := a.Eval(numericContext(ExactNumeric, nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := legacy.(float64); !ok {
			t.Fatalf("legacy %T", legacy)
		}
		n, ok := exact.(json.Number)
		if !ok || !json.Valid([]byte(n)) || string(n) != normalizeDecimalLiteral(src) {
			t.Fatalf("exact %T(%v)", exact, exact)
		}
		again, err := a.Eval(numericContext(LegacyNumeric, nil))
		if err != nil || again != legacy || !reflect.DeepEqual(before, ToSnapshot(a)) {
			t.Fatalf("AST mutated: %v %v", again, err)
		}
	}
	if _, err := Parse("1."); err == nil {
		t.Fatal("grammar widened")
	}
	a := MustParse("input.x == 0.1")
	values := map[string]any{"x": json.Number("0.1")}
	if got, err := a.Eval(numericContext(ExactNumeric, values)); err != nil || got != true {
		t.Fatalf("exact decimal: %v %v", got, err)
	}
	if _, err := a.Eval(numericContext(NumericMode(99), values)); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestExactNumericOperations(t *testing.T) {
	n := func(s string) any { return json.Number(s) }
	values := map[string]any{
		"a": n("9007199254740993"), "b": n("9007199254740992"),
		"huge": n("1e400"), "tiny": n("1e-4000"), "zero": n("-0e9999"),
		"max": n("9223372036854775807"), "min": n("-9223372036854775808"),
		"edge": n("9223372036854775807.1"), "fraction": n("1.000000000000000000001"),
		"index": n("1.00"), "arr": []any{"a", "b", "c"},
		"nums":     []any{n("1.0"), n("1"), n("1e0"), "1", int64(1)},
		"ordered":  []any{n("9007199254740993"), n("9007199254740992")},
		"mix":      []any{n("0.10000000000000001"), float64(0.1), n("0.1")},
		"negative": n("-0.0000000000001"), "native": float64(1e20),
	}
	tests := []struct {
		src  string
		want any
	}{
		{"input.a > input.b", true}, {"input.a == input.b", false},
		{"input.a + 1", int64(9007199254740994)}, {"input.index + 1", int64(2)},
		{"input.fraction / 2", float64(0.5)}, {"input.native + 1", float64(1e20)},
		{"floor(input.edge)", int64(math.MaxInt64)}, {"round(input.edge)", int64(math.MaxInt64)},
		{"floor(input.negative)", int64(-1)}, {"round(input.negative)", int64(0)},
		{"input.arr[input.index]", "b"}, {"slice(input.arr, 1.0, 2.0)", []any{"b"}},
		{"tail(input.arr, 1.0)", []any{"c"}}, {"if(input.tiny, 7, 1 / 0)", int64(7)},
		{"!input.zero", true}, {"input.zero || input.tiny", true}, {"input.zero && (1 / 0)", false},
		{"contains(input.nums, 1.000)", true}, {"contains([1.0], \"1\")", false},
		{"unique(input.nums)", []any{n("1.0"), "1"}},
		{"sort(input.ordered)", []any{n("9007199254740992"), n("9007199254740993")}},
		{"sort(input.mix)", []any{n("0.1"), float64(0.1), n("0.10000000000000001")}},
		{"min(input.ordered)", n("9007199254740992")}, {"max(input.b, input.a)", n("9007199254740993")},
		{"sum([input.a, 1])", int64(9007199254740994)}, {"sum([input.fraction, 1])", float64(2)},
		{"\"n=\" + input.huge", "n=1e400"}, {"join([input.a, input.huge], \",\")", "9007199254740993,1e400"},
		{"filter([input.zero, input.tiny], x => x)", []any{n("1e-4000")}},
		{"map([input.a], x => x + 1)", []any{int64(9007199254740994)}},
	}
	ctx := numericContext(ExactNumeric, values)
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			got, err := MustParse(tt.src).Eval(ctx)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %T(%v), %v; want %T(%v)", got, got, err, tt.want, tt.want)
			}
		})
	}
	for _, src := range []string{"input.huge + 0.5", "input.max + 1", "-input.min", "input.arr[input.fraction]", "floor(input.huge)", "sum([0.5, input.huge])"} {
		t.Run("refuse:"+src, func(t *testing.T) {
			if got, err := MustParse(src).Eval(ctx); err == nil {
				t.Fatalf("accepted %v", got)
			}
		})
	}
	for _, key := range []string{"tiny", "zero"} {
		got, err := MustParse("input." + key).EvalBool(ctx)
		if err != nil || got != (key == "tiny") {
			t.Fatalf("EvalBool %s: %v %v", key, got, err)
		}
	}
}

func TestExactNumericBudgetBeforeReading(t *testing.T) {
	t.Setenv(EvalEnv, "100")
	long := json.Number("1." + strings.Repeat("0", 1000))
	values := map[string]any{"n": long, "arr": []any{long}, "nested": []any{[]any{long}}}
	for _, src := range []string{"input.n + 1", "input.n == 1", "!input.n", "input.arr[input.n]", "tail(input.arr, input.n)", "slice(input.arr, 0, input.n)", "floor(input.n)", "round(input.n)", "contains(input.arr, 1)", "unique(input.arr)", "sort(input.arr)", "sum(input.arr)", "min(input.arr)", "max(input.arr)", "\"\" + input.n", "join(input.arr, \"\")", "join(input.nested, \"\")"} {
		t.Run(src, func(t *testing.T) {
			got, err := MustParse(src).Eval(numericContext(ExactNumeric, values))
			if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
				t.Fatalf("not charged: %v %v", got, err)
			}
		})
	}
	if _, err := MustParse("input.n").EvalBool(numericContext(ExactNumeric, values)); err == nil {
		t.Fatal("EvalBool not charged")
	}
	t.Setenv(EvalEnv, "0")
	if got, err := MustParse("input.n == 1").Eval(numericContext(ExactNumeric, values)); err != nil || got != true {
		t.Fatalf("override broken: %v %v", got, err)
	}
}

func TestExactNumericFormattingAndCancellation(t *testing.T) {
	values := map[string]any{"items": []any{[]any{json.Number("1e400"), "a"}, map[string]any{"x": json.Number("1.00")}}}
	got, err := MustParse("join(input.items, \",\")").Eval(numericContext(ExactNumeric, values))
	if err != nil || got != "[1e400 a],map[x:1.00]" {
		t.Fatalf("format: %v %v", got, err)
	}
	cycle := []any{nil}
	cycle[0] = cycle
	values["items"] = cycle
	if _, err := MustParse("join(input.items, \"\")").Eval(numericContext(ExactNumeric, values)); err == nil {
		t.Fatal("cycle accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MustParse("1 + 1").EvalContext(ctx, numericContext(ExactNumeric, nil)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	st := &evalState{ctx: numericContext(ExactNumeric, nil), numericMode: ExactNumeric, cancel: ctx, work: 10000}
	if _, err := exactSort(st, []any{[]any{json.Number("2"), json.Number("1")}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("sort cancellation: %v", err)
	}
}

func TestExactCollectionIdentityAndMaterializedFormatting(t *testing.T) {
	values := map[string]any{"arrays": []any{[]any{"x"}, []any{"x"}}, "needle": []any{"x"}}
	ctx := numericContext(ExactNumeric, values)
	if got, err := MustParse("contains(input.arrays, input.needle)").Eval(ctx); err != nil || got != true {
		t.Fatalf("collection contains: %v %v", got, err)
	}
	if got, err := MustParse("length(unique(input.arrays))").Eval(ctx); err != nil || got != int64(1) {
		t.Fatalf("collection unique: %v %v", got, err)
	}
	// A shared noncyclic child remains legal; map order is deterministic.
	shared := []any{json.Number("1.0")}
	values["items"] = []any{shared, shared, map[string]any{"z": "last", "a": "first"}}
	if got, err := MustParse(`join(input.items, ",")`).Eval(ctx); err != nil || got != "[1.0],[1.0],map[a:first z:last]" {
		t.Fatalf("DAG and keys: %v %v", got, err)
	}
}

func TestExactFormattingEmptySubviewIsNotCycle(t *testing.T) {
	parent := make([]any, 1)
	parent[0] = parent[:0]
	ctx := numericContext(ExactNumeric, map[string]any{"items": []any{parent}})
	got, err := MustParse(`join(input.items, ",")`).Eval(ctx)
	if err != nil || got != "[[]]" {
		t.Fatalf("empty subview is not a cycle: %v %v", got, err)
	}
}

func TestNumericModeIsFixedForWholeEvaluation(t *testing.T) {
	for _, mode := range []NumericMode{LegacyNumeric, ExactNumeric} {
		ctx := &Context{NumericMode: mode}
		ctx.Input = func([]string) any {
			if ctx.NumericMode == ExactNumeric {
				ctx.NumericMode = LegacyNumeric
			} else {
				ctx.NumericMode = ExactNumeric
			}
			return json.Number("1.0")
		}
		got, err := MustParse("input.n == 1.0").Eval(ctx)
		if err != nil || got != (mode == ExactNumeric) {
			t.Fatalf("mode changed during evaluation: started=%v got=%v err=%v", mode, got, err)
		}
	}
}
