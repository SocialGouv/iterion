package openaiwire

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// streamOver runs the chat-completions stream translation over a raw body.
func streamOver(body io.Reader) []api.StreamEvent {
	return streamResponse(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(body),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// streamResponse runs the translation over a response and collects what it
// emits.
func streamResponse(resp *http.Response) []api.StreamEvent {
	ch := make(chan api.StreamEvent, 64)
	go StreamEvents(context.Background(), resp, ch)
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func finalUsage(t *testing.T, events []api.StreamEvent) api.UsageDelta {
	t.Helper()
	for _, ev := range events {
		if ev.Type == api.EventMessageDelta {
			return ev.Usage
		}
	}
	t.Fatalf("no message_delta in %+v", events)
	return api.UsageDelta{}
}

func streamError(events []api.StreamEvent) string {
	for _, ev := range events {
		if ev.Type == api.EventError {
			return ev.ErrorMessage
		}
	}
	return ""
}

const contentFrame = `data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n"

func errorEvent(t *testing.T, events []api.StreamEvent) api.StreamEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Type == api.EventError {
			return ev
		}
	}
	t.Fatalf("no error event in %+v", events)
	return api.StreamEvent{}
}

const (
	finishFrame = `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	usageFrame  = `data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}` + "\n\n"
)

// The final usage is the provider's account only when a usage chunk naming
// a counter arrived whole: no chunk, or an object naming none, is no report;
// an explicit zero is one.
func TestStreamEvents_UsageIsReportedOnlyWhenTheProviderSentIt(t *testing.T) {
	cases := map[string]struct {
		body     string
		reported bool
		input    int
	}{
		"no usage chunk":         {contentFrame + finishFrame + "data: [DONE]\n\n", false, 0},
		"usage chunk":            {contentFrame + finishFrame + usageFrame + "data: [DONE]\n\n", true, 11},
		"explicit zero":          {contentFrame + finishFrame + `data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` + "\n\ndata: [DONE]\n\n", true, 0},
		"empty usage object":     {contentFrame + finishFrame + `data: {"choices":[],"usage":{}}` + "\n\ndata: [DONE]\n\n", false, 0},
		"only total_tokens":      {contentFrame + finishFrame + `data: {"choices":[],"usage":{"total_tokens":16}}` + "\n\ndata: [DONE]\n\n", false, 0},
		"only completion_tokens": {contentFrame + finishFrame + `data: {"choices":[],"usage":{"completion_tokens":5}}` + "\n\ndata: [DONE]\n\n", true, 0},
		"only prompt_tokens":     {contentFrame + finishFrame + `data: {"choices":[],"usage":{"prompt_tokens":11}}` + "\n\ndata: [DONE]\n\n", true, 11},
		"null usage":             {contentFrame + finishFrame + `data: {"choices":[],"usage":null}` + "\n\ndata: [DONE]\n\n", false, 0},
		"last line unterminated": {contentFrame + finishFrame + strings.TrimSuffix(usageFrame, "\n\n"), true, 11},
		// A whole JSON value that is no chunk, left without its newline, is
		// no cut line.
		"whole value unterminated": {contentFrame + finishFrame + usageFrame + `data: {"choices":"oops"}`, true, 11},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			usage := finalUsage(t, streamOver(strings.NewReader(tc.body)))
			if usage.Reported != tc.reported || usage.InputTokens != tc.input {
				t.Errorf("usage = %+v, want Reported %v with %d input tokens", usage, tc.reported, tc.input)
			}
		})
	}
}

// A connection cut inside the line that followed the finish reason may have
// been a later usage chunk: the usage seen before it — here a cumulative
// count some gateways send on every chunk — is no final account.
func TestStreamEvents_ACutAfterTheFinishLeavesUsageUnreported(t *testing.T) {
	early := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}` + "\n\n"
	usage := finalUsage(t, streamOver(strings.NewReader(contentFrame+early+`data: {"choices":[],"usage":{"prompt_tok`)))
	if usage.Reported {
		t.Errorf("usage = %+v, want unreported: the final usage chunk was cut", usage)
	}
}

// An error event carries the usage the stream had reported before failing,
// which is no final account; an error frame that carries its own usage is
// the provider's account of the failed call.
func TestStreamEvents_AnErrorCarriesTheUsageSoFar(t *testing.T) {
	t.Setenv("CLAW_STREAM_IDLE_TIMEOUT", "100ms")
	cumulative := `data: {"choices":[{"index":0,"delta":{"content":"Hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":1}}` + "\n\n"
	text := func(s string) func(t *testing.T) io.ReadCloser {
		return func(*testing.T) io.ReadCloser { return io.NopCloser(strings.NewReader(s)) }
	}
	cases := map[string]struct {
		body     func(t *testing.T) io.ReadCloser
		reported bool
		input    int
	}{
		"error frame after a cumulative count": {text(cumulative + `data: {"error":{"message":"boom","type":"server_error"}}` + "\n\n"), false, 11},
		"error frame with its own usage":       {text(contentFrame + `data: {"error":{"message":"boom","type":"server_error"},"usage":{"prompt_tokens":13,"completion_tokens":4}}` + "\n\n"), true, 13},
		"truncated stream":                     {text(cumulative), false, 11},
		"read error": {func(*testing.T) io.ReadCloser {
			return io.NopCloser(io.MultiReader(strings.NewReader(cumulative), failingReader{io.ErrUnexpectedEOF}))
		}, false, 11},
		"stalled stream": {func(t *testing.T) io.ReadCloser {
			pr, pw := io.Pipe()
			go func() { _, _ = io.WriteString(pw, cumulative) }() // then silence
			t.Cleanup(func() { _ = pw.Close() })
			return pr
		}, false, 11},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := errorEvent(t, streamResponse(&http.Response{StatusCode: http.StatusOK, Body: tc.body(t)}))
			if ev.Usage.Reported != tc.reported || ev.Usage.InputTokens != tc.input {
				t.Errorf("error %q carries %+v, want Reported %v with %d input tokens", ev.ErrorMessage, ev.Usage, tc.reported, tc.input)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// A connection cut inside a data line is a truncated stream — retryable —
// not an unparseable frame the server sent: the scanner hands the line's
// head over as a last token whether the read ended on EOF or on an error.
func TestStreamEvents_ACutLineIsATruncatedStream(t *testing.T) {
	const partial = `data: {"choices":[{"index":0,"delta":{"content":"Hel`
	cases := map[string]struct {
		body io.Reader
		want string
	}{
		"EOF inside the line":   {strings.NewReader(contentFrame + partial), "openai stream truncated"},
		"error inside the line": {io.MultiReader(strings.NewReader(contentFrame+partial), failingReader{io.ErrUnexpectedEOF}), "openai stream read: unexpected EOF"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := streamError(streamOver(tc.body))
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("stream error = %q, want %q…", got, tc.want)
			}
		})
	}
}

// A whole data line that is no chunk is still the error frame it was.
func TestStreamEvents_AWholeUnparseableLineIsStillAnError(t *testing.T) {
	got := streamError(streamOver(strings.NewReader(contentFrame + "data: not json at all\n\n")))
	if !strings.Contains(got, "unparseable data frame") {
		t.Errorf("stream error = %q, want an unparseable-frame error", got)
	}
}

// An error object's message is cut before its type and code are appended:
// a long message never costs the caller the verdict.
func TestErrorFrame_LongMessageKeepsItsVerdict(t *testing.T) {
	long := strings.Repeat("upstream detail ", 200)
	got := streamError(streamOver(strings.NewReader(contentFrame +
		`data: {"error":{"message":"` + long + `","type":"invalid_request_error","code":"context_length_exceeded"}}` + "\n\n")))
	if !strings.HasSuffix(got, "(type=invalid_request_error, code=context_length_exceeded)") {
		t.Errorf("stream error ends %q, want the type and code kept", got[max(0, len(got)-80):])
	}
	if len(got) > 1200 {
		t.Errorf("stream error is %d bytes, want it bounded", len(got))
	}
}

// A bare-string error frame's error_type (Hugging Face TGI) travels as its
// type.
func TestErrorFrame_ErrorTypeBesideABareString(t *testing.T) {
	got := streamError(streamOver(strings.NewReader(contentFrame +
		`data: {"error":"Input validation error: inputs too long","error_type":"validation"}` + "\n\n")))
	if got != "openai stream error: Input validation error: inputs too long (type=validation)" {
		t.Errorf("stream error = %q", got)
	}
}

// An `event: error` whose data is itself the error object keeps its type and
// code.
func TestErrorEvent_TopLevelTypeAndCode(t *testing.T) {
	got := streamError(streamOver(strings.NewReader(contentFrame +
		"event: error\ndata: {\"message\":\"no such model\",\"type\":\"invalid_request_error\",\"code\":\"model_not_found\"}\n\n")))
	if got != "openai stream error: no such model (type=invalid_request_error, code=model_not_found)" {
		t.Errorf("stream error = %q", got)
	}
}

// A body cut short of its Content-Length reaches the parser as an
// unexpected EOF from net/http, inside the line it cut: a read failure,
// never a frame.
func TestStreamEvents_ABodyShortOfItsContentLength(t *testing.T) {
	const partial = `data: {"choices":[{"index":0,"delta":{"content":"Hel`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		body := contentFrame + partial
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n%s", len(body)+500, body)
		_ = buf.Flush()
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	ev := errorEvent(t, streamResponse(resp))
	if ev.ErrorMessage != "openai stream read: unexpected EOF" {
		t.Errorf("stream error = %q, want the read failure", ev.ErrorMessage)
	}
}
