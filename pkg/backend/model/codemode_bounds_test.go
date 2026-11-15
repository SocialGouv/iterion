package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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
func runBounded(t *testing.T, want scripthost.Limits, toolMap map[string]*GenerationTool, source string) (string, error) {
	t.Helper()
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
