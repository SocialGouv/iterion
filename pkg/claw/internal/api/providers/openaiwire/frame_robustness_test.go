package openaiwire

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/httputil"
)

// No field of an error frame unbounds the error built from it: the message
// is cut at BodyTruncateForLog, each type or code at FieldTruncateForLog.
func TestErrorFrame_EveryFieldIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	limit := len("openai stream error: ") + httputil.BodyTruncateForLog + 2*httputil.FieldTruncateForLog + 64
	for name, frame := range map[string]string{
		"message":        `{"error":{"message":"` + huge + `","type":"t","code":"c"}}`,
		"type":           `{"error":{"message":"m","type":"` + huge + `","code":"c"}}`,
		"code":           `{"error":{"message":"m","type":"t","code":"` + huge + `"}}`,
		"code as object": `{"error":{"message":"m","type":"t","code":{"k":"` + huge + `"}}}`,
		"error_type":     `{"error":"m","error_type":"` + huge + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := streamError(streamOver(strings.NewReader(contentFrame + "data: " + frame + "\n\n")))
			if got == "" || len([]rune(got)) > limit {
				t.Errorf("error is %d runes, want 1..%d", len([]rune(got)), limit)
			}
		})
	}
}

// An `event: error` announces an error whatever came of its data: a payload
// the connection cut is still that error, before or after the finish.
func TestErrorEvent_ACutPayloadIsStillTheError(t *testing.T) {
	const payload = "event: error\ndata: {\"error\":{\"message\":\"quota exceeded for org"
	for name, body := range map[string]string{
		"before the finish": contentFrame + payload,
		"after the finish":  contentFrame + finishFrame + payload,
	} {
		t.Run(name, func(t *testing.T) {
			got := streamError(streamOver(strings.NewReader(body)))
			if !strings.HasPrefix(got, "openai stream error: ") || !strings.Contains(got, "quota exceeded for org") {
				t.Errorf("stream error = %q, want the provider's error text", got)
			}
		})
	}
}

// A chunk whose error_type is not a string still decodes: its content is
// kept, and beside a bare-string error the value is named as written.
func TestStreamEvents_AnOddErrorTypeKeepsTheFrame(t *testing.T) {
	events := streamOver(strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"Hello"}}],"error_type":5}` + "\n\n" + finishFrame + "data: [DONE]\n\n"))
	if text := deltaText(events); text != "Hello" {
		t.Errorf("text = %q, want the chunk's content", text)
	}
	if got := streamError(streamOver(strings.NewReader(contentFrame + `data: {"error":"Input validation error","error_type":5}` + "\n\n"))); got != "openai stream error: Input validation error (type=5)" {
		t.Errorf("stream error = %q", got)
	}
}

// error_type names the kind of a bare-string error, never a second type
// beside an error object's own; a bare string ending in a parenthesis keeps
// it.
func TestErrorFrame_ErrorTypeOnlyBesideABareString(t *testing.T) {
	for frame, want := range map[string]string{
		`{"error":{"message":"m","type":"t"},"error_type":"validation"}`:        "openai stream error: m (type=t)",
		`{"error":"Inputs too long (max 4096)","error_type":"validation"}`:      "openai stream error: Inputs too long (max 4096) (type=validation)",
		`{"error":{"message":"m","type":"t","code":null}}`:                      "openai stream error: m (type=t)",
		`{"error":{"message":"inner","code":"c"},"message":"outer","type":"x"}`: "openai stream error: inner (code=c)",
	} {
		if got := streamError(streamOver(strings.NewReader(contentFrame + "data: " + frame + "\n\n"))); got != want {
			t.Errorf("frame %s: stream error %q, want %q", frame, got, want)
		}
	}
}

// An `event: error` whose data is no chunk, naming its error both nested and
// at the top level, is read from the nested object.
func TestErrorEvent_TheNestedErrorWins(t *testing.T) {
	if got := streamError(streamOver(strings.NewReader(contentFrame + "event: error\ndata: {\"choices\":\"x\",\"error\":{\"message\":\"inner\",\"code\":\"c\"},\"message\":\"outer\",\"type\":\"x\"}\n\n"))); got != "openai stream error: inner (code=c)" {
		t.Errorf("event error = %q, want the nested error", got)
	}
}

// A connection that drops inside any line — not only a JSON data line —
// leaves the usage seen before it unreported.
func TestStreamEvents_ACutOutsideAJSONLineLeavesUsageUnreported(t *testing.T) {
	early := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}` + "\n\n"
	for _, tail := range []string{"dat", "data: ", "event: mess", ": keep", "\r"} {
		t.Run(fmt.Sprintf("%q", tail), func(t *testing.T) {
			usage := finalUsage(t, streamOver(strings.NewReader(contentFrame+early+tail)))
			if usage.Reported {
				t.Errorf("usage = %+v, want unreported: a line was cut after it", usage)
			}
		})
	}
}

// An error frame that does not decode as a chunk lends no usage: what a
// partial decode left in it is no account.
func TestStreamEvents_AnUndecodedErrorFrameLendsNoUsage(t *testing.T) {
	ev := errorEvent(t, streamOver(strings.NewReader(contentFrame+`data: {"error":{"message":"boom"},"choices":"x","usage":{"prompt_tokens":5,"completion_tokens":1}}`+"\n\n")))
	if ev.Usage.Reported {
		t.Errorf("error %q carries %+v, want its usage unreported", ev.ErrorMessage, ev.Usage)
	}
}

// net/http hands the last bytes of a Content-Length body over together with
// io.EOF, so the stream's final lines are split at EOF: only the newline
// check keeps a whole terminated line from reading as a cut.
func TestStreamEvents_AWholeLastLineOverRealHTTP(t *testing.T) {
	for name, tc := range map[string]struct {
		tail, wantErr string
	}{
		"unparseable frame": {"data: not json at all\n\n", "openai stream error: unparseable data frame: not json at all"},
		"keepalive comment": {": keepalive\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			body := contentFrame + finishFrame + usageFrame + tc.tail
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			events := streamResponse(resp)
			if got := streamError(events); got != tc.wantErr {
				t.Fatalf("stream error = %q, want %q", got, tc.wantErr)
			}
			if tc.wantErr == "" && !finalUsage(t, events).Reported {
				t.Error("a whole terminated last line was read as a cut")
			}
		})
	}
}

// deltaText concatenates the text deltas a stream emitted.
func deltaText(events []api.StreamEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == api.EventContentBlockDelta {
			b.WriteString(ev.Delta.Text)
		}
	}
	return b.String()
}

// A last line no chunk can start with is what the endpoint wrote, reported
// with its text before or after the finish; a cut chunk or a cut [DONE]
// stays a cut.
func TestStreamEvents_AnUnterminatedTextLineIsTheEndpointsError(t *testing.T) {
	const text = "data: upstream error: Rate limit exceeded"
	for name, body := range map[string]string{
		"before the finish": contentFrame + text,
		"after the finish":  contentFrame + finishFrame + text,
	} {
		if got := streamError(streamOver(strings.NewReader(body))); got != "openai stream error: unparseable data frame: upstream error: Rate limit exceeded" {
			t.Errorf("%s: stream error = %q, want the endpoint's text", name, got)
		}
	}
	for name, tail := range map[string]string{"cut chunk": `data: {"choices":[],"usage":{"prompt_tok`, "cut [DONE]": "data: [DON"} {
		if got := streamError(streamOver(strings.NewReader(contentFrame + finishFrame + tail))); got != "" {
			t.Errorf("%s after the finish: stream error = %q, want a clean finish", name, got)
		}
	}
}

// A connection cut inside an `event: error` block is that error, whatever
// came of its data, after the finish too.
func TestErrorEvent_ACutBlockIsStillAnError(t *testing.T) {
	for _, tail := range []string{"event: error\ndata: ", "event: error\ndat", "event: error\n: ping"} {
		if got := streamError(streamOver(strings.NewReader(contentFrame + finishFrame + tail))); got != "openai stream error: error event with no data" {
			t.Errorf("%q: stream error = %q, want the announced error", tail, got)
		}
	}
}

// An `event: error` whose object names a type but no message keeps the
// provider's words, under whatever key they came.
func TestErrorEvent_ATypeWithoutAMessageKeepsTheText(t *testing.T) {
	got := streamError(streamOver(strings.NewReader(contentFrame + "event: error\ndata: {\"type\":\"error\",\"error_message\":\"upstream connect error\"}\n\n")))
	if !strings.Contains(got, "upstream connect error") {
		t.Errorf("stream error = %q, want the provider's text", got)
	}
}

// An empty error carries no failure, whatever error_type names beside it.
func TestStreamEvents_AnEmptyErrorBesideAnErrorTypeIsNoError(t *testing.T) {
	for _, empty := range []string{`null`, `""`, `"  "`, `false`, `0`, `{}`, `[]`} {
		body := `data: {"choices":[{"index":0,"delta":{"content":"Hello"}}],"error":` + empty + `,"error_type":"validation"}` + "\n\n" + finishFrame + "data: [DONE]\n\n"
		events := streamOver(strings.NewReader(body))
		if got := streamError(events); got != "" {
			t.Errorf("error %s beside error_type: stream error %q, want none", empty, got)
		}
		if text := deltaText(events); text != "Hello" {
			t.Errorf("error %s beside error_type: text %q, want the chunk's content", empty, text)
		}
	}
}

// A null error_type names nothing.
func TestErrorFrame_ANullErrorTypeNamesNothing(t *testing.T) {
	if got := streamError(streamOver(strings.NewReader(contentFrame + `data: {"error":"boom","error_type":null}` + "\n\n"))); got != "openai stream error: boom" {
		t.Errorf("stream error = %q", got)
	}
}

// The raw data of an `event: error` that names no field is bounded too.
func TestErrorEvent_RawDataIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	limit := len("openai stream error: ") + httputil.BodyTruncateForLog + 8
	for name, data := range map[string]string{"text": huge, "json": `{"foo":"` + huge + `"}`} {
		got := streamError(streamOver(strings.NewReader(contentFrame + "event: error\ndata: " + data + "\n\n")))
		if got == "" || len([]rune(got)) > limit {
			t.Errorf("%s: error is %d runes, want 1..%d", name, len([]rune(got)), limit)
		}
	}
}
