package scripthost

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// A Promise last expression is REFUSED, not encoded.
//
// Measured before this guard: `async function f(){return 1}; f()` came back
// as `"{}"` with no error — `JSON.stringify` renders a promise as an empty
// object, and this host runs no event loop (a bare goja.Runtime pumps no job
// queue), so the promise can never settle. `{}` was therefore not a value the
// program computed but the shape of an absence, returned as a success. The
// `await` form was already loud (a SyntaxError); this was the silent half,
// and a silent wrong answer is what the project's conventions forbid.
func TestAPromiseLastExpressionIsRefusedRatherThanEncodedAsEmpty(t *testing.T) {
	t.Parallel()
	vm := goja.New()
	v, err := vm.RunString(`(async function(){ return 1 })()`)
	if err != nil {
		t.Fatalf("the probe itself did not run: %v", err)
	}
	if _, err := encodeValue(vm, v); err == nil {
		t.Fatalf("a Promise was encoded instead of refused — it renders as {} and reads as a successful empty object")
	} else if !strings.Contains(err.Error(), "Promise") {
		t.Errorf("the refusal does not name the promise, so a model cannot act on it: %v", err)
	}

	// The other arm: an ordinary value still encodes. A guard that refused
	// more than promises would close the service it protects.
	plain, err := vm.RunString(`({a: 1, b: [2, 3]})`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := encodeValue(vm, plain)
	if err != nil {
		t.Fatalf("an ordinary object was refused: %v", err)
	}
	if !strings.Contains(got, `"a":1`) {
		t.Errorf("encoded %q, want the object's own fields", got)
	}
}
