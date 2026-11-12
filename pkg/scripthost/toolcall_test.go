package scripthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingServer is the test's stand-in for the node's dispatcher.
//
// It takes a mutex on EVERY field, including the counter: it is fed by a
// goroutine (the pump) and read by the test's, and this repo's `-race` leg has
// reddened twice on an observer that talked about concurrency without taking a
// lock. The answer func is held under the same lock for the same reason.
type recordingServer struct {
	mu     sync.Mutex
	calls  []recordedCall
	answer func(name string, input json.RawMessage) (ToolOutcome, error)
}

type recordedCall struct {
	Name  string
	Input string
}

func (s *recordingServer) CallTool(_ context.Context, name string, input json.RawMessage) (ToolOutcome, error) {
	s.mu.Lock()
	s.calls = append(s.calls, recordedCall{Name: name, Input: string(input)})
	answer := s.answer
	s.mu.Unlock()
	if answer == nil {
		return ToolOutcome{Text: "ok"}, nil
	}
	return answer(name, input)
}

func (s *recordingServer) seen() []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedCall(nil), s.calls...)
}

// toolHost is a Host wired to a server, against the REAL shipping binary.
// Nothing here uses a fake child: the whole point of the slice is that the
// call crosses a process boundary into a confined child, and a shell stand-in
// would prove the framing and nothing else.
func toolHost(t *testing.T, srv ToolServer) Host {
	t.Helper()
	return Host{Binary: realBinary(t), Tools: srv}
}

func smallLimits() Limits {
	return Limits{Wall: 20 * time.Second, CPU: 10 * time.Second, Memory: 256 << 20}
}

// THE witness of the slice: a script reaches the node's tools, out of the
// model's context, and gets their results as values it can compose.
func TestAScriptCallsTheNodesToolsAndComposesTheResults(t *testing.T) {
	t.Parallel()

	srv := &recordingServer{answer: func(name string, input json.RawMessage) (ToolOutcome, error) {
		var in struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return ToolOutcome{Text: "bad input: " + err.Error(), IsError: true}, nil
		}
		return ToolOutcome{Text: fmt.Sprintf("%d", in.N*in.N)}, nil
	}}

	resp, err := toolHost(t, srv).Run(context.Background(), `
		let total = 0;
		for (let i = 1; i <= 5; i++) {
			total += Number(call("square", { n: i }).text);
		}
		total
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("the script was refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != "55" {
		t.Errorf("the script returned %q, want \"55\" (1+4+9+16+25)", resp.Value)
	}

	seen := srv.seen()
	if len(seen) != 5 {
		t.Fatalf("the server saw %d calls, want 5: %+v", len(seen), seen)
	}
	// The INPUT the tool received is what the script built, encoded by the
	// engine's own JSON.stringify — not a Go reflection of it.
	if seen[2].Name != "square" || seen[2].Input != `{"n":3}` {
		t.Errorf("the third call arrived as %+v, want square with {\"n\":3}", seen[2])
	}
	// Five results crossed the pipe and ONE value came back: that is the whole
	// claim of codemode, and the only number the model would have read is 55.
	if len(resp.Value) > 4 {
		t.Errorf("the value is %q: the script was supposed to distil, not relay", resp.Value)
	}
}

// A failed tool THROWS, so a script that ignores the outcome stops instead of
// aggregating an error message as data — and the throw is catchable, carrying
// the text, so a script can recover.
func TestAFailedToolThrowsAndCarriesItsText(t *testing.T) {
	t.Parallel()

	srv := &recordingServer{answer: func(string, json.RawMessage) (ToolOutcome, error) {
		return ToolOutcome{Text: "no such file: /etc/nope", IsError: true}, nil
	}}
	h := toolHost(t, srv)

	resp, err := h.Run(context.Background(), `
		try { call("read_file", { path: "/etc/nope" }); "NOT REACHED" }
		catch (e) { "caught: " + e.text + " / tool=" + e.tool }
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("refused: %s — %s", resp.Refusal, resp.Message)
	}
	for _, want := range []string{"caught:", "no such file: /etc/nope", "tool=read_file"} {
		if !strings.Contains(resp.Value, want) {
			t.Errorf("the caught value %q does not carry %q", resp.Value, want)
		}
	}

	// And a script that does NOT catch it stops, rather than returning a
	// number computed from an error message.
	resp, err = h.Run(context.Background(), `
		let total = 0;
		total += Number(call("read_file", {}).text);
		total
	`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != RefusalScriptError {
		t.Errorf("an uncaught tool failure gave %q (%s), want %s: a script that ignores a failure must stop",
			resp.Refusal, resp.Message, RefusalScriptError)
	}
}

// The call budget is the parent's, and an over-budget call costs NOTHING: the
// server is never asked, so no hook fires and no permission verdict is
// computed for a call that was already refused.
func TestTheCallBudgetIsEnforcedByTheParentAndCostsNothingOnceSpent(t *testing.T) {
	t.Parallel()

	srv := &recordingServer{}
	limits := smallLimits()
	limits.ToolCalls = 3

	resp, err := toolHost(t, srv).Run(context.Background(), `
		let ok = 0, refused = 0, lastRemaining = -1;
		for (let i = 0; i < 10; i++) {
			try { const r = call("ping", {}); ok++; lastRemaining = r.remaining; }
			catch (e) { refused++; }
		}
		ok + ":" + refused + ":" + lastRemaining
	`, limits)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("refused: %s — %s", resp.Refusal, resp.Message)
	}
	if resp.Value != `"3:7:0"` {
		t.Errorf("the script reports %s, want \"3:7:0\" — three served, seven refused, nothing left", resp.Value)
	}
	if n := len(srv.seen()); n != 3 {
		t.Errorf("the server was asked %d times under a budget of 3: an over-budget call must not reach it", n)
	}
}

// A tool result past the bound is cut on a rune boundary AND says it was cut:
// a script that aggregates a silently truncated result and returns a confident
// total is worse than one that knows.
func TestAnOversizedToolResultIsCutOnARuneBoundaryAndSaysSo(t *testing.T) {
	t.Parallel()

	// Multi-byte runes so a byte cut would split one.
	srv := &recordingServer{answer: func(string, json.RawMessage) (ToolOutcome, error) {
		return ToolOutcome{Text: strings.Repeat("é", 100)}, nil
	}}
	limits := smallLimits()
	limits.ToolResultBytes = 11 // mid-rune: 5 runes are 10 bytes, the 6th straddles

	resp, err := toolHost(t, srv).Run(context.Background(), `
		const r = call("read", {});
		r.text.length + ":" + r.truncated + ":" + (r.text === "é".repeat(r.text.length))
	`, limits)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Refusal != "" {
		t.Fatalf("refused: %s — %s", resp.Refusal, resp.Message)
	}
	// 5 whole runes, flagged, and every character intact — a byte cut would
	// have produced a replacement character and failed the third term.
	if resp.Value != `"5:true:true"` {
		t.Errorf("the cut result reports %s, want \"5:true:true\" (five whole runes, flagged, undamaged)", resp.Value)
	}
}

// No server, no binding. A capability that exists and refuses teaches a script
// to retry; one that was never installed is an honest "call is not defined".
func TestWithoutAToolServerTheBindingIsAbsentRatherThanRefusing(t *testing.T) {
	t.Parallel()

	resp, err := Host{Binary: realBinary(t)}.Run(context.Background(), `typeof call`, smallLimits())
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if resp.Value != `"undefined"` {
		t.Errorf("a host with no tool server exposes call as %s, want \"undefined\"", resp.Value)
	}
}

// The server aborting is neither the script's fault nor the host's: the caller
// gets the server's own error, because what must happen next — pause the run,
// ask the operator — is the caller's business.
func TestAnAbortingServerReachesTheCallerAsAnError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the tool needs the operator")
	srv := &recordingServer{answer: func(string, json.RawMessage) (ToolOutcome, error) {
		return ToolOutcome{}, sentinel
	}}

	_, err := toolHost(t, srv).Run(context.Background(), `call("ask_user", {})`, smallLimits())
	if err == nil {
		t.Fatal("an aborting server produced no error: the caller cannot know it must pause the run")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the caller got %v, which does not wrap the server's own error", err)
	}
}

// serveToolCalls is the parent's half and is driven here over in-memory pipes,
// without a child: the shapes below are what a MISBEHAVING child sends, and a
// real child cannot be asked to misbehave — its argv is fixed and its
// environment is empty by construction.
func TestTheParentRefusesEveryFrameItDoesNotServe(t *testing.T) {
	t.Parallel()

	limits, _ := smallLimits().Clamp()
	for _, tc := range []struct {
		name, line, wantErr string
	}{
		{"a result frame, which only the parent sends", `{"type":"result","id":1}`, `only carries "call"`},
		{"a frame with no type, which is what an old child's bare Response unmarshals to",
			`{"value":"42"}`, `only carries "call"`},
		{"an unknown type", `{"type":"store","id":1}`, `only carries "call"`},
		{"an id out of step", `{"type":"call","id":7,"name":"ping"}`, "out of step"},
		{"not JSON at all", `{`, "decoding a tool-channel frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			_, err := serveToolCalls(context.Background(), strings.NewReader(tc.line+"\n"), &out,
				&recordingServer{}, limits)
			if err == nil {
				t.Fatalf("the parent served %s and reported nothing", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the error is %q, which does not say %q", err, tc.wantErr)
			}
		})
	}
}

// A frame longer than the bound is refused rather than buffered: the one
// process the child's rlimits do not cover is the PARENT, so a child must not
// be able to choose how much the parent allocates.
func TestAnOversizedFrameIsRefusedRatherThanBuffered(t *testing.T) {
	t.Parallel()

	limits, _ := smallLimits().Clamp()
	limits.ScriptBytes = 1 << 10

	huge := `{"type":"call","id":1,"name":"ping","input":{"pad":"` + strings.Repeat("x", 4<<10) + `"}}`
	var out strings.Builder
	_, err := serveToolCalls(context.Background(), strings.NewReader(huge+"\n"), &out, &recordingServer{}, limits)
	if err == nil {
		t.Fatal("a 4 KiB frame passed a 1 KiB bound without a word")
	}
	if !strings.Contains(err.Error(), "does not carry") {
		t.Errorf("the error is %q, which does not name the framing bound", err)
	}
}

// The protocol version is checked by the CHILD, because the child is the side
// that would otherwise run a script believing in a capability its parent does
// not serve.
func TestAChildRefusesAToolChannelItDoesNotSpeak(t *testing.T) {
	t.Parallel()

	resp := childWithRequest(t, Request{
		Script:      `call("ping", {})`,
		Limits:      smallLimits(),
		ToolChannel: true,
		Proto:       Proto + 1,
		// The child here is the TEST binary, which is cgo-linked on the -race
		// leg and therefore cannot confine its own threads. Without this the
		// posture verdict refuses first and the protocol check is never
		// reached — the arm would then pass for the wrong reason, which is how
		// this package's /proc arm was green for a prctl it never exercised.
		AllowSingleThreadConfinement: true,
	})
	if resp.Refusal != RefusalUnavailable {
		t.Fatalf("a child handed protocol %d answered %q (%s), want %s",
			Proto+1, resp.Refusal, resp.Message, RefusalUnavailable)
	}
	if !strings.Contains(resp.Message, "not the same build") {
		t.Errorf("the refusal does not name the skew: %q", resp.Message)
	}
}
