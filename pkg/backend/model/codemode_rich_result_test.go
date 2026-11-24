package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The CM-7/CM-8 wiring at the model seam: a nested call's outcome carries the
// structured payload a sink observed, the direct path arms no sink at all,
// and the script's own result budget reaches a nested tool from the CLAMPED
// limits the codemode tool installed.

// structuredReportTool answers like an MCP tool whose adapter flattens the
// render but observes the structured content into the sink the dispatch
// armed — what the registration closure in pkg/backend/mcp does for every
// server it registers. It reports whether it saw a sink, so one probe drives
// both paths.
func structuredReportTool(sawSink *bool) *GenerationTool {
	return &GenerationTool{Name: "mcp.report", Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
		sink, ok := tool.RichResultSinkFrom(ctx)
		*sawSink = ok
		if ok {
			sink.Observe(json.RawMessage(`{"matches":7}`))
		}
		// The text the model reads, unchanged by the sink: the flattening
		// contract is the one this payload travels beside.
		return `{"matches":7}`, nil
	}}
}

// A nested call's ToolOutcome carries the structured payload the tool
// observed, and its TEXT is byte-for-byte what the direct path produced. The
// model-facing render keeping its contract while the script gains a field is
// the whole claim of CM-7, and the comparison is against the real direct
// path, not against a copy of the text.
func TestANestedCallCarriesTheStructuredPayloadAndTheModelsText(t *testing.T) {
	t.Parallel()

	var directSawSink bool
	dd, dctx := newToolDispatcher(context.Background(),
		map[string]*GenerationTool{"mcp.report": structuredReportTool(&directSawSink)},
		nil, nil, nil, nil, nil)
	block, abort := dd.dispatch(dctx, toolUseBlock{Name: "mcp.report", PartialJSON: "{}"})
	if abort != nil {
		t.Fatalf("direct: unexpected abort: %v", abort)
	}
	directText := toolResultText(block)

	var nestedSawSink bool
	_, nctx := newToolDispatcher(context.Background(),
		map[string]*GenerationTool{"mcp.report": structuredReportTool(&nestedSawSink)},
		nil, nil, nil, nil, nil)
	srv, err := newScriptToolServer(nctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out, abort := srv.CallTool(programCall(1), "mcp.report", json.RawMessage("{}"))
	if abort != nil {
		t.Fatalf("nested: unexpected abort: %v", abort)
	}

	if directSawSink {
		t.Error("the DIRECT path saw a rich-result sink: arming must live on the nested path alone")
	}
	if !nestedSawSink {
		t.Error("the nested path saw no sink: the script tool server did not arm one")
	}
	if out.IsError {
		t.Fatalf("the nested call failed: %+v", out)
	}
	if string(out.Structured) != `{"matches":7}` {
		t.Errorf("the script's outcome carries structured %q, want the observed payload", out.Structured)
	}
	if out.Text != directText {
		t.Errorf("the script read %q where the model would have read %q: the text contract moved", out.Text, directText)
	}
}

// A refusal before dispatch — the withheld class — carries no structured
// payload: the tool never ran, so the sink never observed.
func TestARefusedCallCarriesNoStructuredPayload(t *testing.T) {
	t.Parallel()

	var ran bool
	tools := map[string]*GenerationTool{
		"ask_user": {Name: "ask_user", Execute: func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "unreachable", nil
		}},
	}
	_, ctx := newToolDispatcher(context.Background(), tools, nil, nil, nil, nil, nil)
	srv, err := newScriptToolServer(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	out, abort := srv.CallTool(programCall(1), "ask_user", nil)
	if abort != nil {
		t.Fatalf("unexpected abort: %v", abort)
	}
	if !out.IsError || ran {
		t.Fatalf("a withheld tool was served: %+v (ran=%t)", out, ran)
	}
	if out.Structured != nil {
		t.Errorf("a refused call carries structured %q, want none", out.Structured)
	}
}

// CM-8's arm at the seam: the budget a nested tool re-caps its own output to
// is the CLAMPED ToolResultBytes the codemode tool installed — an author's
// declared 512 KiB passes through, nobody's choice becomes the 64 KiB
// default — and the direct path arms none.
func TestTheScriptBudgetReachesANestedToolFromTheClampedLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		want   scripthost.Limits
		answer string
	}{
		{"an author's declared bound", scripthost.Limits{ToolResultBytes: 512 << 10, Memory: 1 << 30}, "524288:true"},
		{"nobody's choice", scripthost.Limits{}, "65536:true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			toolMap := map[string]*GenerationTool{"probe": {Name: "probe", Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
				budget, ok := scripthost.ScriptResultBudgetFrom(ctx)
				return fmt.Sprintf("%d:%t", budget, ok), nil
			}}}
			codetool := codemodeToolWithState(childHost(), tc.want, codemodeDescription, nil)
			in, err := json.Marshal(map[string]string{"source": `call('probe',{}).text`})
			if err != nil {
				t.Fatal(err)
			}
			_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)
			out, execErr := codetool.Execute(ctx, in)
			if execErr != nil {
				t.Fatalf("execute: %v", execErr)
			}
			if !strings.Contains(out, tc.answer) {
				t.Errorf("the nested tool saw %s, want the budget %s armed on its context", out, tc.answer)
			}
		})
	}
}

// Two calls of one batch read their OWN sink. A sink shared across the batch
// would let one worker's observation overwrite another's, and which entry
// came out wrong would depend on which worker finished last — the verdict
// reconstructed from order this repository has paid for. The choreography
// pins the order instead: right observes first, left observes last, so a
// shared sink deterministically hands right's entry left's payload.
func TestParallelNestedCallsEachReadTheirOwnSink(t *testing.T) {
	t.Parallel()

	leftEntered := make(chan struct{})
	rightObserved := make(chan struct{})
	tools := map[string]*GenerationTool{
		"left": {Name: "left", concurrentSafe: true, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(leftEntered)
			<-rightObserved
			if sink, ok := tool.RichResultSinkFrom(ctx); ok {
				sink.Observe([]byte(`{"which":"A"}`))
			}
			return "A", nil
		}},
		"right": {Name: "right", concurrentSafe: true, Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
			<-leftEntered
			if sink, ok := tool.RichResultSinkFrom(ctx); ok {
				sink.Observe([]byte(`{"which":"B"}`))
			}
			close(rightObserved)
			return "B", nil
		}},
	}
	codetool := codemodeToolWithState(childHost(), scripthost.Limits{Memory: 1 << 30, Wall: 10 * time.Second, ConcurrentToolCalls: 2}, codemodeDescription, nil)
	in, err := json.Marshal(map[string]string{"source": `call.parallel([{name:'left',input:{}},{name:'right',input:{}}]).map(r => r.structured.which)`})
	if err != nil {
		t.Fatal(err)
	}
	_, ctx := newToolDispatcher(context.Background(), tools, nil, nil, nil, nil, nil)
	out, execErr := codetool.Execute(ctx, in)
	if execErr != nil {
		t.Fatalf("execute: %v", execErr)
	}
	if out != `["A","B"]` {
		t.Errorf("the batch returned %s, want each entry carrying its own observed payload", out)
	}
}

// The frame cap must cover BOTH payloads a result may carry, each at the
// wire worst case: text that JSON-escapes at six bytes per byte, and the
// structured payload whose <, > and & bytes the frame encoder re-escapes the
// same way. A frame holding a full bound of each escapes to about 12x
// tool_result_bytes plus its envelope, so a cap computed at anything less
// refuses a frame the parent was told to send — and the refusal is not one
// the model can act on, it is a broken deployment. This witness drives the
// REAL child over both escaping shapes.
func TestTheFrameCapCoversTextAndStructuredTogether(t *testing.T) {
	t.Parallel()

	const bound = 256 << 10
	for _, tc := range []struct {
		name       string
		structured string
	}{
		// A payload whose strings escape 1:1 on the wire.
		{"plain strings", `{"pad":"` + strings.Repeat("x", int(bound)-10) + `"}`},
		// A payload full of bytes the frame encoder re-escapes 6:1 — the
		// worst case the cap's factor exists for. An XML or HTML report is
		// the normal shape of a structured result.
		{"re-escaped bytes", `{"pad":"` + strings.Repeat("<", int(bound)-10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			text := strings.Repeat("<", bound)
			toolMap := map[string]*GenerationTool{"big": {Name: "big", Execute: func(ctx context.Context, _ json.RawMessage) (string, error) {
				if sink, ok := tool.RichResultSinkFrom(ctx); ok {
					sink.Observe([]byte(tc.structured))
				}
				return text, nil
			}}}
			codetool := codemodeToolWithState(childHost(), scripthost.Limits{ToolResultBytes: bound, Memory: 1 << 30}, codemodeDescription, nil)
			in, err := json.Marshal(map[string]string{"source": `var r = call('big',{}); [r.text.length, r.structured.pad.length, r.structuredTruncated]`})
			if err != nil {
				t.Fatal(err)
			}
			_, ctx := newToolDispatcher(context.Background(), toolMap, nil, nil, nil, nil, nil)
			out, execErr := codetool.Execute(ctx, in)
			if execErr != nil {
				t.Fatalf("a frame holding a full bound of text and a full bound of structured broke the channel: %v", execErr)
			}
			want := fmt.Sprintf("[%d,%d,false]", bound, bound-10)
			if !strings.Contains(out, want) {
				t.Errorf("the program saw %s, want both payloads whole (%s)", out, want)
			}
		})
	}
}
