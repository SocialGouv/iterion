package compatgw

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

// observe runs body through the observer the transport installs and
// returns what the reader got and what the capture kept.
func observe(t *testing.T, body io.Reader) (string, *ReasoningCapture) {
	t.Helper()
	c := NewReasoningCapture()
	o := &sseObserver{body: io.NopCloser(body), capture: c}
	got, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("read through the observer: %v", err)
	}
	return string(got), c
}

const reasoningStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"Weigh 1.1\"}}]}\r\n\r\n" +
	"data:{\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\" then answer: \"}}]}\n\n" +
	": keepalive\n\n" +
	"event: message\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"{\\\"ok\\\":true}\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"visible\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

// The observer hands every byte through unchanged, and collects the
// reasoning (both field names, `data:` with or without its space, CRLF
// lines) and the raw finish_reason — even when the stream arrives one byte
// at a time, so every line is split across reads.
func TestSSEObserver_CollectsReasoningAndFinishWithoutTouchingTheStream(t *testing.T) {
	for name, body := range map[string]io.Reader{
		"whole":        strings.NewReader(reasoningStream),
		"byte by byte": iotest.OneByteReader(strings.NewReader(reasoningStream)),
	} {
		t.Run(name, func(t *testing.T) {
			got, c := observe(t, body)
			if got != reasoningStream {
				t.Fatalf("the observer altered the stream:\n got %q\nwant %q", got, reasoningStream)
			}
			if r := c.Reasoning(); r != `Weigh 1.1 then answer: {"ok":true}` {
				t.Errorf("Reasoning() = %q", r)
			}
			if f := c.FinishReason(); f != "length" {
				t.Errorf("FinishReason() = %q, want the raw \"length\"", f)
			}
		})
	}
}

// A gateway mid-way through the reasoning_content → reasoning rename sends
// the same text under both names: it is kept once.
func TestSSEObserver_ADeltaCarryingBothNamesIsKeptOnce(t *testing.T) {
	_, c := observe(t, strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"same\",\"reasoning\":\"same\"}}]}\n\n"))
	if r := c.Reasoning(); r != "same" {
		t.Errorf("Reasoning() = %q, want the text once", r)
	}
}

// Frames the observer cannot read are skipped, never fatal: an odd
// reasoning type, a null, a non-JSON data line, an over-long line. The
// frames around them still count.
func TestSSEObserver_SkipsWhatItCannotRead(t *testing.T) {
	long := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"" + strings.Repeat("x", maxObservedLine) + "\"}}]}\n\n"
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning\":{\"summary\":\"obj\"}}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":null},\"finish_reason\":null}]}\n\n" +
		"data: not json at all\n\n" +
		long +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"kept\"}}]}\n\n"
	got, c := observe(t, strings.NewReader(stream))
	if got != stream {
		t.Fatal("the observer altered the stream")
	}
	if r := c.Reasoning(); r != "kept" {
		t.Errorf("Reasoning() = %.40q…, want only the readable frame's text", r)
	}
	if f := c.FinishReason(); f != "" {
		t.Errorf("FinishReason() = %q, want none (null is no reason)", f)
	}
}

// The capture is bounded: past maxCapturedReasoning it stops growing and
// says so.
func TestReasoningCapture_IsBounded(t *testing.T) {
	c := NewReasoningCapture()
	c.add(strings.Repeat("a", maxCapturedReasoning-1), "")
	c.add("bc", "")
	c.add("d", "")
	if !c.Truncated() {
		t.Error("a capture past its bound does not say it was truncated")
	}
	if got := len(c.Reasoning()); got != maxCapturedReasoning-1 {
		t.Errorf("kept %d bytes, want the %d that fit before the bound", got, maxCapturedReasoning-1)
	}
}

// Through a real gateway client the transport feeds the capture of the
// request's context — and only a successful response, and only a request
// that carries one.
func TestObservingTransport_FeedsTheRequestsCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fail") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"from an error body\"}}]}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, reasoningStream)
	}))
	defer srv.Close()
	t.Setenv(SandboxProxyEndpointEnv, "")
	tr, err := guardedTransport(false)
	if err != nil {
		t.Fatalf("guardedTransport: %v", err)
	}
	client := &http.Client{Transport: observingTransport{next: tr}}
	get := func(ctx context.Context, fail bool) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if fail {
			req.Header.Set("X-Fail", "1")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}

	c := NewReasoningCapture()
	get(WithReasoningCapture(context.Background(), c), false)
	if !strings.HasPrefix(c.Reasoning(), "Weigh 1.1") || c.FinishReason() != "length" {
		t.Errorf("capture = %q / %q, want the stream's reasoning and finish", c.Reasoning(), c.FinishReason())
	}

	refused := NewReasoningCapture()
	get(WithReasoningCapture(context.Background(), refused), true)
	if refused.Reasoning() != "" {
		t.Errorf("an error response fed the capture: %q", refused.Reasoning())
	}

	// No capture on the context: the response is read as is.
	get(context.Background(), false)

	if WithReasoningCapture(context.Background(), nil) != context.Background() {
		t.Error("a nil capture changed the context")
	}
}
