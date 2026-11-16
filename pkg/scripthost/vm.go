package scripthost

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
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
		// A tool-channel failure reaches here TOO, when the script had no
		// `catch` to make the bridge's interrupt win the race: the bridge's
		// panic surfaces as a *goja.Exception whose thrown value wraps
		// errToolChannelBroken, and the InterruptedError branch above never
		// matches. Measured both ways before this existed: the SAME broken
		// channel came back script_error without a catch and
		// script_host_died with one — and script_error is the class
		// errToolChannelBroken's doc forbids, because a script told "your
		// call failed" would retry a broken deployment in a loop, which is
		// exactly the shape a bare `call(...)` — what a model writes — makes
		// the common case. Unwrap the exception and read the thrown value.
		// Export here is safe: the value was built by the bridge from a Go
		// error, not by running script code.
		var exception *goja.Exception
		if errors.As(err, &exception) {
			// The read is GUARDED, because Export() on a script-thrown value
			// RUNS script code: goja walks the thrown object's own properties
			// and invokes getters and Proxy traps (the hazard this file's
			// encodeValue comment documents as the reason Export was
			// abandoned for encoding — and the reason safeErrorText carries
			// its own recover). Two measured failures without the guard:
			// `throw {get x(){throw 1}}` panicked through Export into the
			// outer belt, degrading the message to "could not be evaluated",
			// and `throw {get x(){while(true){}}}` looped PAST the wall bound
			// — the timer is stopped and the interrupt cleared before this
			// error path — for the parent to later report wall_clock_exceeded
			// about what is a script throw. So: recover around the read, and
			// re-arm the wall interrupt for its duration. A panic here falls
			// through to the generic rendering below — the verdict class
			// never moves.
			wrapped := func() (wrapped error) {
				defer func() {
					if recover() != nil {
						wrapped = nil
					}
				}()
				var guard *time.Timer
				if wall > 0 {
					guard = time.AfterFunc(wall, func() { vm.Interrupt(RefusalWallClock) })
				}
				if guard != nil {
					defer guard.Stop()
				}
				// Unwrap reads ONLY the genuine GoError's "value" property
				// (after an instanceof GoError check — vendor runtime.go): it
				// never walks the thrown object's other properties, so a
				// large or deeply nested throw is NOT materialised into Go.
				// The first draft used Export() here and the gate review
				// measured the flaw on this tree: Export recursively
				// materialises every own property (the repo's encodeValue
				// comment records a.length = 50000000 costing 2 GiB on this
				// exact path) with NO JS running, so the interrupt guard
				// could not preempt it and a script fault came back in a
				// memory/host class. Unwrap never runs script code; the
				// closure stays for a forged GoError prototype whose getter
				// does.
				return exception.Unwrap()
			}()
			if wrapped != nil && errors.Is(wrapped, errToolChannelBroken) {
				return Refuse(RefusalHostDied,
					"the script was stopped because its tool channel failed: %s", wrapped.Error())
			}
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

// promiseType is what ExportType answers for a promise. Held as a value so
// the comparison costs no allocation and no evaluation.
var promiseType = reflect.TypeOf((*goja.Promise)(nil))

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
// promiseRefusal is the one sentence both guards answer with, so a program
// cannot tell which of them refused it — and so the depth half can be
// strengthened or weakened without changing what a model reads.
func promiseRefusal(what string) error {
	return fmt.Errorf("%s is a Promise, and this host runs no event loop, so it can never settle: there is no async here — return the value itself, not a promise of it", what)
}

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

	// A PROMISE is refused rather than encoded, AT EVERY DEPTH. `JSON.stringify`
	// renders one as `{}`, so a value holding a promise came back as a
	// successful empty object — a silent wrong answer, which is the one thing
	// this repository's conventions forbid outright. There is no event loop
	// here (a bare goja.Runtime pumps no job queue), so the promise could
	// never settle and `{}` is not a value the script computed: it is the
	// shape of an absence. `await` is already a syntax error; this was the
	// half that was silent.
	//
	// The guard is a REPLACER rather than a test on the root, and that is the
	// correction of a first draft that checked the root only. Measured: a bare
	// `f()` was refused while `({report: f()})` came back `{"report":{}}` and
	// `[f()]` came back `[{}]` — and the object form is the LIKELIER shape,
	// since a program's last expression is normally a summary of what it did.
	// Worse, this function also encodes a tool-call INPUT, so
	// `call("t", {q: fetchAll()})` handed a real tool `{"q":{}}`. One site
	// fixed and the class left open is the defect this repository names
	// "corriger le site, pas la classe"; the replacer is the throttle point
	// both callers and every depth go through.
	//
	// ExportType, not Export: exporting MATERIALISES the value, which
	// evaluates an exotic object's getters — a `new Map([[{get x(){throw 1}},1]])`
	// threw from inside the first draft of this guard and changed the error
	// path a sibling test pins. ExportType reads the shape without running
	// anything, and was measured to trap on none of the exotics probed.
	//
	// KNOWN GAPS of the DEPTH half, stated rather than hidden, and all of them
	// require a program to put a promise somewhere JSON does not render: a
	// `goja.Proxy` wrapping one (it answers `goja.Proxy` from ExportType), a
	// promise inside a `Map` or a `Set` (both encode as `{}` whatever they
	// hold), and a program that replaces `JSON.stringify` or defines
	// `toJSON`. The ROOT test above is what holds regardless. Closing the
	// Proxy case needs a `then`-own-property probe, which runs script code
	// inside the encoder — the hazard the Export path was abandoned for.
	// TWO guards, and the pair is the point: one the script cannot reach and
	// one that reaches further.
	//
	// The ROOT test runs in Go before any script code, so nothing a program
	// does can disarm it. The REPLACER covers depth, and it is ADVISORY by
	// construction: it runs only if whatever `JSON.stringify` names calls it,
	// and on whatever `toJSON` left behind — both names belong to the script.
	// A draft that replaced the root test with the replacer alone was
	// measured and regressed exactly there: `var S = JSON.stringify;
	// JSON.stringify = v => S(v)` — a commonplace idiom, for BigInt support
	// or pretty-printing — re-opened the silent `{}`, as did
	// `p.toJSON = () => 7`. A deliberate override is a program's own
	// business; the accident is what the root test keeps.
	if v.ExportType() == promiseType {
		return "", promiseRefusal("the value itself")
	}

	promised := ""
	replacer := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		v := call.Argument(1)
		if promised == "" && v.ExportType() == promiseType {
			if key := call.Argument(0).String(); key != "" {
				// Clipped: a program may use a megabyte-long object key, and
				// an error carrying it whole was measured turning the verdict
				// into `result_too_large` — a refusal about the wrong thing.
				clipped, _ := clipToolText(key, 80)
				promised = "property " + clipped
			} else {
				promised = "the value itself"
			}
		}
		return v
	})

	out, err := stringify(goja.Undefined(), v, replacer)
	if err != nil {
		return "", err
	}
	if promised != "" {
		return "", promiseRefusal(promised)
	}
	// JSON.stringify answers undefined for a function, a symbol or undefined
	// itself. The script returned something unrepresentable, which is a value
	// of null rather than an error — the same answer the browsers give.
	if out == nil || goja.IsUndefined(out) {
		return "null", nil
	}
	return out.String(), nil
}
