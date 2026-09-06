package expr

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// ctxLists builds a Context exposing arrays/maps under input.* so the bounded
// combinators and indexing have realistic, runtime-shaped data to work on.
func ctxLists() *Context {
	input := map[string]any{
		"nums":  []any{int64(3), int64(1), int64(2)},
		"words": []any{"b", "a", "c"},
		"people": []any{
			map[string]any{"name": "ana", "score": int64(7)},
			map[string]any{"name": "bo", "score": int64(3)},
		},
		"nested": []any{[]any{int64(1), int64(2)}, []any{int64(3)}},
		"obj":    map[string]any{"x": int64(10), "y": int64(20)},
		"floats": []any{1.5, 2.5},
	}
	return makeCtx(nil, input, nil, nil)
}

func evalOK(t *testing.T, src string, ctx *Context) any {
	t.Helper()
	ast, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", src, err)
	}
	got, err := ast.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval(%q) error: %v", src, err)
	}
	return got
}

func TestExpr_Indexing(t *testing.T) {
	ctx := ctxLists()
	cases := []struct {
		src    string
		expect any
	}{
		{"nums[0]", int64(3)},
		{"nums[2]", int64(2)},
		{"nums[9]", nil},  // out of bounds → nil
		{"nums[-1]", nil}, // negative → nil
		{`obj["x"]`, int64(10)},
		{`obj["missing"]`, nil}, // missing key → nil
		{"people[0].name", "ana"},
		{"nested[0][1]", int64(2)},
	}
	for _, c := range cases {
		got := evalOK(t, c.src, ctx)
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("Eval(%q) = %v (%T), want %v (%T)", c.src, got, got, c.expect, c.expect)
		}
	}
}

func TestExpr_IndexingErrors(t *testing.T) {
	ctx := ctxLists()
	cases := []string{
		`nums["k"]`,  // string index on array
		`obj[0]`,     // int key on map
		`nums[0][0]`, // index a scalar
	}
	for _, src := range cases {
		ast, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q) unexpected parse error: %v", src, err)
		}
		if _, err := ast.Eval(ctx); err == nil {
			t.Errorf("Eval(%q) expected a loud error, got nil", src)
		}
	}
}

func TestExpr_Map(t *testing.T) {
	ctx := ctxLists()
	got := evalOK(t, "map(input.people, p => p.score)", ctx)
	want := []any{int64(7), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("map scores = %v, want %v", got, want)
	}
	// map producing a derived scalar
	got = evalOK(t, "map(input.nums, x => x * 2)", ctx)
	want = []any{int64(6), int64(2), int64(4)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("map doubled = %v, want %v", got, want)
	}
}

func TestExpr_Filter(t *testing.T) {
	ctx := ctxLists()
	got := evalOK(t, "filter(input.people, p => p.score > 5)", ctx)
	want := []any{map[string]any{"name": "ana", "score": int64(7)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("filter = %v, want %v", got, want)
	}
}

func TestExpr_Reduce(t *testing.T) {
	ctx := ctxLists()
	got := evalOK(t, "reduce(input.nums, 0, (acc, x) => acc + x)", ctx)
	if got != int64(6) {
		t.Errorf("reduce sum = %v, want 6", got)
	}
	// reduce over object scores
	got = evalOK(t, "reduce(input.people, 0, (acc, p) => acc + p.score)", ctx)
	if got != int64(10) {
		t.Errorf("reduce people scores = %v, want 10", got)
	}
}

func TestExpr_NestedCombinators(t *testing.T) {
	ctx := ctxLists()
	// map over names, each mapped from a person; sort the result
	got := evalOK(t, "sort(map(input.people, p => p.name))", ctx)
	want := []any{"ana", "bo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sort(map names) = %v, want %v", got, want)
	}
	// nested map: flatten(map(nested, xs => xs))
	got = evalOK(t, "flatten(input.nested)", ctx)
	want = []any{int64(1), int64(2), int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flatten = %v, want %v", got, want)
	}
}

func TestExpr_Helpers(t *testing.T) {
	ctx := ctxLists()
	cases := []struct {
		src    string
		expect any
	}{
		{"sort(input.nums)", []any{int64(1), int64(2), int64(3)}},
		{"sort(input.words)", []any{"a", "b", "c"}},
		{"keys(input.obj)", []any{"x", "y"}},
		{"values(input.obj)", []any{int64(10), int64(20)}},
		{"slice(input.nums, 1, 3)", []any{int64(1), int64(2)}},
		{"slice(input.nums, -1, 3)", []any{int64(2)}},
		{"sum(input.nums)", int64(6)},
		{"sum(input.floats)", 4.0},
		{"min(input.nums)", int64(1)},
		{"max(input.nums)", int64(3)},
		// Variadic form. A budget guard clamping one figure between two
		// others — `min(max(floor, cap * ratio), cap * 0.5)` — is the
		// shape every author writes first, and the array-only form made it
		// unwritable: the alternative is the same two subexpressions
		// spelled out four times inside nested `if`s, which is how a
		// deterministic guard becomes unreadable and then wrong.
		{"min(3, 1, 2)", int64(1)},
		{"max(3, 1, 2)", int64(3)},
		{"min(1.5, 2)", 1.5},
		{"max(input.nums, 7)", int64(7)},
	}
	for _, c := range cases {
		got := evalOK(t, c.src, ctx)
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("Eval(%q) = %v (%T), want %v (%T)", c.src, got, got, c.expect, c.expect)
		}
	}
}

// floor / round are the explicit form a division takes under an `int` field
// (#792): both return an int64, so the compute output's declared type and
// its value agree without the engine guessing which rounding was meant.
func TestExpr_FloorRound(t *testing.T) {
	ctx := makeCtx(nil, map[string]any{
		"f":    10.58,
		"neg":  -2.5,
		"half": 2.5,
		"i":    int64(7),
		"s":    "x",
		"inf":  math.Inf(1),
	}, nil, nil)
	cases := []struct {
		src    string
		expect any
	}{
		{"floor(input.f)", int64(10)},
		{"round(input.f)", int64(11)},
		{"floor(input.neg)", int64(-3)},
		{"round(input.neg)", int64(-3)}, // half away from zero
		{"round(input.half)", int64(3)},
		{"floor(input.i)", int64(7)},
		{"round(input.i)", int64(7)},
		{"floor(1058 / 100.0)", int64(10)},
		{"floor(7 * 100 / 30)", int64(23)},
	}
	for _, c := range cases {
		got := evalOK(t, c.src, ctx)
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("Eval(%q) = %v (%T), want %v (%T)", c.src, got, got, c.expect, c.expect)
		}
	}

	errCases := []struct {
		src      string
		contains string
	}{
		{"floor(input.s)", "expects a number"},
		{"round(input.s)", "expects a number"},
		{"floor(input.inf)", "not a finite"},
		{"floor(input.f, 2)", "takes 1 argument"},
	}
	for _, c := range errCases {
		ast, err := Parse(c.src)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.src, err)
			continue
		}
		_, err = ast.Eval(ctx)
		if err == nil || !strings.Contains(err.Error(), c.contains) {
			t.Errorf("Eval(%q) error = %v, want substring %q", c.src, err, c.contains)
		}
	}
}

func TestExpr_LambdaParseErrors(t *testing.T) {
	cases := []struct {
		src      string
		contains string
	}{
		{"map(input.nums, x, y => x)", "'=>'"},                          // bad single-param shape (missing parens)
		{"reduce(input.nums, 0, x => x)", "takes 2 parameter"},          // reduce needs 2 params
		{"map(input.nums, (a, b) => a)", "takes 1 parameter"},           // map needs 1 param
		{"map(input.nums, outputs => 1)", "collides with the reserved"}, // namespace collision
		{"map(input.nums, x x)", "'=>'"},                                // missing arrow
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("Parse(%q) expected error, got nil", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.contains) {
			t.Errorf("Parse(%q) error = %q, want substring %q", c.src, err.Error(), c.contains)
		}
	}
}

// TestExpr_VisitBudget proves the combinator work budget is finite: an
// adversarial doubly-nested map over large inputs must error rather than spin.
func TestExpr_VisitBudget(t *testing.T) {
	big := make([]any, 1000)
	for i := range big {
		big[i] = int64(i)
	}
	ctx := makeCtx(nil, map[string]any{"big": big}, nil, nil)
	// 1000 * 1000 = 1_000_000 element visits > maxEvalVisits (100_000).
	ast, err := Parse("map(input.big, x => map(input.big, y => y))")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if _, err := ast.Eval(ctx); err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("expected visit-budget error, got %v", err)
	}
}

// TestExpr_LambdaRefsExcludeParams verifies the reference walker does not
// surface lambda-bound parameters as external refs (which would make the
// compiler reject a valid expression as an unknown reference).
func TestExpr_LambdaRefsExcludeParams(t *testing.T) {
	ast, err := Parse("map(input.people, p => p.score > vars.threshold)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	var got []string
	for _, r := range ast.Refs() {
		got = append(got, r.Namespace)
	}
	for _, ns := range got {
		if ns == "p" {
			t.Errorf("Refs() leaked lambda parameter %q as an external ref: %v", "p", got)
		}
	}
	// The real external refs (input.people, vars.threshold) must still surface.
	wantNS := map[string]bool{"input": false, "vars": false}
	for _, ns := range got {
		if _, ok := wantNS[ns]; ok {
			wantNS[ns] = true
		}
	}
	for ns, seen := range wantNS {
		if !seen {
			t.Errorf("Refs() dropped expected namespace %q: %v", ns, got)
		}
	}
}

// TestExpr_BracketArrowBackwardCompat verifies the new tokens are still parse
// errors outside their valid positions, so no previously-invalid expression
// silently changes meaning.
func TestExpr_BracketArrowBackwardCompat(t *testing.T) {
	for _, src := range []string{"1 => 2", "a = b", "[1, 2]"} {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", src)
		}
	}
}
