package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// A tool result inside its own declared bound must reach the program.
//
// `scripthost.Limits.ScriptBytes` is read at FOUR sites, and two of them are
// the tool-channel FRAME cap (toolcall.go, toolcall_child.go) rather than the
// source-size bound its own doc describes ("ScriptBytes caps the source").
// A tool RESULT frame carries text the parent already clipped to
// ToolResultBytes — so the two numbers are coupled, and nothing says so.
//
// Until the bounds were declarable the pair was always DefaultLimits
// (64 KiB result inside a 256 KiB frame) and the coupling could not bite.
// Ceiling() permits tool_result_bytes up to 1 MiB while the DEFAULT
// script_bytes is 256 KiB, so the platform now declares legal a pair that
// cannot work — and the failure is not a refusal the model can act on: the
// frame error is classified as a DEPLOYMENT failure and tells the model no
// other program will run either.
//
// The frame the child refuses here was written by its own PARENT, measured
// against a number describing source size.
func TestAToolResultInsideItsOwnBoundReachesTheProgram(t *testing.T) {
	t.Parallel()
	// 300 KiB of result: well inside a declared 512 KiB tool_result_bytes,
	// well past the 256 KiB default script_bytes.
	// `<` escapes to six bytes under json.Encoder's HTML escaping, which is
	// the WHOLE reason resultFrameCap carries a factor. Measured: with
	// `strings.Repeat("y", …)` — which escapes 1:1 — a mutant setting the
	// factor to 1 survived both suites, so the constant the fix exists for
	// was unexercised. A tool returning XML, HTML or JSON is the normal case.
	payload := strings.Repeat("<", 300<<10)
	toolMap := map[string]*GenerationTool{
		"big": {Name: "big", Execute: func(context.Context, json.RawMessage) (string, error) {
			return payload, nil
		}},
	}
	in, err := json.Marshal(map[string]string{"source": `call('big',{}).text.length`})
	if err != nil {
		t.Fatal(err)
	}
	_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)

	out, err := codemodeTool(childHost(), scripthost.Limits{ToolResultBytes: 512 << 10}).Execute(ctx, in)
	if err != nil {
		t.Fatalf("a 300 KiB result under a declared 512 KiB tool_result_bytes broke the channel: %v", err)
	}
	if !strings.Contains(out, "307200") {
		t.Errorf("result = %q, want the program to have seen all 307200 bytes", out)
	}
}

// The other arm: a result ABOVE its declared bound is clipped and flagged,
// not a channel failure. Without this arm a frame cap raised to silence the
// test above would look like a fix.
func TestAToolResultAboveItsBoundIsClippedNotFatal(t *testing.T) {
	t.Parallel()
	// The same escaping shape here, and it is not cosmetic: this is the arm
	// where the clip lands EXACTLY on the declared bound, so it is the only
	// one that pins the factor to 6 rather than to "at least 4". Measured:
	// with a payload that escapes 1:1, a factor of 5 — a wrong value that
	// breaks a reachable configuration — survived both suites.
	payload := strings.Repeat("<", 200<<10)
	toolMap := map[string]*GenerationTool{
		"big": {Name: "big", Execute: func(context.Context, json.RawMessage) (string, error) {
			return payload, nil
		}},
	}
	in, err := json.Marshal(map[string]string{"source": `var r = call('big',{}); [r.text.length, r.truncated]`})
	if err != nil {
		t.Fatal(err)
	}
	_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)

	out, err := codemodeTool(childHost(), scripthost.Limits{ToolResultBytes: 4096}).Execute(ctx, in)
	if err != nil {
		t.Fatalf("a clipped result must come back, not break the channel: %v", err)
	}
	if !strings.Contains(out, "4096") || !strings.Contains(out, "true") {
		t.Errorf("result = %q, want the clipped length and truncated=true", out)
	}
}
