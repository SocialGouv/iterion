package scripthost

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/dop251/goja"
)

// MaxCallStack bounds recursion. goja's default is unbounded, so a three-line
// recursive script would exhaust the Go stack and take the process down by a
// route no rlimit names.
//
// It bounds CALLS, not parsing: 120 KiB of nested parentheses, or a
// `JSON.parse` of deeply nested brackets, recurse in Go rather than in the
// interpreter's call stack and are stopped by [MaxGoStackBytes] and the memory
// bound instead.
const MaxCallStack = 2048

// MaxGoStackBytes bounds the child's Go stack, which is what parse-time and
// JSON.parse recursion consume. Without it those overflow as an unrecoverable
// `fatal error` that no recover can convert into a refusal; with it the child
// still dies, but it dies at a bound this package chose rather than at
// whatever the host's default happened to be.
const MaxGoStackBytes = 64 << 20

// newRuntime builds the interpreter a script runs in.
//
// It takes a BARE [goja.New]: measured on the version this tree pins, a bare
// runtime has no require, no console, no process and no timers — those arrive
// with goja_nodejs's event loop, which this package deliberately does not use.
// A breadth-first walk of the reachable object graph from every built-in root,
// following own properties, symbols, prototypes and both accessor halves,
// found 517 objects, zero functions whose source is not `[native code]`, and
// only standard JS brands. So "a script cannot read a file" and "a script is
// deterministic" are properties of what was never installed.
//
// The two remaining sources of ambient truth are seeded explicitly:
//   - Math.random, from a seed the CALLER chose and therefore holds, which is
//     what makes a run replayable;
//   - Date, from a fixed instant, so a script cannot read the wall clock.
//
// Both are the model's view of the world, not a security boundary: the
// boundary is [Confine].
func newRuntime(seed [32]byte, now time.Time) *goja.Runtime {
	vm := goja.New()
	vm.SetMaxCallStackSize(MaxCallStack)

	prng := rand.New(rand.NewChaCha8(seed))
	vm.SetRandSource(prng.Float64)

	// A fixed instant rather than a frozen offset: Date.now() returning a
	// monotonically rising value would let a script measure how long a call
	// took, which is a side channel the tool binding would inherit.
	vm.SetTimeSource(func() time.Time { return now })

	return vm
}

// runScript evaluates src and returns its value JSON-encoded.
//
// goja signals failure by PANICKING, and script-controlled code runs at two
// points after RunString has returned: the JSON encoding reads the value
// (which can invoke a getter) and reporting an error reads the thrown object
// (which can invoke toString). Measured before either was addressed: twelve
// one-line scripts — a throwing getter, a polluted Array.prototype,
// `throw Object.create(null)`, a recursive toString — crashed the child with
// no document, and the parent then reported `script_host_died`, dressing an
// ordinary script fault as a host bug.
//
// Those two sites are closed at the SITE: [encodeValue] goes through the
// engine's own JSON.stringify, which raises a catchable JS exception instead
// of panicking, and [safeErrorText] carries its own recover. The recover below
// is therefore a belt with NO witness — a mutation bench finds it as a
// survivor, and nine further adversarial shapes (a throwing toJSON, a cyclic
// object, a Proxy whose ownKeys throws, a Symbol-keyed getter, 5000 levels of
// nesting, a BigInt) produce refusals without it. It is kept because a dead
// child is the one outcome that cannot be reported, not because anything
// measured needs it.
//
// The wall deadline here is BEST EFFORT and says so, because
// [goja.Runtime.Interrupt] does not preempt a native call: measured on the
// pinned engine, a backreference regex ran past 60 s of wall clock with an
// interrupt posted at 1 s. The enforcing bound is RLIMIT_CPU in [Confine] and
// the parent's own deadline; this interrupt only buys a clean refusal for the
// scripts that happen to be preemptible.
func runScript(vm *goja.Runtime, src string, wall time.Duration, resultBytes int64) (resp Response) {
	defer func() {
		if r := recover(); r != nil {
			// A panic from the engine is the script's fault far more often
			// than ours, and either way it must not end the process: a dead
			// child produces no document, and no document is the one verdict
			// that means "the host is broken".
			resp = Refuse(RefusalScriptError,
				"the script, its value or its error could not be evaluated: %v", r)
		}
	}()

	var timer *time.Timer
	if wall > 0 {
		timer = time.AfterFunc(wall, func() { vm.Interrupt(RefusalWallClock) })
	}

	value, err := vm.RunString(src)

	// Disarm BEFORE any further JS runs. The encoding below can invoke a
	// getter, and an interrupt landing there used to panic out of a function
	// whose own refusal was already decided.
	if timer != nil {
		timer.Stop()
	}
	vm.ClearInterrupt()

	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			// An interrupt is how anything NON-SCRIPT stops a script, and the
			// wall bound is no longer the only one: a broken tool channel
			// interrupts too, precisely so no `catch` or `finally` can swallow
			// it. The VALUE the interrupter posted is what says which — read
			// it, rather than assuming the only interrupter is the timer.
			// Assuming it reported a broken deployment as "the wall-clock
			// bound elapsed", and an operator reading that raises the wall
			// bound forever.
			if chErr, ok := interrupted.Value().(error); ok && errors.Is(chErr, errToolChannelBroken) {
				return Refuse(RefusalHostDied,
					"the script was stopped because its tool channel failed: %s", chErr.Error())
			}
			return Refuse(RefusalWallClock, "script interrupted after %s: the wall-clock bound elapsed", wall)
		}
		// Matched on the type, because the message is a bare stack trace:
		// goja's StackOverflowError carries no sentence, so the cause has to be
		// supplied here or the model receives a file:line and nothing else.
		var overflow *goja.StackOverflowError
		if errors.As(err, &overflow) {
			return Refuse(RefusalCallStack,
				"the script recursed past the %d-frame call-stack bound: %s", MaxCallStack, safeErrorText(err))
		}
		// A goja.Exception is the script's own throw; a parser error is its
		// own syntax. Both are the script's fault and both carry a message the
		// model can act on, so they are reported rather than flattened.
		return Refuse(RefusalScriptError, "%s", safeErrorText(err))
	}

	encoded, err := encodeValue(vm, value)
	if err != nil {
		return Refuse(RefusalScriptError, "the script's value cannot be encoded as JSON: %s", safeErrorText(err))
	}
	if resultBytes > 0 && int64(len(encoded)) > resultBytes {
		return Refuse(RefusalResultBytes,
			"the script returned %d bytes of JSON, over the %d-byte bound: return less, or aggregate in the script",
			len(encoded), resultBytes)
	}
	return Response{Value: encoded}
}

// safeErrorText renders an error whose Error() may itself run script code.
//
// `goja.Exception.Error()` converts the thrown value to a string, which runs
// the script's `toString`/`valueOf` — so it can throw, recurse, or hit an
// object with neither, and goja signals each by panicking. Reporting an error
// must not be able to fail.
func safeErrorText(err error) (text string) {
	defer func() {
		if r := recover(); r != nil {
			text = fmt.Sprintf("the thrown value could not be rendered (%v)", r)
		}
	}()
	return err.Error()
}

// encodeValue turns a script value into JSON using the ENGINE's own
// JSON.stringify rather than Go's json.Marshal over goja's Export().
//
// Three reasons, each measured on the Export path:
//
//   - A throwing getter or Proxy trap reached during Export panics in Go.
//     Through JSON.stringify the same throw is an ordinary JS exception this
//     function returns as an error.
//   - Export + Marshal is not the JSON of the value the script returned.
//     Object key order became lexicographic instead of insertion order
//     (`{zebra,apple,"10"}` came back sorted); a Proxy, an Error, a RegExp, a
//     Promise and an ArrayBuffer all flattened to `{}` — a silent wrong
//     answer; an object carrying any method was refused whole with
//     `json: unsupported type: func(...)`; and NaN and Infinity were refused
//     where JSON.stringify writes null.
//   - Export materialises the whole value in Go before anything can measure
//     it, so the result bound amplified rather than bounded: a 31-character
//     script declaring `a.length = 50000000` cost 2 GiB before being refused.
//     Stringifying inside the VM keeps that allocation on the script's own
//     heap, where RLIMIT_DATA already answers for it.
//
// undefined becomes JSON null, so a script that returns nothing is still a
// value rather than a refusal.
func encodeValue(vm *goja.Runtime, v goja.Value) (string, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "null", nil
	}

	jsonObj := vm.Get("JSON")
	if jsonObj == nil {
		return "", errors.New("the runtime has no JSON object")
	}
	stringify, ok := goja.AssertFunction(jsonObj.ToObject(vm).Get("stringify"))
	if !ok {
		return "", errors.New("JSON.stringify is not callable")
	}

	out, err := stringify(goja.Undefined(), v)
	if err != nil {
		return "", err
	}
	// JSON.stringify answers undefined for a function, a symbol or undefined
	// itself. The script returned something unrepresentable, which is a value
	// of null rather than an error — the same answer the browsers give.
	if out == nil || goja.IsUndefined(out) {
		return "null", nil
	}
	return out.String(), nil
}
