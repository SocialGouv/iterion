package scripthost

import (
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// A Promise is REFUSED, not encoded — at every depth.
//
// Measured before the guard existed: `async function f(){return 1}; f()` came
// back as `"{}"` with no error, because `JSON.stringify` renders a promise as
// an empty object and this host runs no event loop (a bare goja.Runtime pumps
// no job queue), so the promise can never settle. `{}` was therefore not a
// value the program computed but the shape of an absence, returned as a
// success. The `await` form was already loud (a SyntaxError); this was the
// silent half, and a silent wrong answer is what the project's conventions
// forbid.
//
// The first draft of the guard tested the ROOT value only, and the depth rows
// below are why that was not enough: `({report: f()})` is the LIKELIER shape
// — a program's last expression is normally a summary of what it did — and it
// came back `{"report":{}}`. This function also encodes a tool-call INPUT, so
// `call("t", {q: fetchAll()})` handed a real tool `{"q":{}}`.
func TestAPromiseIsRefusedAtEveryDepthRatherThanEncodedAsEmpty(t *testing.T) {
	t.Parallel()
	vm := goja.New()

	refused := []struct {
		src  string
		name string // the key the refusal must name, so an author can find it
	}{
		{`(async function(){ return 1 })()`, "the value itself"},
		{`({ report: (async function(){ return 1 })() })`, "report"},
		{`[ (async function(){ return 1 })() ]`, "0"},
		{`({ a: 1, b: Promise.resolve({deep: 2}) })`, "b"},
		{`({ outer: { inner: Promise.resolve(1) } })`, "inner"},
		{`new Promise(function(){})`, "the value itself"},
	}
	for _, c := range refused {
		v, err := vm.RunString(c.src)
		if err != nil {
			t.Fatalf("%s: the probe itself did not run: %v", c.src, err)
		}
		out, err := encodeValue(vm, v)
		if err == nil {
			t.Errorf("%s encoded as %q instead of being refused — a promise renders as {} and reads as a successful empty value", c.src, out)
			continue
		}
		if !strings.Contains(err.Error(), "Promise") {
			t.Errorf("%s: the refusal does not name the promise, so a model cannot act on it: %v", c.src, err)
		}
		if !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: the refusal does not name %q, so an author cannot find which field it was: %v", c.src, c.name, err)
		}
	}

	// The other arm. A guard that refused more than promises would close the
	// service it protects, and a false refusal in a blocking path costs as
	// much as a hole.
	for _, src := range []string{
		`({a: 1, b: [2, 3]})`,
		`({a: 1, b: [2, {c: "x"}]})`,
		`"plain"`,
		`42`,
		`({})`,
		`[]`,
		`null`,
	} {
		v, err := vm.RunString(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if _, err := encodeValue(vm, v); err != nil {
			t.Errorf("%s was refused: %v", src, err)
		}
	}
}
