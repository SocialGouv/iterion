package scripthost

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// The ticket's witness (#3316): a program with NO try/catch whose channel
// breaks under a call must come back script_host_died, and the same program
// WITH a try/catch must too.
//
// Measured before the fix, by fault injection: the same broken channel came
// back script_error without a catch and script_host_died with one — and
// script_error is the class errToolChannelBroken's doc forbids, because a
// script told "your call failed" would retry a broken deployment in a loop.
// Why the classes differed: with a catch, the bridge's interrupt wins the
// race and the InterruptedError branch classifies; without one, the bridge's
// panic surfaces as a *goja.Exception and fell to the generic refusal.
//
// This test reproduces the bridge's EXACT panic shape — `panic(vm.NewGoError(
// fmt.Errorf("%w: …", errToolChannelBroken)))`, the same construction
// toolcall_child.go's binding performs — bound as `call`, because the channel
// itself cannot be broken through the public surface any more: since the
// frame caps became coherent, both directions refuse by construction.
func TestABrokenChannelIsHostDiedWhateverTheCatch(t *testing.T) {
	t.Parallel()

	bindAndRun := func(src string) Response {
		vm := newRuntime([32]byte{9}, epoch)
		vm.Set("call", func(call goja.FunctionCall) goja.Value {
			// The bridge's own double gesture, verbatim in shape
			// (toolcall_child.go): post the interrupt, THEN panic. The
			// interrupt cannot be caught, so with a catch it wins the race
			// and classifies; without one, the panic surfaces as an
			// exception — which is the row that classified wrong.
			err := fmt.Errorf("%w: reading the result of call 1: the frame was refused",
				errToolChannelBroken)
			vm.Interrupt(err)
			panic(vm.NewGoError(err))
		})
		lim := DefaultLimits()
		return runScript(vm, src, lim.Wall, lim.ResultBytes)
	}

	for _, row := range []struct {
		name string
		src  string
	}{
		{"no catch (the shape a model writes)", `call('x', {})`},
		{"try/catch returning its own answer", `try { call('x', {}) } catch (e) { 'swallowed' }`},
		{"try/finally", `try { call('x', {}) } finally { }`},
	} {
		t.Run(row.name, func(t *testing.T) {
			resp := bindAndRun(row.src)
			if resp.Refusal != RefusalHostDied {
				t.Fatalf("refusal = %q (message %q), want %q — the verdict must not depend on whether the program happens to have a catch", resp.Refusal, resp.Message, RefusalHostDied)
			}
			if !strings.Contains(resp.Message, "tool channel failed") {
				t.Errorf("the message does not name the channel: %q", resp.Message)
			}
		})
	}

	// HOSTILE THROWS, from the gate review of this very fix: Export() on a
	// script-thrown object runs its getters, so the guarded read above is
	// what keeps a hostile throw from (a) panicking the error path with the
	// message degraded, or (b) looping past the disarmed wall bound for the
	// parent to misreport as wall_clock_exceeded. Both rows must come back
	// script_error WITHIN the wall bound, with a rendered message — and
	// without the guard both were measured failing (the hang row hangs the
	// whole binary).
	// NOTE: these throws are the program ITSELF, not routed through `call` —
	// a channel error's interrupt preempts the catch block, so routing them
	// through call never reaches the hostile throw. The gate review probed
	// them as top-level throws, which is the path the unwrap sees.
	bindAndRunHostile := func(src string) Response {
		vm := newRuntime([32]byte{11}, epoch)
		lim := DefaultLimits()
		return runScript(vm, src, lim.Wall, lim.ResultBytes)
	}
	for _, row := range []struct{ name, src string }{
		{"throwing getter", `throw { get x(){ throw 1 } }`},
		{"looping getter", `throw { get x(){ for(;;){} } }`},
	} {
		t.Run(row.name, func(t *testing.T) {
			start := time.Now()
			resp := bindAndRunHostile(row.src)
			// The SPEED is the property that distinguishes Unwrap from the
			// Export walk: with Export, the looping getter runs until the
			// re-armed wall interrupt (30s for a 30s wall); with Unwrap, the
			// thrown object's properties are never touched and the row
			// returns instantly (measured 0.00s). 10s is a third of the wall.
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("%s took %s — the thrown object's properties were walked (a getter ran to the wall)", row.name, d)
			}
			if resp.Refusal != RefusalScriptError {
				t.Errorf("refusal = %q (message %.120q), want %q — a script throw must stay the script's own verdict", resp.Refusal, resp.Message, RefusalScriptError)
			}
			if strings.Contains(resp.Message, "could not be evaluated") {
				t.Errorf("the thrown value's message degraded: %.200q", resp.Message)
			}
		})
	}

	// The other arm: a script's OWN throw is still script_error. The
	// unwrapping must key on errToolChannelBroken and nothing else, or every
	// ordinary script exception would be reported as a broken deployment.
	vm := newRuntime([32]byte{10}, epoch)
	vm.Set("call", func(call goja.FunctionCall) goja.Value {
		panic(vm.NewGoError(fmt.Errorf("some ordinary tool failure, not a channel defect")))
	})
	resp := runScript(vm, `try { call('x', {}) } catch (e) { throw new Error('rethrown: ' + e.message) }`, 10*time.Second, 1<<20)
	if resp.Refusal != RefusalScriptError {
		t.Errorf("an ordinary tool failure thrown and rethrown by the script = %q, want %q", resp.Refusal, RefusalScriptError)
	}
}
