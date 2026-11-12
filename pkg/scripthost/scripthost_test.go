package scripthost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// The globals a script can see, as a GOLDEN rather than a deny list.
//
// An earlier version probed ten chosen names, which proves nothing about the
// eleventh: the point of this package is that NOTHING of the host reaches the
// VM, and the tool binding that comes next will add exactly one thing. A
// golden notices a binding that appears under any name; a deny list only
// notices the names someone thought of.
//
// Measured: a bare goja runtime exposes 54 globals, all standard ES plus
// goja's own GoError. A breadth-first walk of the reachable graph from every
// root — own properties, symbols, prototypes, both accessor halves — found 517
// objects and zero functions whose source is not `[native code]`.
func TestTheScriptsGlobalsAreAGolden(t *testing.T) {
	vm := newRuntime([32]byte{}, epoch)

	v, err := vm.RunString(`Object.getOwnPropertyNames(globalThis).sort().join(" ")`)
	if err != nil {
		t.Fatal(err)
	}
	const want = "AggregateError Array ArrayBuffer BigInt BigInt64Array BigUint64Array Boolean DataView " +
		"Date Error EvalError Float32Array Float64Array Function GoError Infinity Int16Array " +
		"Int32Array Int8Array JSON Map Math NaN Number Object Promise Proxy RangeError " +
		"ReferenceError Reflect RegExp Set String Symbol SyntaxError TypeError URIError " +
		"Uint16Array Uint32Array Uint8Array Uint8ClampedArray WeakMap WeakSet decodeURI " +
		"decodeURIComponent encodeURI encodeURIComponent escape eval globalThis isFinite isNaN " +
		"parseFloat parseInt undefined unescape"
	if got := v.String(); got != want {
		t.Errorf("the script's globals changed.\n got: %s\nwant: %s\n\n"+
			"If a host binding was added on purpose, update this golden in the same change and say in the "+
			"commit what the script can now reach. If it was not, something reached the VM.", got, want)
	}

	// The capabilities whose ABSENCE is the design, named so a reader sees
	// what the golden is protecting.
	for _, name := range []string{
		"require", "console", "process", "setTimeout", "setInterval", "setImmediate",
		"queueMicrotask", "fetch", "XMLHttpRequest", "WebAssembly", "SharedArrayBuffer", "Atomics",
		"WeakRef", "FinalizationRegistry", "performance", "structuredClone",
	} {
		v, err := vm.RunString("typeof " + name)
		if err != nil {
			t.Fatalf("probing %q: %v", name, err)
		}
		if got := v.String(); got != "undefined" {
			t.Errorf("a script can see %q (typeof = %q): a capability reached the VM", name, got)
		}
	}
}

// Determinism is a property of what the host seeds, and both sources are
// seeded. A script that could read the wall clock would be able to measure how
// long a tool call took once the binding lands, which is a side channel the
// tool door cannot see.
func TestTheClockAndThePRNGAreTheHostsToChoose(t *testing.T) {
	vm := newRuntime([32]byte{7}, epoch)

	v, err := vm.RunString("Date.now()")
	if err != nil {
		t.Fatal(err)
	}
	if want := epoch.UnixMilli(); v.ToInteger() != want {
		t.Errorf("Date.now() = %d, want the fixed epoch %d", v.ToInteger(), want)
	}

	// Two reads of the clock inside one script must not differ, or a script
	// can time its own execution.
	same, err := vm.RunString("(function(){var a=Date.now(); for(var i=0;i<100000;i++){}; return Date.now()===a})()")
	if err != nil {
		t.Fatal(err)
	}
	if !same.ToBoolean() {
		t.Error("Date.now() advanced inside a script: a script can measure elapsed time")
	}

	// The timezone is NOT asserted here: newRuntime does not force UTC —
	// Confine does, before Landlock closes the zoneinfo read — and this unit
	// never confines. The child-side assertion is
	// TestConfineResolvesTheTimezoneBeforeTheFilesystemCloses.

	first, err := newRuntime([32]byte{7}, epoch).RunString("[Math.random(),Math.random()].join(',')")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRuntime([32]byte{7}, epoch).RunString("[Math.random(),Math.random()].join(',')")
	if err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Errorf("the same seed gave different sequences: %q then %q", first.String(), second.String())
	}
	other, err := newRuntime([32]byte{8}, epoch).RunString("[Math.random(),Math.random()].join(',')")
	if err != nil {
		t.Fatal(err)
	}
	if other.String() == first.String() {
		t.Error("two different seeds gave the same sequence: the seed is not reaching the PRNG")
	}
}

// goja signals failure by PANICKING, and script-controlled code runs at two
// points after RunString has returned: the JSON encoding reads the value
// (which can invoke a getter) and reporting an error reads the thrown object
// (which can invoke toString). Before this was wrapped, each of these
// one-liners killed the child with no document, and the parent then reported
// `script_host_died` — dressing an ordinary script fault as a broken host.
//
// Every case must come back as a REFUSAL, from a live process.
func TestAScriptCannotKillTheHostByThrowingTheWrongThing(t *testing.T) {
	for name, src := range map[string]string{
		"a throwing getter on the value":   `({ get x() { throw new Error("boom") } })`,
		"a throwing index getter":          `var a=[]; Object.defineProperty(a,"0",{enumerable:true,get:function(){throw 1}}); a`,
		"a polluted Array.prototype":       `Object.defineProperty(Array.prototype,"0",{get:function(){throw 1}}); [,]`,
		"a null-prototype object thrown":   `throw Object.create(null)`,
		"a throwing toString on the throw": `throw { toString: function(){ throw new Error("nested") } }`,
		"a recursive toString":             `throw { toString: function(){ return this.toString() } }`,
		"a throwing message getter":        `var e=new Error("a"); Object.defineProperty(e,"message",{get:function(){throw 1}}); throw e`,
		"a throwing Proxy trap thrown":     `throw new Proxy({}, {get:function(){throw 1}})`,
		"an object with no conversion":     `var o={}; o.toString=o.valueOf=null; throw o`,
		"a getter that recurses forever":   `({ get x() { function f(){ return f() } return f() } })`,
		"eval of a null-prototype throw":   `eval("throw Object.create(null)")`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := runScript(newRuntime([32]byte{}, epoch), src, 10*time.Second, 1<<20)
			if resp.Refusal == "" {
				t.Fatalf("expected a refusal, got a value: %q", resp.Value)
			}
			if resp.Message == "" {
				t.Error("the refusal carries no message, so the model learns nothing")
			}
		})
	}
}

// The document is the JSON of the value the script returned, encoded by the
// ENGINE. Go's json.Marshal over goja's Export disagreed with JSON.stringify
// in ways that were silently wrong — and each row below was one of them.
func TestTheValueIsEncodedTheWayJavaScriptWould(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"a number", "1+1", "2"},
		{"a string", "'ok'", `"ok"`},
		{"an array", "[1,2,3]", "[1,2,3]"},
		{"an object", "({a:1})", `{"a":1}`},
		{"undefined", "undefined", "null"},
		{"null", "null", "null"},
		{"no return", "(function(){})()", "null"},
		// Insertion order, not Go's lexicographic sort — and integer-like keys
		// first, which is what the JS spec says.
		{"key order", `({zebra:1, apple:2, mango:3, "10":4, "2":5})`, `{"2":5,"10":4,"zebra":1,"apple":2,"mango":3}`},
		// Each of these came back as `{}` through Export — a silent wrong
		// answer, which this repo's rules forbid.
		{"an Error", `new Error("e")`, `{}`},
		{"a RegExp", `/re/g`, `{}`},
		// NaN and Infinity are null in JSON, not a refusal.
		{"NaN", "NaN", "null"},
		{"Infinity", "Infinity", "null"},
		// An object carrying a method keeps its data and drops the function,
		// instead of refusing the whole value.
		{"an object with a method", `({a:1, f:function(){}})`, `{"a":1}`},
		{"a toJSON hook", `({toJSON:function(){return {x:1}}})`, `{"x":1}`},
		// A Map and a Set carry no own enumerable properties, so JavaScript
		// encodes them as `{}` and never reaches a key's getter. That is the
		// engine's answer, not ours — and it is why a throwing Map key, which
		// crashed the Export path, is simply a value here.
		{"a Map", `new Map([["a",1]])`, `{}`},
		{"a Map with a throwing key", `new Map([[{get x(){throw 1}},1]])`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := runScript(newRuntime([32]byte{}, epoch), tc.src, time.Second, 1<<20)
			if !resp.OK() {
				t.Fatalf("%s: %+v", tc.src, resp)
			}
			if resp.Value != tc.want {
				t.Errorf("%s gave %q, want %q", tc.src, resp.Value, tc.want)
			}
		})
	}
}

// Unbounded recursion is a route to killing the process that no rlimit names:
// the Go stack goes, not the heap. goja's default call-stack size is
// unlimited, so the bound is ours to set.
func TestRecursionIsBounded(t *testing.T) {
	resp := runScript(newRuntime([32]byte{}, epoch), "(function f(n){return f(n+1)})(0)", 10*time.Second, 1<<20)
	if resp.Refusal != RefusalCallStack {
		t.Fatalf("runaway recursion gave %+v, want a %s refusal", resp, RefusalCallStack)
	}
	// The message has to carry a cause, not just a location: goja's own text
	// for this error is a bare stack trace, which is why the refusal supplies
	// the sentence.
	if !strings.Contains(strings.ToLower(resp.Message), "call-stack bound") {
		t.Errorf("the refusal does not say what bound bit: %q", resp.Message)
	}
}

func TestClampFillsTheUnsetAndCapsTheGreedy(t *testing.T) {
	def, ceil := DefaultLimits(), Ceiling()

	got, clamped := Limits{}.Clamp()
	if clamped {
		t.Error("an empty request reported a clamp: nothing was asked for, so nothing was reduced")
	}
	if got != def {
		t.Errorf("an empty request gave %+v, want the defaults %+v", got, def)
	}

	greedy := Limits{
		Wall: time.Hour, CPU: time.Hour,
		Memory: 1 << 40, ScriptBytes: 1 << 30, ResultBytes: 1 << 30,
	}
	got, clamped = greedy.Clamp()
	if !clamped {
		t.Error("a request over every ceiling reported no clamp: the operator would never be warned")
	}
	if got != ceil {
		t.Errorf("a greedy request gave %+v, want the ceiling %+v", got, ceil)
	}

	// Below the ceiling, the caller's choice survives untouched: a bound that
	// quietly replaced an explicit value would be the defect philosophy 1
	// names.
	modest := Limits{Wall: time.Second, CPU: 2 * time.Second, Memory: 1 << 20, ScriptBytes: 10, ResultBytes: 20}
	got, clamped = modest.Clamp()
	if clamped {
		t.Error("a request under every ceiling reported a clamp")
	}
	if got != modest {
		t.Errorf("a modest request was rewritten to %+v, want %+v", got, modest)
	}
}

// RLIMIT_CPU's unit is whole seconds. Rounding down would turn a sub-second
// request into zero, and zero means "kill on the first tick" — the opposite of
// what asking for half a second means.
func TestCPUSecondsRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want uint64
	}{
		{0, 0},
		{-time.Second, 0},
		{time.Nanosecond, 1},
		{time.Millisecond, 1},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{10 * time.Second, 10},
	} {
		if got := (Limits{CPU: tc.in}).CPUSeconds(); got != tc.want {
			t.Errorf("CPUSeconds(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The wire names say what the unit IS. They said `wall_ms` while
// time.Duration encodes nanoseconds, so a hand-written request got a bound a
// million times tighter than it asked for: `"wall_ms":30000` was enforced as
// 30µs.
func TestTheDurationFieldsSayTheirRealUnit(t *testing.T) {
	encoded, err := json.Marshal(Limits{Wall: 30 * time.Second, CPU: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"wall_ns":30000000000`, `"cpu_ns":10000000000`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the encoded limits do not contain %s: %s", want, encoded)
		}
	}

	var back Limits
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if back.Wall != 30*time.Second || back.CPU != 10*time.Second {
		t.Errorf("the round trip changed the bounds: %+v", back)
	}
}

func TestAnOversizedResultIsRefusedWhereItIsProduced(t *testing.T) {
	resp := runScript(newRuntime([32]byte{}, epoch), `'x'.repeat(5000)`, 10*time.Second, 100)
	if resp.Refusal != RefusalResultBytes {
		t.Fatalf("a 5000-byte result under a 100-byte bound gave %+v, want %s", resp, RefusalResultBytes)
	}
	if resp.Value != "" {
		t.Error("the refusal still carries the value: the oversized payload crossed the pipe anyway")
	}
}

// A script's own throw is the script's fault and comes back with its message,
// not flattened into a bound.
func TestAScriptsOwnThrowIsNotABound(t *testing.T) {
	resp := runScript(newRuntime([32]byte{}, epoch), `throw new Error('mine')`, time.Second, 1<<20)
	if resp.Refusal != RefusalScriptError {
		t.Fatalf("a throw gave %+v, want %s", resp, RefusalScriptError)
	}
	if !strings.Contains(resp.Message, "mine") {
		t.Errorf("the script's own message is gone: %q", resp.Message)
	}
}

func TestASyntaxErrorIsTheScriptsFault(t *testing.T) {
	resp := runScript(newRuntime([32]byte{}, epoch), `function (`, time.Second, 1<<20)
	if resp.Refusal != RefusalScriptError {
		t.Fatalf("a syntax error gave %+v, want %s", resp, RefusalScriptError)
	}
}

// One document in, one document out. A second document means the caller thinks
// it is streaming to a child that is not, and running only the first script
// would hide that until something subtler broke.
func TestASecondRequestDocumentIsRefused(t *testing.T) {
	_, err := readRequest(strings.NewReader(`{"script":"1"}{"script":"2"}`))
	if err == nil {
		t.Fatal("two documents were accepted")
	}
	if !strings.Contains(err.Error(), "does not stream") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// Reading the request must not WAIT on an idle pipe. Decoder.More() refills
// from the reader, so it blocked for as long as a writer held stdin open —
// measured at 8 s — and that wait happens BEFORE the process is confined,
// which makes it a bound nobody set.
func TestReadingTheRequestDoesNotWaitOnAnIdleWriter(t *testing.T) {
	// A reader that yields one document and then blocks forever.
	blocking := io.MultiReader(strings.NewReader(`{"script":"1+1"}`), neverEOF{})

	done := make(chan error, 1)
	go func() {
		_, err := readRequest(blocking)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading one document answered an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readRequest is still waiting on a writer that holds the pipe open, before the child " +
			"is confined")
	}
}

// neverEOF blocks on Read forever without returning data or EOF.
type neverEOF struct{}

func (neverEOF) Read([]byte) (int, error) { <-make(chan struct{}); return 0, nil }

// decodeResponse takes the FIRST document and refuses one with no verdict.
// `null`, `{}` and `{"nope":1}` all decoded to an empty Response that OK()
// called success, which would hand the model an empty tool result labelled OK.
func TestAResponseWithoutAVerdictIsNotAResponse(t *testing.T) {
	for _, raw := range []string{``, `   `, `null`, `null\n`, `{}`, `  {}  `, `{"nope":1}`, `{"confinement":{"landlock_abi":4}}`} {
		if resp, ok := decodeResponse([]byte(raw)); ok {
			t.Errorf("%q decoded as a verdict: %+v", raw, resp)
		}
	}
	for raw, want := range map[string]string{
		`{"value":"42"}`:        "42",
		`{"value":"42"}` + "\n": "42",
		`{"value":"42"}x`:       "42",
	} {
		resp, ok := decodeResponse([]byte(raw))
		if !ok || resp.Value != want {
			t.Errorf("%q gave (%+v, %v), want the value %q", raw, resp, ok, want)
		}
	}
	if resp, ok := decodeResponse([]byte(`{"refusal":"script_error","message":"x"}`)); !ok || resp.OK() {
		t.Errorf("a refusal document gave (%+v, %v)", resp, ok)
	}
}

// clip bounds a blob in BYTES. firstLines alone did not: 4096 newline-free
// bytes are one line, and they travelled whole into a refusal message.
func TestTheEvidenceInARefusalIsBoundedInBytes(t *testing.T) {
	long := strings.Repeat("y", 10_000)
	if got := clip(long, 100); len(got) > 110 {
		t.Errorf("clip kept %d bytes of a 10000-byte single line", len(got))
	}
	if got := firstLines("a\nb\nc\nd\ne", 3); got != "a\nb\nc" {
		t.Errorf("firstLines = %q", got)
	}
}

// The posture verdict is ONE place, and both of its refusing branches are
// unreachable through the real syscalls on a healthy kernel and build — which
// is exactly why they are a function with a witness.
func TestTheConfinementVerdictRefusesBothWeakPostures(t *testing.T) {
	healthy := Applied{LandlockABI: 4, AllThreads: true, CPUSeconds: 10, MemoryBytes: 1 << 20}

	if got := confinementVerdict(healthy, Request{}); got != nil {
		t.Errorf("a healthy posture was refused: %+v", got)
	}

	noLandlock := healthy
	noLandlock.LandlockABI = 0
	got := confinementVerdict(noLandlock, Request{})
	if got == nil || got.Refusal != RefusalUnavailable {
		t.Fatalf("a kernel with no Landlock was accepted: %+v", got)
	}
	if !strings.Contains(got.Message, "allow_without_landlock") {
		t.Errorf("the refusal does not name its escape hatch: %q", got.Message)
	}
	if confinementVerdict(noLandlock, Request{AllowWithoutLandlock: true}) != nil {
		t.Error("the explicit allowance did not take")
	}

	oneThread := healthy
	oneThread.AllThreads = false
	got = confinementVerdict(oneThread, Request{})
	if got == nil || got.Refusal != RefusalUnavailable {
		t.Fatalf("a single-thread confinement was accepted: %+v", got)
	}
	if !strings.Contains(got.Message, "allow_single_thread_confinement") {
		t.Errorf("the refusal does not name its escape hatch: %q", got.Message)
	}
	if confinementVerdict(oneThread, Request{AllowSingleThreadConfinement: true}) != nil {
		t.Error("the explicit allowance did not take")
	}
}

// A seed that is not 32 hex-encoded bytes is an error, not a silently
// different run.
func TestAMalformedSeedIsRefused(t *testing.T) {
	if _, err := resolveSeed("not hex"); err == nil {
		t.Error("a non-hex seed was accepted")
	}
	if _, err := resolveSeed("abcd"); err == nil {
		t.Error("a short seed was accepted")
	}
	seed, err := resolveSeed(strings.Repeat("ff", 32))
	if err != nil {
		t.Fatal(err)
	}
	if seed[0] != 0xff || seed[31] != 0xff {
		t.Errorf("the seed did not round-trip: %x", seed)
	}
	// No seed at all is allowed and draws one.
	if _, err := resolveSeed(""); err != nil {
		t.Errorf("an absent seed answered %v", err)
	}
}

// THE order of deathToRefusal, which its own comment calls "the whole
// correctness of this function" and which nothing checked: swapping the memory
// and CPU branches left the entire suite green, while a probe showed the exact
// catastrophe the comment names — a memory death reported as
// `cpu_time_exceeded` with the parent still holding `runtime: out of memory`.
//
// The live arms cannot reach it: the memory bomb runs with a two-minute CPU
// bound, so the CPU branch can never fire there. It needs a death where BOTH
// conditions hold, which is why the evidence carries facts rather than a
// process.
func TestADeathIsReadInOrderOfEvidence(t *testing.T) {
	const memoryMarker = "fatal error: runtime: out of memory"
	limits, _ := Limits{CPU: time.Second, Wall: time.Minute, Memory: 128 << 20}.Clamp()

	// Burned past the CPU bound AND carrying the runtime's memory marker: the
	// positive identification must win over the threshold.
	both := deathEvidence{
		limits: limits, signalled: true, signal: 9,
		burned: 2 * time.Second, stderrText: memoryMarker,
	}
	resp, err := deathToRefusal(both)
	if err != nil {
		t.Fatalf("a death that names a bound became a host error: %v", err)
	}
	if resp.Refusal != RefusalMemory {
		t.Errorf("a memory death that had also burned its CPU budget gave %q: the CPU threshold "+
			"outranked the runtime's own words, and an operator reading that raises the CPU bound forever",
			resp.Refusal)
	}

	// The same, without the marker: now the CPU threshold is the best evidence.
	onlyCPU := both
	onlyCPU.stderrText = "killed"
	resp, err = deathToRefusal(onlyCPU)
	if err != nil {
		t.Fatalf("host error: %v", err)
	}
	if resp.Refusal != RefusalCPUTime {
		t.Errorf("a signalled death over the CPU bound gave %q, want %s", resp.Refusal, RefusalCPUTime)
	}
	// And it names BOTH numbers, because they differ on purpose.
	for _, want := range []string{"1s bound the operator set", "kernel kills at 2s"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("the CPU refusal does not say %q: %s", want, resp.Message)
		}
	}

	for _, tc := range []struct {
		name string
		ev   deathEvidence
		want string // a refusal code, or "" for a host error
	}{
		{"the caller's own deadline beats everything", deathEvidence{
			limits: limits, callerErr: context.DeadlineExceeded, scriptErr: context.DeadlineExceeded,
			signalled: true, burned: 2 * time.Second, stderrText: memoryMarker}, ""},
		{"the caller's cancellation beats everything", deathEvidence{
			limits: limits, callerErr: context.Canceled, signalled: true}, ""},
		{"the script's wall bound beats the memory marker", deathEvidence{
			limits: limits, scriptErr: context.DeadlineExceeded, stderrText: memoryMarker}, RefusalWallClock},
		{"a cut stdout, under the CPU bound", deathEvidence{
			limits: limits, signalled: true, burned: 10 * time.Millisecond, stdoutCut: true}, RefusalResultBytes},
		{"signalled, nothing names a bound", deathEvidence{
			limits: limits, signalled: true, signal: 9, burned: 10 * time.Millisecond}, RefusalHostDied},
		{"a runtime crash is the host's, not the deployment's", deathEvidence{
			limits: limits, stderrText: "SIGSEGV: segmentation violation"}, RefusalHostDied},
		{"nothing at all is a broken deployment", deathEvidence{
			limits: limits, waitErr: errors.New("exit status 127")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := deathToRefusal(tc.ev)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("want a host error, got %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("want %s, got the host error %v", tc.want, err)
			}
			if resp.Refusal != tc.want {
				t.Errorf("got %q, want %s (message: %s)", resp.Refusal, tc.want, resp.Message)
			}
		})
	}
}
