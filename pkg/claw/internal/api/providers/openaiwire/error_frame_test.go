package openaiwire

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/httputil"
)

func translate(t *testing.T, frames ...string) []api.StreamEvent {
	t.Helper()
	body := strings.Join(frames, "\n\n") + "\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	ch := make(chan api.StreamEvent, 64)
	go StreamEvents(context.Background(), resp, ch)
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

// A failure frame inside a 200 stream ends it as an error — even when a
// [DONE] follows, which would otherwise read as a clean finish.
func TestStreamEvents_ErrorFrameIsAnError(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   string
	}{
		{"after partial content", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: {"error":{"message":"upstream exploded","type":"server_error","code":500}}`,
			`data: [DONE]`,
		}, "upstream exploded"},
		{"before any content", []string{
			`data: {"error":{"message":"upstream exploded","type":"server_error","code":"overloaded"}}`,
			`data: [DONE]`,
		}, "upstream exploded"},
		{"bare string", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: {"error":"upstream exploded"}`,
			`data: [DONE]`,
		}, "upstream exploded"},
		{"alongside choices", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"error"}],"error":{"message":"upstream exploded","code":502}}`,
			`data: [DONE]`,
		}, "upstream exploded"},
		{"next to a mistyped field", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: {"usage":{"prompt_tokens":12.5},"error":{"message":"upstream exploded"}}`,
			`data: [DONE]`,
		}, "upstream exploded"},
		{"non-JSON frame", []string{
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			`data: Object of type Response is not JSON serializable`,
			`data: {"choices":[{"index":0,"delta":{"content":"tail"},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}, "not JSON serializable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := translate(t, tc.frames...)
			var errMsg string
			for _, ev := range events {
				switch ev.Type {
				case api.EventError:
					errMsg = ev.ErrorMessage
				case api.EventMessageStop, api.EventMessageDelta:
					t.Fatalf("stream finished cleanly (%s) despite an error frame", ev.Type)
				}
			}
			if !strings.Contains(errMsg, tc.want) {
				t.Fatalf("error event = %q, want it to carry %q", errMsg, tc.want)
			}
		})
	}
}

// Tool-call deltas before the error are partial: no block is closed as if
// complete, and the error is the stream's last word.
func TestStreamEvents_ErrorAfterToolCallDeltas(t *testing.T) {
	events := translate(t,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`data: {"error":{"message":"upstream exploded"}}`,
		`data: [DONE]`,
	)
	if len(events) == 0 || events[len(events)-1].Type != api.EventError {
		t.Fatalf("last event must be the error, got %+v", events)
	}
	for _, ev := range events {
		if ev.Type == api.EventContentBlockStop {
			t.Fatalf("a partial tool call was closed as complete: %+v", events)
		}
	}
}

// Empty error values are not failures: a stream that carries them next to a
// normal finish completes, content included.
func TestStreamEvents_EmptyErrorValuesAreNotErrors(t *testing.T) {
	for _, empty := range []string{`null`, ` null `, `{}`, `false`, `""`, `0`, `[]`, `{"message":null,"type":null,"code":null}`} {
		t.Run(empty, func(t *testing.T) {
			events := translate(t,
				`data: {"choices":[{"index":0,"delta":{"content":"ok"}}],"error":`+empty+`}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"error":`+empty+`}`,
				`data: [DONE]`,
			)
			var text string
			for _, ev := range events {
				if ev.Type == api.EventError {
					t.Fatalf("healthy stream with error=%s failed: %s", empty, ev.ErrorMessage)
				}
				if ev.Type == api.EventContentBlockDelta {
					text += ev.Delta.Text
				}
			}
			if text != "ok" || events[len(events)-1].Type != api.EventMessageStop {
				t.Fatalf("error=%s: text %q, events %+v", empty, text, events)
			}
		})
	}
}

func TestChunkErrorMessage(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{``, "", false},
		{`null`, "", false},
		{`false`, "", false},
		{`{}`, "", false},
		{`""`, "", false},
		{`{"message":null,"code":0}`, "", false},
		{`"rate limited"`, "rate limited", true},
		{`{"message":"boom","type":"server_error","code":500}`, "boom (type=server_error, code=500)", true},
		{`{"message":"boom","code":"overloaded"}`, "boom (code=overloaded)", true},
		{`{"detail":"weird"}`, `{"detail":"weird"}`, true},
	}
	for _, tc := range cases {
		got, ok := Chunk{Error: []byte(tc.raw)}.ErrorMessage()
		if ok != tc.ok || got != tc.want {
			t.Errorf("ErrorMessage(%s) = %q, %t; want %q, %t", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// Whatever the endpoint sends, the message it can put into errors and logs
// is bounded.
func TestChunkErrorMessage_Bounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	for _, raw := range []string{
		`{"message":"` + huge + `"}`,
		`"` + huge + `"`,
		`{"detail":"` + huge + `"}`,
	} {
		got, ok := Chunk{Error: []byte(raw)}.ErrorMessage()
		if !ok {
			t.Fatal("an oversized error must still be an error")
		}
		if n := utf8.RuneCountInString(got); n > httputil.BodyTruncateForLog+16 {
			t.Fatalf("error message is %d runes, want at most ~%d", n, httputil.BodyTruncateForLog)
		}
	}
	msg, ok := UnparsedFrameError(huge)
	if !ok || utf8.RuneCountInString(msg) > httputil.BodyTruncateForLog+16 {
		t.Fatalf("unparseable frame: ok=%t, %d runes", ok, utf8.RuneCountInString(msg))
	}
}

// Control: a normal stream, with a keepalive data line, is not mistaken for a
// failure.
func TestStreamEvents_NoErrorFrameFinishesCleanly(t *testing.T) {
	events := translate(t,
		`data:`,
		`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"error":null}`,
		`data: [DONE]`,
	)
	for _, ev := range events {
		if ev.Type == api.EventError {
			t.Fatalf("unexpected error: %s", ev.ErrorMessage)
		}
	}
	if len(events) == 0 || events[len(events)-1].Type != api.EventMessageStop {
		t.Fatalf("stream did not finish with message_stop: %+v", events)
	}
}

// An event named "error" is a failure whatever its payload: a detail that
// no error field carries (FastAPI-style) still ends the stream as an error,
// with its text.
func TestStreamEvents_ErrorEventIsAnError(t *testing.T) {
	for _, payload := range []string{
		`{"detail":"Model is too busy (ReadTimeout)"}`,
		`Model is too busy (ReadTimeout)`,
	} {
		events := translate(t,
			`data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`,
			"event: error\ndata: "+payload,
			`data: [DONE]`,
		)
		last := events[len(events)-1]
		if last.Type != api.EventError || !strings.Contains(last.ErrorMessage, "too busy") {
			t.Fatalf("payload %s: last event = %+v, want an error carrying the detail", payload, last)
		}
		if strings.Contains(last.ErrorMessage, "{") {
			t.Fatalf("payload %s: message %q is the raw payload, want the detail extracted", payload, last.ErrorMessage)
		}
	}
}

// A named non-error event whose data is no chunk (a server ping carrying a
// timestamp) is skipped; the name ends with its event, so a later unnamed
// frame that is no chunk is still a failure.
func TestStreamEvents_NamedNonErrorEvents(t *testing.T) {
	events := translate(t,
		"event: ping\r\ndata: 2023-04-18 22:39:17.123456\r",
		`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	)
	if last := events[len(events)-1]; last.Type != api.EventMessageStop {
		t.Fatalf("a ping event failed a healthy stream: %+v", events)
	}
	events = translate(t,
		"event: ping\ndata: 2023-04-18 22:39:17.123456",
		`data: garbage from the gateway`,
		`data: [DONE]`,
	)
	if last := events[len(events)-1]; last.Type != api.EventError {
		t.Fatalf("an unnamed non-chunk frame after a ping must fail the stream: %+v", events)
	}
	// JSON chunks under a named event are still chunks.
	events = translate(t,
		"event: message\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}",
		`data: [DONE]`,
	)
	var text string
	for _, ev := range events {
		if ev.Type == api.EventContentBlockDelta {
			text += ev.Delta.Text
		}
	}
	if text != "ok" {
		t.Fatalf("a chunk under a named event was dropped: %+v", events)
	}
}

// [DONE] is matched by prefix, like the openai-python stream decoder.
func TestStreamEvents_DoneByPrefix(t *testing.T) {
	events := translate(t,
		`data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE] ok`,
	)
	if last := events[len(events)-1]; last.Type != api.EventMessageStop {
		t.Fatalf("[DONE] with trailing text must end the stream cleanly: %+v", events)
	}
}
