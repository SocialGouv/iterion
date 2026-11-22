package scripthost

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// structuredTestServer answers every call with a fixed outcome.
type structuredTestServer struct {
	mu      sync.Mutex
	outcome ToolOutcome
	err     error
}

func (s *structuredTestServer) CallTool(_ context.Context, _ string, _ json.RawMessage) (ToolOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome, s.err
}

// THE CM-7 witness: a tool's structured payload reaches the program as a
// value it composes, beside the text the model would have read — not as a
// string a program would have to parse.
func TestAStructuredResultReachesTheProgramAsAValue(t *testing.T) {
	t.Parallel()

	srv := &structuredTestServer{outcome: ToolOutcome{
		Text:       "2 files",
		Structured: json.RawMessage(`{"count":2,"files":["a.md","b.md"]}`),
	}}

	resp, err := toolHost(t, srv).Run(context.Background(), `
		const r = call("list", {});
		[r.text, r.structured.count, r.structured.files[1], r.structuredTruncated, r.structuredBytes === undefined]
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the script was refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != `["2 files",2,"b.md",false,true]` {
		t.Errorf("the program saw %s, want the structured object composed beside the text, unflagged, no size", resp.Value)
	}
}

// A payload past the bound is dropped ALL-OR-NOTHING and the drop is
// explicit: undefined payload, the truncated flag, and the size the program
// was shorted. Half a JSON document would not be a value a program can
// parse, so there is no cut payload to hand over.
func TestAStructuredResultAboveTheBoundIsDroppedAndSaysSo(t *testing.T) {
	t.Parallel()

	payload := `{"pad":"` + strings.Repeat("x", 200) + `"}`
	srv := &structuredTestServer{outcome: ToolOutcome{
		Text:       "small text",
		Structured: json.RawMessage(payload),
	}}
	limits := smallLimits()
	limits.ToolResultBytes = 50

	resp, err := toolHost(t, srv).Run(context.Background(), `
		const r = call("big", {});
		[r.structured === undefined, r.structuredTruncated, r.structuredBytes, r.text]
	`, limits)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the script was refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != `[true,true,210,"small text"]` {
		t.Errorf("the program saw %s, want undefined payload, flagged, the dropped size 210, text intact", resp.Value)
	}
}

// The batch path builds the same result objects: an ok entry carries its
// structured payload, an entry whose tool sent none carries the flag unset —
// and every entry reports input order, unchanged by the field.
func TestParallelBatchEntriesCarryStructuredResults(t *testing.T) {
	t.Parallel()

	srv := parallelTestServer{call: func(_ context.Context, name string, _ json.RawMessage) (ToolOutcome, error) {
		if name == "structured" {
			return ToolOutcome{Text: "with", Structured: json.RawMessage(`{"count":7}`)}, nil
		}
		return ToolOutcome{Text: "without"}, nil
	}}

	resp, err := toolHost(t, srv).Run(context.Background(), `
		call.parallel([{name:'structured'}, {name:'plain'}]).map(r =>
			[r.status, r.structured === undefined ? "none" : r.structured.count, r.structuredTruncated])
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the script was refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != `[["ok",7,false],["ok","none",false]]` {
		t.Errorf("the batch returned %s, want both ok entries with the structured payload only where one was sent", resp.Value)
	}
}

// A failed call throws, and the thrown value carries NO structured fields:
// the parent never sends a payload on an error result, so the catch must not
// be handed metadata about a payload that was never sent.
func TestAFailedToolCarriesNoStructuredFields(t *testing.T) {
	t.Parallel()

	srv := &structuredTestServer{outcome: ToolOutcome{
		Text:       "tool exploded",
		IsError:    true,
		Structured: json.RawMessage(`{"never":"sent"}`),
	}}

	resp, err := toolHost(t, srv).Run(context.Background(), `
		try { call("bad", {}); "NOT REACHED" }
		catch (e) { [e.structured === undefined, e.structuredTruncated === undefined, e.structuredBytes === undefined, e.text] }
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the script was refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != `[true,true,true,"tool exploded"]` {
		t.Errorf("the catch saw %s, want no structured fields on the thrown value and the text present", resp.Value)
	}
}

// The wire shape the parent sends, driven over in-memory pipes without a
// child: a success frame carries the structured payload, an error frame
// carries none — even when the server delivered one — and an over-budget
// refusal carries none. A program-visible field absent from the wire cannot
// leak by child-side accident either.
func TestTheParentSendsStructuredOnlyOnSuccessfulResults(t *testing.T) {
	t.Parallel()

	structured := json.RawMessage(`{"ok":true}`)
	for _, tc := range []struct {
		name        string
		outcome     ToolOutcome
		wantPresent bool
	}{
		{"a success", ToolOutcome{Text: "done", Structured: structured}, true},
		{"a failure", ToolOutcome{Text: "broken", IsError: true, Structured: structured}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &structuredTestServer{outcome: tc.outcome}
			var out strings.Builder
			if _, err := serveToolCalls(context.Background(),
				strings.NewReader(`{"type":"call","id":1,"name":"probe","input":{}}`+"\n"),
				&out, srv, DefaultLimits()); err != nil {
				t.Fatalf("serve: %v", err)
			}
			var frame Frame
			if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &frame); err != nil {
				t.Fatalf("decoding the reply: %v", err)
			}
			if tc.wantPresent {
				if string(frame.Structured) != string(structured) {
					t.Errorf("the reply carries structured %q, want the payload verbatim", frame.Structured)
				}
				if frame.StructuredTruncated {
					t.Error("a payload inside the bound must not be flagged truncated")
				}
				return
			}
			if frame.Structured != nil {
				t.Errorf("an error result carries structured %q, want none", frame.Structured)
			}
			if frame.StructuredTruncated || frame.StructuredBytes != 0 {
				t.Errorf("an error result carries truncation metadata (%v, %d), want none", frame.StructuredTruncated, frame.StructuredBytes)
			}
		})
	}
}

// An over-budget refusal is a frame of its own with no payload at all.
func TestABudgetRefusalCarriesNoStructuredPayload(t *testing.T) {
	t.Parallel()

	srv := &structuredTestServer{}
	var out strings.Builder
	served, err := serveToolCalls(context.Background(),
		strings.NewReader(`{"type":"call","id":1,"name":"probe","input":{}}`+"\n"),
		&out, srv, Limits{ToolCalls: 0, ToolResultBytes: 1 << 10})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if served != 0 {
		t.Fatalf("served %d calls under a zero budget", served)
	}
	var frame Frame
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &frame); err != nil {
		t.Fatalf("decoding the budget frame: %v", err)
	}
	if frame.Type != FrameBudget || frame.Structured != nil || frame.StructuredTruncated || frame.StructuredBytes != 0 {
		t.Errorf("the budget frame = %+v, want a bare budget refusal with no structured fields", frame)
	}
}

// The frame cap covers BOTH payloads the parent may write, each at its wire
// worst case: the text it clipped, and the structured payload it clipped —
// whose <, > and & bytes the frame encoder re-escapes exactly like the
// text's. A cap that spent less than six on either would refuse a frame the
// parent was told to send.
func TestResultFrameCapCoversTextAndStructured(t *testing.T) {
	t.Parallel()

	l := Limits{ToolResultBytes: 1000}
	if got, want := resultFrameCap(l), l.ToolResultBytes*12+1024; got != want {
		t.Errorf("resultFrameCap(1000) = %d, want %d (6x text escaping + 6x structured escaping + envelope)", got, want)
	}
}
