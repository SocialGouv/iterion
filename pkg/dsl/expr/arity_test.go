package expr

import (
	"errors"
	"strings"
	"testing"
)

// The registry's declared arity is what the compiler refuses a bot on, and
// what the evaluator dispatches from. A builtin whose BODY disagrees with
// its declaration reopens the exact hole this closes: the call compiles,
// then dies mid-run. Drive every builtin at each boundary of its declared
// range and one step outside it.
func TestBuiltinArityMatchesTheImplementation(t *testing.T) {
	for name, b := range builtins {
		if b.min < 1 {
			t.Errorf("%s: declared min = %d — every builtin takes at least one argument", name, b.min)
		}
		if b.max != arityUnbounded && b.max < b.min {
			t.Errorf("%s: declared max %d < min %d", name, b.max, b.min)
		}

		// One argument short of the floor: the body must refuse it too, so
		// a caller reaching the function directly cannot index past its
		// arguments.
		if _, err := b.fn(make([]any, b.min-1)); err == nil {
			t.Errorf("%s: the body accepted %d argument(s) while the registry declares min %d", name, b.min-1, b.min)
		}
		if b.max != arityUnbounded {
			if _, err := b.fn(make([]any, b.max+1)); err == nil {
				t.Errorf("%s: the body accepted %d argument(s) while the registry declares max %d", name, b.max+1, b.max)
			}
		}
	}
}

// The parser is the boundary the compiler reads, and it must report a TYPED
// arity error — the ir package raises C138 off the type, not off the text.
func TestParseRefusesABadArityWithATypedError(t *testing.T) {
	cases := []struct{ src, want string }{
		{"length(1, 2)", "length() takes 1 argument, got 2"},
		{"slice(1, 2)", "slice() takes 3 arguments, got 2"},
		{"max()", "max() takes at least 1 argument, got 0"},
		{"if(1, 2)", "if() takes 3 arguments, got 2"},
	}
	for _, c := range cases {
		_, err := Parse(c.src)
		if err == nil {
			t.Errorf("Parse(%q) = nil error, want an arity refusal", c.src)
			continue
		}
		var arity *ArityError
		if !errors.As(err, &arity) {
			t.Errorf("Parse(%q) error %v is not an *ArityError — the compiler cannot tell it from a syntax error", c.src, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q) error = %q, want %q", c.src, err, c.want)
		}
	}

	// Accepted shapes stay accepted — the variadic forms most of all, since
	// clamping (`min(max(floor, x), cap)`) is what they exist for.
	for _, ok := range []string{"max(1, 2, 3)", "min(vars.xs)", "concat(vars.a)", "if(1, 2, 3)", "slice(vars.a, 0, 1)"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q) = %v, want it accepted", ok, err)
		}
	}
}
