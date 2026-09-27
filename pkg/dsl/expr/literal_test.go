package expr

import (
	"reflect"
	"strings"
	"testing"
)

// TestExpr_CollectionLiterals is the positive class table for the list and
// object literals (#1525): parse, then evaluate — a literal with no
// reference evaluates against a nil context.
func TestExpr_CollectionLiterals(t *testing.T) {
	cases := []struct {
		src    string
		expect any
	}{
		{`[]`, []any{}},
		{`[1, 2, 3]`, []any{int64(1), int64(2), int64(3)}},
		{`['a', "b"]`, []any{"a", "b"}},
		{`[1, 'a', true, 2.5]`, []any{int64(1), "a", true, 2.5}},
		{`{}`, map[string]any{}},
		{`{a: 1, 'b': 2}`, map[string]any{"a": int64(1), "b": int64(2)}},
		{`{a: [1, {b: 'x'}]}`, map[string]any{"a": []any{int64(1), map[string]any{"b": "x"}}}},
		{`[[1, 2], []]`, []any{[]any{int64(1), int64(2)}, []any{}}},
		// Postfix access reaches into a literal directly.
		{`[1, 2][0]`, int64(1)},
		{`{a: 1}.a`, int64(1)},
		{`{a: 1}["a"]`, int64(1)},
		{`[9][5]`, nil}, // out of bounds resolves to nil, like an absent path
		// Builtins and combinators read a literal like any other value.
		{`length([1, 2])`, int64(2)},
		{`contains(['a', 'b'], 'b')`, true},
		{`keys({b: 1, a: 2})`, []any{"a", "b"}},
		{`join(['a', 'b'], '-')`, "a-b"},
		{`map([1, 2, 3], x => x * 2)`, []any{int64(2), int64(4), int64(6)}},
		{`filter([1, 2, 3], x => x > 1)`, []any{int64(2), int64(3)}},
		{`reduce([1, 2, 3], 0, (acc, x) => acc + x)`, int64(6)},
		{`map([{n: 'a'}, {n: 'b'}], x => x.n)`, []any{"a", "b"}},
	}
	for _, c := range cases {
		ast, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.src, err)
		}
		got, err := ast.Eval(nil)
		if err != nil {
			t.Fatalf("Eval(%q) error: %v", c.src, err)
		}
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("Eval(%q) = %#v, want %#v", c.src, got, c.expect)
		}
	}
}

// TestExpr_CollectionLiteralsWithRefs: an element or a value is any
// expression, references included.
func TestExpr_CollectionLiteralsWithRefs(t *testing.T) {
	ctx := makeCtx(
		map[string]any{"n": int64(7)},
		map[string]any{"x": "hello"},
		map[string]map[string]any{"prev": {"f": true}},
		nil,
	)
	cases := []struct {
		src    string
		expect any
	}{
		{`[input.x, vars.n]`, []any{"hello", int64(7)}},
		{`{k: outputs.prev.f, n: vars.n + 1}`, map[string]any{"k": true, "n": int64(8)}},
		{`[missing.ref]`, []any{nil}},
	}
	for _, c := range cases {
		ast, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.src, err)
		}
		got, err := ast.Eval(ctx)
		if err != nil {
			t.Fatalf("Eval(%q) error: %v", c.src, err)
		}
		if !reflect.DeepEqual(got, c.expect) {
			t.Errorf("Eval(%q) = %#v, want %#v", c.src, got, c.expect)
		}
	}
}

// TestExpr_CollectionLiteralErrors is the negative class table: each shape
// must be refused at parse, with a message that names the shape.
func TestExpr_CollectionLiteralErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantErr string
	}{
		{`[1,`, "unexpected token"},         // EOF inside the literal
		{`[1,]`, "unexpected token"},        // no trailing comma — the call-argument rule
		{`[1 2]`, "expected ',' or ']'"},    // missing separator
		{`{a 1}`, "expected ':'"},           // missing colon
		{`{a: 1,}`, "keys are identifiers"}, // no trailing comma
		{`{a: 1, a: 2}`, `repeats key "a"`}, // a repeated key is an authoring error
		{`{1: 2}`, "keys are identifiers"},  // a key is fixed text, not a number
		{`{a: }`, "unexpected token"},       // a pair needs a value
		{`{a: 1`, "expected ',' or '}'"},
		{`{a: 1]`, "expected ',' or '}'"},
		{`}`, "unexpected token"},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("Parse(%q) succeeded, want error containing %q", c.src, c.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("Parse(%q) error = %q, want it to contain %q", c.src, err, c.wantErr)
		}
	}
}

// TestExpr_CollectionLiteralDepth: nested literals recurse through parseExpr,
// so the depth cap that already bounds `(((...)))` bounds `[[[...]]]` the
// same way — an adversarial source cannot blow the goroutine stack.
func TestExpr_CollectionLiteralDepth(t *testing.T) {
	deep := strings.Repeat("[", maxExprDepth) + "1" + strings.Repeat("]", maxExprDepth)
	if _, err := Parse(deep); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("Parse(deeply nested lists) err = %v, want the depth cap", err)
	}
}

// TestExpr_CollectionEqualityIsNotByValue pins the trap C306 guards at
// compile time: equals() never walks into a slice or a map, so two
// collections never compare equal — not even a literal against itself.
func TestExpr_CollectionEqualityIsNotByValue(t *testing.T) {
	for _, src := range []string{`[1] == [1]`, `{a: 1} == {a: 1}`, `[] == []`} {
		ast := MustParse(src)
		got, err := ast.Eval(nil)
		if err != nil {
			t.Fatalf("Eval(%q) error: %v", src, err)
		}
		if got != false {
			t.Errorf("Eval(%q) = %v, want false (collections never compare by value)", src, got)
		}
	}
	ast := MustParse(`[1] != [1]`)
	got, err := ast.EvalBool(nil)
	if err != nil || !got {
		t.Errorf("EvalBool(`[1] != [1]`) = %v, %v — want true", got, err)
	}
}

// TestExpr_CollectionTruthiness: the truthy() rules for collections predate
// the literal; pin that a literal rides them — empty is falsy, non-empty
// truthy, so `when "[]"` means false.
func TestExpr_CollectionTruthiness(t *testing.T) {
	cases := []struct {
		src    string
		expect bool
	}{
		{`[]`, false},
		{`{}`, false},
		{`[1]`, true},
		{`{a: 1}`, true},
	}
	for _, c := range cases {
		got, err := MustParse(c.src).EvalBool(nil)
		if err != nil {
			t.Fatalf("EvalBool(%q) error: %v", c.src, err)
		}
		if got != c.expect {
			t.Errorf("EvalBool(%q) = %v, want %v", c.src, got, c.expect)
		}
	}
}

// TestExpr_CollectionLiteralRefs: Refs() surfaces the references a literal
// holds, so the compiler validates them like any other position.
func TestExpr_CollectionLiteralRefs(t *testing.T) {
	ast := MustParse(`[input.x, {k: outputs.n.f, l: vars.v}]`)
	refs := ast.Refs()
	want := []Ref{
		{Namespace: "input", Path: []string{"x"}},
		{Namespace: "outputs", Path: []string{"n", "f"}},
		{Namespace: "vars", Path: []string{"v"}},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("Refs() = %#v, want %#v", refs, want)
	}
}

// TestExpr_CollectionLiteralIteratedRefs: the collection a combinator walks
// is the literal — the paths its ELEMENTS name are read once, as values, so
// IteratedRefs does not report them. A literal inside a lambda body is no
// iterated position either; the parameter is not a ref at all.
func TestExpr_CollectionLiteralIteratedRefs(t *testing.T) {
	if refs := MustParse(`map([input.x], y => y)`).IteratedRefs(); len(refs) != 0 {
		t.Errorf("map([input.x], ...) IteratedRefs = %#v, want none (the list is iterated, input.x is read once)", refs)
	}
	refs := MustParse(`map(input.xs, y => [y, 1])`).IteratedRefs()
	want := []Ref{{Namespace: "input", Path: []string{"xs"}}}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("map(input.xs, ...) IteratedRefs = %#v, want %#v", refs, want)
	}
}

// TestExpr_CollectionLiteralSnapshot: the static projection mirrors a
// literal faithfully — kinds, keys, and children in source order — so the
// ir type-checker reads it without reaching into the AST.
func TestExpr_CollectionLiteralSnapshot(t *testing.T) {
	root := ToSnapshot(MustParse(`{a: [1, 'x'], b: {}}`))
	if root.Kind != SnapObject || len(root.Keys) != 2 || root.Keys[0] != "a" || root.Keys[1] != "b" {
		t.Fatalf("object snapshot = %#v, want SnapObject with keys [a b]", root)
	}
	list := root.Children[0]
	if list.Kind != SnapList || len(list.Children) != 2 ||
		list.Children[0].Kind != SnapInt || list.Children[1].Kind != SnapString || list.Children[1].Str != "x" {
		t.Fatalf("list snapshot = %#v, want SnapList of [SnapInt, SnapString x]", list)
	}
	if empty := root.Children[1]; empty.Kind != SnapObject || len(empty.Children) != 0 || len(empty.Keys) != 0 {
		t.Fatalf("empty object snapshot = %#v, want SnapObject with no entries", empty)
	}
	if root := ToSnapshot(MustParse(`[]`)); root.Kind != SnapList || len(root.Children) != 0 {
		t.Fatalf("empty list snapshot = %#v, want SnapList with no elements", root)
	}
}
