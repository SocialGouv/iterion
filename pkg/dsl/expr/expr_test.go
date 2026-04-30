package expr

import (
	"testing"
)

func makeCtx(vars, input map[string]interface{}, outputs map[string]map[string]interface{}, loop map[string]map[string]interface{}) *Context {
	resolveMap := func(m map[string]interface{}) func([]string) interface{} {
		return func(path []string) interface{} {
			if len(path) == 0 {
				return m
			}
			cur := interface{}(m)
			for _, key := range path {
				switch t := cur.(type) {
				case map[string]interface{}:
					cur = t[key]
				default:
					return nil
				}
			}
			return cur
		}
	}
	resolveOutputs := func(path []string) interface{} {
		if len(path) == 0 {
			return nil
		}
		out, ok := outputs[path[0]]
		if !ok {
			return nil
		}
		if len(path) == 1 {
			return out
		}
		return out[path[1]]
	}
	resolveLoop := func(path []string) interface{} {
		if len(path) < 2 {
			return nil
		}
		loopState, ok := loop[path[0]]
		if !ok {
			return nil
		}
		if len(path) == 2 {
			return loopState[path[1]]
		}
		// loop.<name>.previous_output.field
		nested, ok := loopState[path[1]].(map[string]interface{})
		if !ok {
			return nil
		}
		cur := interface{}(nested)
		for _, key := range path[2:] {
			m, ok := cur.(map[string]interface{})
			if !ok {
				return nil
			}
			cur = m[key]
		}
		return cur
	}
	return &Context{
		Vars:    resolveMap(vars),
		Input:   resolveMap(input),
		Outputs: resolveOutputs,
		Loop:    resolveLoop,
		Run: func(path []string) interface{} {
			if len(path) == 1 && path[0] == "id" {
				return "run-test-123"
			}
			return nil
		},
	}
}

func TestExpr_Literals(t *testing.T) {
	cases := []struct {
		src    string
		expect interface{}
	}{
		{"true", true},
		{"false", false},
		{"42", int64(42)},
		{"-3", int64(-3)},
		{"3.14", 3.14},
		{`"hello"`, "hello"},
		{`'world'`, "world"},
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
		if got != c.expect {
			t.Errorf("Eval(%q) = %v (%T), want %v (%T)", c.src, got, got, c.expect, c.expect)
		}
	}
}

func TestExpr_Boolean(t *testing.T) {
	ctx := makeCtx(
		map[string]interface{}{"flag": true},
		nil,
		map[string]map[string]interface{}{
			"reviewer": {"approved": true, "family": "claude"},
			"prev":     {"approved": true, "family": "gpt"},
		},
		nil,
	)
	cases := []struct {
		src    string
		expect bool
	}{
		{"true && false", false},
		{"true || false", true},
		{"!false", true},
		{"!(false && true)", true},
		{"true and false", false},
		{"true or false", true},
		{"not false", true},
		{"vars.flag", true},
		{"outputs.reviewer.approved && outputs.prev.approved", true},
		{`outputs.reviewer.family == "claude"`, true},
		{`outputs.reviewer.family != outputs.prev.family`, true},
		{`outputs.reviewer.approved && outputs.prev.approved && outputs.reviewer.family != outputs.prev.family`, true},
	}
	for _, c := range cases {
		ast, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.src, err)
		}
		got, err := ast.EvalBool(ctx)
		if err != nil {
			t.Fatalf("EvalBool(%q) error: %v", c.src, err)
		}
		if got != c.expect {
			t.Errorf("EvalBool(%q) = %v, want %v", c.src, got, c.expect)
		}
	}
}

func TestExpr_Comparisons(t *testing.T) {
	cases := []struct {
		src    string
		expect bool
	}{
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 2", true},
		{"3 >= 3", true},
		{"1 == 1", true},
		{"1 != 2", true},
		{"1.5 < 2", true},
		{"2 < 1.5", false},
		{`"a" < "b"`, true},
		{`"abc" == "abc"`, true},
	}
	for _, c := range cases {
		got, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.src, err)
		}
		v, err := got.EvalBool(nil)
		if err != nil {
			t.Fatalf("EvalBool(%q) error: %v", c.src, err)
		}
		if v != c.expect {
			t.Errorf("EvalBool(%q) = %v, want %v", c.src, v, c.expect)
		}
	}
}

func TestExpr_Arithmetic(t *testing.T) {
	cases := []struct {
		src    string
		expect interface{}
	}{
		{"1 + 2", int64(3)},
		{"10 - 4", int64(6)},
		{"3 * 4", int64(12)},
		{"10 / 3", int64(3)},
		{"10 % 3", int64(1)},
		{"1.5 + 2.5", 4.0},
		{`"foo" + "bar"`, "foobar"},
	}
	for _, c := range cases {
		ast, err := Parse(c.src)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", c.src, err)
		}
		v, err := ast.Eval(nil)
		if err != nil {
			t.Fatalf("Eval(%q) error: %v", c.src, err)
		}
		if v != c.expect {
			t.Errorf("Eval(%q) = %v, want %v", c.src, v, c.expect)
		}
	}
}

func TestExpr_LoopNamespace(t *testing.T) {
	ctx := makeCtx(
		nil, nil, nil,
		map[string]map[string]interface{}{
			"review_loop": {
				"iteration": int64(2),
				"max":       int64(6),
				"previous_output": map[string]interface{}{
					"approved": true,
					"family":   "gpt",
				},
			},
		},
	)
	src := `loop.review_loop.iteration < loop.review_loop.max && loop.review_loop.previous_output.approved`
	ast, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	got, err := ast.EvalBool(ctx)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	if !got {
		t.Errorf("expected true, got false")
	}
}

func TestExpr_Refs(t *testing.T) {
	ast, err := Parse(`vars.flag && outputs.reviewer.approved && loop.l.iteration > 0 && run.id != ""`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	refs := ast.Refs()
	expectedNamespaces := map[string]bool{"vars": true, "outputs": true, "loop": true, "run": true}
	gotNamespaces := make(map[string]bool)
	for _, r := range refs {
		gotNamespaces[r.Namespace] = true
	}
	for ns := range expectedNamespaces {
		if !gotNamespaces[ns] {
			t.Errorf("expected namespace %q in Refs(), got %v", ns, refs)
		}
	}
}

func TestExpr_ParseErrors(t *testing.T) {
	bad := []string{
		"1 +",
		"&",
		"|",
		"=",
		"\"unterminated",
		"1 2",
		"foo.",
	}
	for _, src := range bad {
		_, err := Parse(src)
		if err == nil {
			t.Errorf("expected Parse(%q) to fail, got nil", src)
		}
	}
}

func TestExpr_FuncCall_Length(t *testing.T) {
	ctx := makeCtx(
		map[string]interface{}{
			"items": []interface{}{"a", "b", "c"},
			"empty": []interface{}{},
			"name":  "iterion",
		},
		nil, nil, nil,
	)
	cases := []struct {
		src    string
		expect interface{}
	}{
		{"length(vars.items)", int64(3)},
		{"length(vars.empty)", int64(0)},
		{"length(vars.name)", int64(7)},
		{"length(vars.missing)", int64(0)},
		{"length(vars.items) + 1", int64(4)},
		{"length(vars.items) > 2", true},
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
		if got != c.expect {
			t.Errorf("Eval(%q) = %v (%T), want %v (%T)", c.src, got, got, c.expect, c.expect)
		}
	}
}

func TestExpr_FuncCall_Concat(t *testing.T) {
	ctx := makeCtx(
		map[string]interface{}{
			"a": []interface{}{"x", "y"},
			"b": []interface{}{"z"},
		},
		nil, nil, nil,
	)
	ast, err := Parse("concat(vars.a, vars.b, vars.missing)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	got, err := ast.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", got)
	}
	want := []interface{}{"x", "y", "z"}
	if len(arr) != len(want) {
		t.Fatalf("concat length = %d, want %d (%v)", len(arr), len(want), arr)
	}
	for i := range want {
		if arr[i] != want[i] {
			t.Errorf("concat[%d] = %v, want %v", i, arr[i], want[i])
		}
	}
}

func TestExpr_FuncCall_Unique(t *testing.T) {
	ctx := makeCtx(
		map[string]interface{}{
			"items": []interface{}{"a", "b", "a", "c", "b"},
		},
		nil, nil, nil,
	)
	ast, err := Parse("unique(vars.items)")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	got, err := ast.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", got)
	}
	want := []interface{}{"a", "b", "c"}
	if len(arr) != len(want) {
		t.Fatalf("unique length = %d, want %d (%v)", len(arr), len(want), arr)
	}
	for i := range want {
		if arr[i] != want[i] {
			t.Errorf("unique[%d] = %v, want %v", i, arr[i], want[i])
		}
	}

	// nil input → empty array
	nilAst := MustParse("unique(vars.missing)")
	out, err := nilAst.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval nil error: %v", err)
	}
	if a, ok := out.([]interface{}); !ok || len(a) != 0 {
		t.Errorf("unique(nil) = %v, want empty array", out)
	}
}

func TestExpr_FuncCall_Contains(t *testing.T) {
	ctx := makeCtx(
		map[string]interface{}{
			"items": []interface{}{"a", "b", "c"},
			"nums":  []interface{}{int64(1), int64(2), int64(3)},
		},
		nil, nil, nil,
	)
	cases := []struct {
		src    string
		expect interface{}
	}{
		{`contains(vars.items, "a")`, true},
		{`contains(vars.items, "z")`, false},
		{`contains(vars.nums, 2)`, true},
		{`contains(vars.missing, "x")`, false},
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
		if got != c.expect {
			t.Errorf("Eval(%q) = %v, want %v", c.src, got, c.expect)
		}
	}
}

func TestExpr_FuncCall_Nested(t *testing.T) {
	// Mirrors the iterion review-workflow accumulator:
	// unique(concat(loop.l.previous_output.cumulative, input.scanned_areas))
	ctx := makeCtx(
		nil,
		map[string]interface{}{
			"scanned_areas": []interface{}{"docs/", "pkg/dsl/"},
		},
		nil,
		map[string]map[string]interface{}{
			"l": {
				"previous_output": map[string]interface{}{
					"cumulative": []interface{}{"docs/", "pkg/runtime/"},
				},
			},
		},
	)
	ast, err := Parse("unique(concat(loop.l.previous_output.cumulative, input.scanned_areas))")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	got, err := ast.Eval(ctx)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	arr, ok := got.([]interface{})
	if !ok {
		t.Fatalf("expected []interface{}, got %T", got)
	}
	want := []interface{}{"docs/", "pkg/runtime/", "pkg/dsl/"}
	if len(arr) != len(want) {
		t.Fatalf("got %v, want %v", arr, want)
	}
	for i := range want {
		if arr[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, arr[i], want[i])
		}
	}
}

func TestExpr_FuncCall_FirstIterationNilCumulative(t *testing.T) {
	// Models the first iteration of the review loop where
	// loop.l.previous_output.cumulative is nil.
	ctx := makeCtx(
		nil,
		map[string]interface{}{"scanned_areas": []interface{}{"a", "b"}},
		nil,
		map[string]map[string]interface{}{
			"l": {"previous_output": map[string]interface{}{}},
		},
	)
	got, err := MustParse("unique(concat(loop.l.previous_output.cumulative, input.scanned_areas))").Eval(ctx)
	if err != nil {
		t.Fatalf("Eval error: %v", err)
	}
	arr, _ := got.([]interface{})
	if len(arr) != 2 || arr[0] != "a" || arr[1] != "b" {
		t.Errorf("got %v, want [a b]", arr)
	}
}

func TestExpr_FuncCall_Errors(t *testing.T) {
	// Parse-time: unknown function name.
	if _, err := Parse("frobnicate(1)"); err == nil {
		t.Errorf("expected unknown-function parse error")
	}

	// Parse-time: missing closing paren.
	if _, err := Parse("length(1"); err == nil {
		t.Errorf("expected missing-paren parse error")
	}

	// Eval-time: arg-count mismatches and type errors.
	ctx := makeCtx(
		map[string]interface{}{"s": "hi", "n": int64(5)},
		nil, nil, nil,
	)
	bad := []string{
		"length()",
		"length(vars.s, vars.s)",
		"length(vars.n)", // int is not array/string
		"concat()",
		"concat(vars.n)", // int is not array
		"unique()",
		"unique(vars.s)", // string is not array
		"contains(vars.s)",
		"contains(vars.n, 1)", // int is not array
	}
	for _, src := range bad {
		ast, err := Parse(src)
		if err != nil {
			// arg-count failures may surface at parse time for some shapes;
			// either layer is acceptable.
			continue
		}
		if _, err := ast.Eval(ctx); err == nil {
			t.Errorf("expected Eval(%q) to fail", src)
		}
	}
}

func TestExpr_FuncCall_Refs(t *testing.T) {
	ast, err := Parse("unique(concat(loop.l.previous_output.cumulative, input.scanned_areas))")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	gotNs := make(map[string]bool)
	for _, r := range ast.Refs() {
		gotNs[r.Namespace] = true
	}
	for _, ns := range []string{"loop", "input"} {
		if !gotNs[ns] {
			t.Errorf("expected ref namespace %q, got %v", ns, gotNs)
		}
	}
}
