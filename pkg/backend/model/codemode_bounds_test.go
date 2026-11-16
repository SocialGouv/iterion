package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The declared bounds, from installCodemode to the thing that enforces them.
//
// Both arms for each, deliberately: refused above the bound AND allowed
// below. The bounds doctrine asks for it in those words, because a false
// refusal in a blocking path costs as much as a hole and is harder to see —
// a node whose programs are all refused looks like a node whose model cannot
// write JavaScript.

// runBounded is runCodemode with a chosen bound set instead of the zero value.
//
// MEMORY is filled here rather than at each call site, because the reason is
// the HARNESS's and not any one test's: this child is the test binary, and on
// the `-race` leg it is race-instrumented, which does not fit in
// DefaultLimits' 256 MiB — the same reason codemode_tool_test.go states for
// its own `Memory: 1 << 30`. Measured by not doing it: a child died mid-write
// and the parent reported `write |1: broken pipe`, which reads as a channel
// defect rather than as a child that ran out of address space. 1 GiB is under
// scripthost.Ceiling, so Clamp leaves it alone and the bound under test is
// still the one the caller chose.
func runBounded(t *testing.T, want scripthost.Limits, toolMap map[string]*GenerationTool, source string) (string, error) {
	t.Helper()
	if want.Memory == 0 {
		want.Memory = 1 << 30
	}
	_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)
	in, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		t.Fatal(err)
	}
	return codemodeTool(childHost(), want).Execute(ctx, in)
}

func TestADeclaredToolCallBoundRefusesTheCallPastIt(t *testing.T) {
	t.Parallel()
	calls := 0
	toolMap := map[string]*GenerationTool{
		"ping": {Name: "ping", Execute: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "pong", nil
		}},
	}
	// Three calls under a bound of two. The budget is the PARENT's, so this
	// also witnesses that the number crossed the pipe rather than being the
	// child's own idea.
	src := `call('ping',{}); call('ping',{}); call('ping',{}); 'done'`
	_, err := runBounded(t, scripthost.Limits{ToolCalls: 2}, toolMap, src)
	if err == nil {
		t.Fatalf("three calls ran under a declared bound of two: the bound did not reach the host")
	}
	if !strings.Contains(err.Error(), "budget") && !strings.Contains(err.Error(), "tool call") {
		t.Errorf("the refusal does not name the budget, so a model cannot act on it: %v", err)
	}
	// The dispatcher saw two and refused the third BEFORE it reached the
	// door: an over-budget call fires no hook and computes no verdict.
	if calls != 2 {
		t.Errorf("the node's door was reached %d time(s), want 2 — the third must not arrive", calls)
	}
}

func TestADeclaredToolCallBoundAllowsTheCallsUnderIt(t *testing.T) {
	t.Parallel()
	calls := 0
	toolMap := map[string]*GenerationTool{
		"ping": {Name: "ping", Execute: func(context.Context, json.RawMessage) (string, error) {
			calls++
			return "pong", nil
		}},
	}
	src := `call('ping',{}); call('ping',{}); call('ping',{}); 'done'`
	out, err := runBounded(t, scripthost.Limits{ToolCalls: 8}, toolMap, src)
	if err != nil {
		t.Fatalf("three calls refused under a declared bound of eight — a false refusal: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("result = %q, want the program's last expression", out)
	}
	if calls != 3 {
		t.Errorf("the node's door was reached %d time(s), want 3", calls)
	}
}

// TestOnClampReportsAFloorRaise pins the report half of #3290's floor: a
// bound the child cannot work under is RAISED by Clamp and the raise travels
// OnClamp — the operator's warning — exactly as a ceiling reduction does.
// Without this row, a floor raise was silent: the operator wrote 8 MiB and
// the child ran with 64 MiB, and nothing in the run said why.
func TestOnClampReportsAFloorRaise(t *testing.T) {
	t.Parallel()
	var onClampReq, onClampApplied *scripthost.Limits
	h := childHost()
	h.OnClamp = func(requested, applied scripthost.Limits) {
		onClampReq, onClampApplied = &requested, &applied
	}
	// The run's OUTCOME is irrelevant to this witness — in CI there is no
	// installed iterion for a production-path child to resolve (the first
	// CI attempt failed exactly there: "exec: no command"), so the assertion
	// is the CAPTURE: Clamp fires OnClamp BEFORE any spawn attempt
	// (host.go: clamped → OnClamp → exec), and the report says the 8 MiB
	// the witness wrote became the floor.
	_, _ = h.Run(context.Background(), `1+1`, scripthost.Limits{Memory: 8 << 20, Wall: 20 * time.Second, CPU: 15 * time.Second})
	if onClampReq == nil || onClampApplied == nil {
		t.Fatalf("OnClamp never fired on a floor raise: the operator wrote 8 MiB and nothing said the child ran with more")
	}
	floor := scripthost.MemoryFloor().Memory
	if onClampReq.Memory != 8<<20 || onClampApplied.Memory != floor {
		t.Errorf("OnClamp (requested %+v, applied %+v), want 8 MiB raised to the floor %d", *onClampReq, *onClampApplied, floor)
	}
}

// TestInstallCodemodePassesTheDeclaredBounds is the ONE line this slice adds
// to the runtime: installCodemode used to hand the host a zero Limits, so a
// declared bound could reach the task and stop there.
//
// It measures with ScriptBytes because that bound is enforced PARENT-side,
// before the spawn — so the witness needs no child process, no cgo
// allowance, and no filesystem: it fails if, and only if, the argument is not
// passed through.
func TestInstallCodemodePassesTheDeclaredBounds(t *testing.T) {
	t.Parallel()
	source := strings.Repeat("x", 200) // a program larger than the bound below
	in, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		t.Fatal(err)
	}

	var opts GenerationOptions
	installCodemode(&opts, &scripthost.Limits{ScriptBytes: 10}, nil)
	if len(opts.Tools) != 1 {
		t.Fatalf("installCodemode appended %d tool(s), want 1", len(opts.Tools))
	}
	_, ctx := newToolDispatcher(context.Background(), map[string]*GenerationTool{}, nil, nil, nil, nil, nil)
	_, err = opts.Tools[0].Execute(ctx, in)
	if err == nil {
		t.Fatalf("a 200-byte program ran under a declared script_bytes of 10: the bound stopped at the task")
	}
	// The REFUSAL KIND, not merely that something failed. Measured: with the
	// bound dropped, this program still errors — the host spawns a child
	// which cannot parse 200 bytes of `x`, so `err != nil` holds either way
	// and an assertion on its mere presence passes under the defect it was
	// written for. That is how the first version of this test survived its
	// own mutant.
	if !strings.Contains(err.Error(), scripthost.RefusalScriptBytes) {
		t.Fatalf("refused, but not for its size (%s): %v", scripthost.RefusalScriptBytes, err)
	}

	// The other arm: nil bounds must NOT invent one. The same program under
	// the defaults (256 KiB) passes the parent-side size check and goes on to
	// the spawn, so what this arm asserts is that it is not refused FOR ITS
	// SIZE — the error, if any, comes from somewhere else.
	var plain GenerationOptions
	installCodemode(&plain, nil, nil)
	_, err = plain.Tools[0].Execute(ctx, in)
	if err != nil && strings.Contains(err.Error(), scripthost.RefusalScriptBytes) {
		t.Errorf("nil bounds refused a 200-byte program for its size: %v", err)
	}
}

// The witness #3311 asks for, in the ticket's own words: "a program whose
// source is `await call('x', {})` must come back as the script_error refusal,
// so the day someone wires a job queue the test says the description is now
// wrong".
//
// It was owed with the description clause and delivered without it, which the
// gate reviewer caught — a sentence a fix writes is an assertion to execute,
// and a sentence written in a TICKET is one too.
//
// `await` outside an async function is a SyntaxError in ES, and a goja script
// is not a module, so there is no top-level await either. That is why the
// clause exists: every JavaScript tool API a model has seen is asynchronous,
// and the description's own list of absences ("no console, no require, no
// process, no timers, no network") taught it that anything unforbidden was
// present.
func TestAProgramThatAwaitsACallIsRefusedAsAScriptError(t *testing.T) {
	t.Parallel()
	toolMap := map[string]*GenerationTool{
		"x": {Name: "x", Execute: func(context.Context, json.RawMessage) (string, error) {
			t.Error("the tool ran: a program that does not parse must not reach the node's door")
			return "", nil
		}},
	}
	_, err := runBounded(t, scripthost.Limits{}, toolMap, `await call('x', {})`)
	if err == nil {
		t.Fatalf("`await call(...)` was accepted: either this host grew an event loop, in which case the tool description is now wrong, or the parse stopped being strict")
	}
	if !strings.Contains(err.Error(), scripthost.RefusalScriptError) {
		t.Errorf("refused as %q, want the %s refusal — the model has to be able to tell its own mistake from a broken deployment", err, scripthost.RefusalScriptError)
	}
	// The description's promise, executed: the clause that tells the model
	// not to write this must be in the text the model reads.
	for _, want := range []string{"SYNCHRONOUS", "await"} {
		if !strings.Contains(codemodeDescription, want) {
			t.Errorf("the tool description does not mention %q, so nothing warns the model off the one form every JS API it has seen uses", want)
		}
	}
}
