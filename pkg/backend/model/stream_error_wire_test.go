package model

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/anthropic"
	openaiprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// serveRawSSE starts a raw HTTP/1.1 listener: after reading a request, write
// gets the connection and sends whatever response bytes it wants — a frame,
// a line cut in the middle, a chunked body that stops, a stall.
func serveRawSSE(t *testing.T, write func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				write(c)
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

const (
	sseHeadClose   = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n"
	sseHeadChunked = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
	sseContent     = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	ssePartialLine = `data: {"choices":[{"index":0,"delta":{"content":"Hel`
)

func sseChunk(s string) string { return fmt.Sprintf("%x\r\n%s\r\n", len(s), s) }

// streamErrorOf drives the vendored claw OpenAI client — its real stream
// parser — against a raw server, and returns the error iterion's own
// aggregator classifies the stream's end as. Chat wire; streamEventErrorOf
// takes the model when the wire is what the case is about.
func streamErrorOf(t *testing.T, write func(c net.Conn)) error {
	t.Helper()
	return streamEventErrorOf(t, "gpt-4o", write)
}

// streamEventErrorOf is streamErrorOf with the wire open: the model selects
// it inside claw — gpt-6-* goes to the Responses API, everything else to
// chat completions.
func streamEventErrorOf(t *testing.T, model string, write func(c net.Conn)) error {
	t.Helper()
	client, err := openaiprovider.New().NewClient(api.ProviderConfig{APIKey: "test-key", Model: model, BaseURL: serveRawSSE(t, write)})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     model,
		MaxTokens: 64,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	return aggregateStream(ctx, ch).err
}

// genericWireErrorOf drives claw's GENERIC SSE client — the one the
// anthropic, moonshot and zai providers build (internal/api.NewClient) —
// against a raw server that answers with one `error` frame, and returns the
// error iterion's aggregator classifies it as. That client parses the
// frame's `error.message` and keeps neither its `type` nor its `code`
// (internal/api/client.go), so the provider-verdict branches of
// classifyStreamEventError decline and the stall predicate is the first
// branch that can match such a message — which is why branch order alone
// cannot protect a verdict here.
func genericWireErrorOf(t *testing.T, errorFrame string) error {
	t.Helper()
	client, err := anthropicprovider.New().NewClient(api.ProviderConfig{APIKey: "test-key", Model: "claude-opus-4", BaseURL: serveRawSSE(t, func(c net.Conn) {
		_, _ = io.WriteString(c, sseHeadClose+errorFrame)
	})})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     "claude-opus-4",
		MaxTokens: 64,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	return aggregateStream(ctx, ch).err
}

// What a stream ends on is classified from what claw's parser actually
// emits, not from a string written to look like it: an error frame after the
// provider accepted the request is retried unless it names a permanent
// condition, and a connection cut in the middle of a line or left silent is
// retried like any truncated stream — on the chat wire and the Responses API
// alike.
func TestStreamErrors_FromTheRealParser(t *testing.T) {
	t.Setenv("CLAW_STREAM_IDLE_TIMEOUT", "300ms")
	frames := []struct {
		name      string
		frame     string
		retryable bool
		overflow  bool
	}{
		{"openai server_error", `{"error":{"message":"The server had an error while processing your request.","type":"server_error","param":null,"code":null}}`, true, false},
		{"openrouter server_error code", `{"id":"x","object":"chat.completion.chunk","error":{"code":"server_error","message":"Provider disconnected unexpectedly"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`, true, false},
		{"litellm 429 as a string code", `{"error":{"message":"litellm.RateLimitError: Rate limit reached","type":"None","param":"None","code":"429"}}`, true, false},
		{"litellm 408 timeout", `{"error":{"message":"litellm.Timeout: Request timed out.","type":null,"param":null,"code":"408"}}`, true, false},
		{"vllm 0.10/0.11 mid-stream catch-all", `{"error":{"object":"error","message":"Background loop has errored already.","type":"BadRequestError","param":null,"code":400}}`, true, false},
		{"litellm 409 conflict", `{"error":{"message":"Conflict.","type":null,"param":null,"code":"409"}}`, true, false},
		{"vllm 500 numeric code", `{"error":{"object":"error","message":"EngineCore encountered an issue.","type":"InternalServerError","param":null,"code":500}}`, true, false},
		{"server_error past the truncation length", `{"error":{"message":"The server had an error. ` + strings.Repeat("Upstream detail. ", 60) + `","type":"server_error","param":null,"code":null}}`, true, false},
		{"bare string error", `{"error":"Too Many Requests"}`, true, false},
		{"uppercase type", `{"error":{"message":"Slow down","type":"RATE_LIMIT_EXCEEDED","code":"RATE_LIMIT_EXCEEDED"}}`, true, false},
		{"nested error.error", `{"error":{"error":{"message":"Overloaded","type":"overloaded_error"}}}`, true, false},
		{"context overflow", `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, false, true},
		{"invalid request", `{"error":{"message":"Invalid value for 'tool_choice'.","type":"invalid_request_error","param":"tool_choice","code":null}}`, false, false},
		{"bad api key", `{"error":{"message":"Incorrect API key provided.","type":"authentication_error","code":"invalid_api_key"}}`, false, false},
		{"code-only 404", `{"error":{"code":404}}`, false, false},
		// LiteLLM's APIConnectionError message carries the traceback, so claw
		// cuts the (type, code) suffix away: the exception head is no verdict.
		{"litellm connection error past the truncation length", `{"error":{"message":"litellm.APIConnectionError: peer closed connection without sending complete message body (incomplete chunked read)\nTraceback (most recent call last):\n` + strings.Repeat(`  File \"/usr/lib/python3.11/site-packages/litellm/main.py\", line 1, in completion\n`, 20) + `","type":null,"param":null,"code":"500"}}`, true, false},
		{"python exception as a bare string", `{"error":"RuntimeError: CUDA error: device-side assert triggered"}`, true, false},
		{"exception head in a message-only object", `{"error":{"message":"RateLimitError: Too many requests, please slow down"}}`, true, false},
		{"exception head naming a timeout", `{"error":"Error: upstream request timeout"}`, true, false},
		{"litellm timeout labelled invalid_request_error", `{"error":{"message":"litellm.Timeout: APITimeoutError - Request timed out.","type":"invalid_request_error","param":null,"code":"408"}}`, true, false},
		{"litellm conflict labelled invalid_request_error", `{"error":{"message":"Conflict.","type":"invalid_request_error","param":null,"code":"409"}}`, true, false},
		{"tgi validation beside a bare string", `{"error":"Input validation error: inputs tokens + max_new_tokens must be <= 4096","error_type":"validation"}`, false, false},
		{"tgi overloaded beside a bare string", `{"error":"Model is overloaded","error_type":"overloaded"}`, true, false},
	}
	for _, f := range frames {
		t.Run("frame/"+f.name, func(t *testing.T) {
			err := streamErrorOf(t, func(c net.Conn) {
				_, _ = io.WriteString(c, sseHeadClose+sseContent+"data: "+f.frame+"\n\n")
			})
			if err == nil {
				t.Fatal("the stream ended without an error")
			}
			if got := isRetryable(err); got != f.retryable {
				t.Errorf("isRetryable = %v, want %v (err %T %v)", got, f.retryable, err, err)
			}
			var overflow *ContextOverflowError
			if got := errors.As(err, &overflow); got != f.overflow {
				t.Errorf("context overflow = %v, want %v (err %v)", got, f.overflow, err)
			}
		})
	}

	// An `event: error` whose detail leads with an exception name is no
	// verdict either.
	t.Run("event/exception detail", func(t *testing.T) {
		err := streamErrorOf(t, func(c net.Conn) {
			_, _ = io.WriteString(c, sseHeadClose+sseContent+"event: error\ndata: {\"detail\":\"Timeout: generation took too long\"}\n\n")
		})
		if err == nil || !isRetryable(err) {
			t.Errorf("err = %v, want a retried failure", err)
		}
	})

	// The Responses API ends a failed response with its own code, which
	// claw names in the event: retried unless the code is a refusal.
	for _, rc := range []struct {
		code      string
		retryable bool
	}{{"server_error", true}, {"rate_limit_exceeded", true}, {"slow_down", true}, {"a_code_nobody_documented", true},
		{"invalid_image", false}, {"bio_policy", false}, {"invalid_prompt", false}, {"cyber_policy", false}} {
		t.Run("responses/"+rc.code, func(t *testing.T) {
			base := serveRawSSE(t, func(c net.Conn) {
				_, _ = io.WriteString(c, sseHeadClose+`data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"`+rc.code+`","message":"m"}}}`+"\n\n")
			})
			client, err := openaiprovider.New().NewClient(api.ProviderConfig{APIKey: "test-key", Model: "gpt-6-sol", BaseURL: base})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{Model: "gpt-6-sol", MaxTokens: 64,
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
			if err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			got := aggregateStream(ctx, ch).err
			if got == nil {
				t.Fatal("the stream ended without an error")
			}
			if isRetryable(got) != rc.retryable {
				t.Errorf("isRetryable = %v, want %v (err %v)", isRetryable(got), rc.retryable, got)
			}
		})
	}

	// The Responses API's documented error event carries its code at the top
	// level, and a stream that closes before response.completed was cut.
	responses := []struct {
		name      string
		frames    string
		retryable bool
	}{
		{"error event, transient top-level code", `data: {"type":"error","code":"server_error","message":"boom","param":null,"sequence_number":3}` + "\n\n", true},
		{"error event, refusal top-level code", `data: {"type":"error","code":"invalid_prompt","message":"flagged","param":null,"sequence_number":3}` + "\n\n", false},
		{"closed before response.completed", `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","item_id":"m1","delta":"The answer is: Hel"}` + "\n\n", true},
		{"cut inside an event", `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1"}}` + "\n\n" +
			`data: {"type":"response.output_text.delta","item_id":"m1","del`, true},
	}
	for _, rc := range responses {
		t.Run("responses/"+rc.name, func(t *testing.T) {
			base := serveRawSSE(t, func(c net.Conn) { _, _ = io.WriteString(c, sseHeadClose+rc.frames) })
			client, err := openaiprovider.New().NewClient(api.ProviderConfig{APIKey: "test-key", Model: "gpt-6-sol", BaseURL: base})
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{Model: "gpt-6-sol", MaxTokens: 64,
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
			if err != nil {
				t.Fatalf("StreamResponse: %v", err)
			}
			got := aggregateStream(ctx, ch).err
			if got == nil {
				t.Fatal("the stream ended without an error")
			}
			if isRetryable(got) != rc.retryable {
				t.Errorf("isRetryable = %v, want %v (err %v)", isRetryable(got), rc.retryable, got)
			}
		})
	}

	cuts := []struct {
		name  string
		write func(c net.Conn)
	}{
		{"data line cut, then EOF", func(c net.Conn) { _, _ = io.WriteString(c, sseHeadClose+sseContent+ssePartialLine) }},
		{"data line cut, chunked body stops", func(c net.Conn) {
			_, _ = io.WriteString(c, sseHeadChunked+sseChunk(sseContent)+sseChunk(ssePartialLine))
		}},
		{"whole lines, chunked body stops", func(c net.Conn) { _, _ = io.WriteString(c, sseHeadChunked+sseChunk(sseContent)) }},
		{"silence past claw's idle watchdog", func(c net.Conn) {
			_, _ = io.WriteString(c, sseHeadChunked+sseChunk(sseContent))
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, _ = io.Copy(io.Discard, c) // hold the connection until the client drops it
		}},
	}
	for _, cut := range cuts {
		t.Run("cut/"+cut.name, func(t *testing.T) {
			err := streamErrorOf(t, cut.write)
			if err == nil {
				t.Fatal("the stream ended without an error")
			}
			if !isRetryable(err) {
				t.Errorf("a stream cut is not retried: %T %v", err, err)
			}
		})
	}
}

// stalledStreamError drives claw's real stream parser on the wire the model
// selects — gpt-4o the chat wire, gpt-6-* the Responses API — into claw's
// OWN idle watchdog: the SSE body carries one frame and then goes silent
// past CLAW_STREAM_IDLE_TIMEOUT, so sseutil.IdleWatchdog closes the body and
// the wire reports the stall as a stream error event. The returned error is
// what iterion's aggregator classifies that event as.
func stalledStreamError(t *testing.T, model, firstFrame string) error {
	t.Helper()
	return streamEventErrorOf(t, model, func(c net.Conn) {
		_, _ = io.WriteString(c, sseHeadChunked+sseChunk(firstFrame))
		// Hold the connection open and silent: only the client's own
		// watchdog ends this read.
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.Copy(io.Discard, c)
	})
}

// A stall claw's own per-wire watchdog reports is the same condition
// iterion's stream-silence watchdog stops — a provider stream gone silent —
// so it carries the same typed verdict: both retry loops read it through
// errors.As, the inner loop prices it on the transient attempt budget, the
// outer delegate loop retries it at all, and a `fallbacks:` chain reads the
// same category for the two. claw's own report rides along in the message,
// naming the wire and the window it was configured with.
func TestClawStreamStall_CarriesIterionsOwnIdleVerdict(t *testing.T) {
	t.Setenv("CLAW_STREAM_IDLE_TIMEOUT", "300ms")
	for _, wire := range []struct {
		name       string
		model      string
		firstFrame string
	}{
		{"chat", "gpt-4o", sseContent},
		{"responses", "gpt-6-sol", `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"m1"}}` + "\n\n"},
	} {
		t.Run(wire.name, func(t *testing.T) {
			err := stalledStreamError(t, wire.model, wire.firstFrame)
			if err == nil {
				t.Fatal("the silent stream ended without an error — this witness no longer drives claw's watchdog")
			}
			// The wire's own report is what the operator reads: without it
			// the typed verdict names neither the wire nor the window.
			if !strings.Contains(err.Error(), "stream stalled") || !strings.Contains(err.Error(), "CLAW_STREAM_IDLE_TIMEOUT") {
				t.Fatalf("the error lost the wire's own stall report: %v", err)
			}
			// In the form the retry loops read a generation failure in —
			// the classified error under the claw backend's wrap. The wrap
			// itself is driven for real by
			// TestClawStreamStall_TypedThroughTheBackendsOwnWrap below;
			// this row only spells the shape.
			for _, form := range []struct {
				name string
				err  error
			}{
				{"as classified", err},
				{"wrapped by the backend", fmt.Errorf("claw backend: text generation: %w", err)},
			} {
				var idle *StreamIdleError
				if !errors.As(form.err, &idle) {
					t.Fatalf("%s: the stall is not the typed idle verdict: %T %v", form.name, form.err, form.err)
				}
				if idle.Idle != 300*time.Millisecond {
					t.Errorf("%s: the typed window is %v, want the 300ms claw named", form.name, idle.Idle)
				}
				if idle.Phase != StreamIdleWire {
					t.Errorf("%s: phase = %q, want %q (the wire measured the silence, not us)", form.name, idle.Phase, StreamIdleWire)
				}
				if !isRetryable(form.err) {
					t.Errorf("%s: the inner loop refuses the stall: %v", form.name, form.err)
				}
				if got := (RetryPolicy{}).effectiveMaxAttempts(form.err); got != DefaultMaxAttemptsTransient {
					t.Errorf("%s: inner budget = %d attempts, want the transient %d", form.name, got, DefaultMaxAttemptsTransient)
				}
				if !isDelegateRetryable(form.err) {
					t.Errorf("%s: the outer delegate loop does not retry the stall: %v", form.name, form.err)
				}
				// The parity reaches the `fallbacks:` chain too, and it is
				// not free: a stall is the SAME category as iterion's own
				// stall, transient_exhausted, where it used to be
				// unclassified. elementAccepts bypasses the author's `on:`
				// filter only for unclassified, so a chain whose `on:` does
				// not name transient_exhausted — the default
				// [usage_window, unavailable] included — stops at a stall
				// instead of routing around it. Pinned here so the category
				// cannot move back without a red.
				if got := delegate.ClassifyFallback(form.err, isDelegateRetryable(form.err)); got != delegate.FallbackTransientExhausted {
					t.Errorf("%s: fallback category = %q, want %q (iterion's own stall's category, which honours the author's on: filter)", form.name, got, delegate.FallbackTransientExhausted)
				}
			}
		})
	}
}

// stallStream is one stream a claw wire's own watchdog ends: the EventError
// the watchdog's emit site writes verbatim
// (openaiwire/stream.go — "openai stream stalled: no data for <window> —
// aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)"), production
// default window included. A fresh channel per call: the aggregator drains
// the one it reads.
func stallStream() <-chan api.StreamEvent {
	ch := make(chan api.StreamEvent, 1)
	ch <- api.StreamEvent{
		Type:         api.EventError,
		ErrorMessage: "openai stream stalled: no data for 5m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)",
	}
	close(ch)
	return ch
}

// The claw backend's own wraps are part of the guarantee: Execute dispatches
// on the task's shape to three generation strategies, and each folds the
// classified stall through its own wrap — "claw backend: text generation:
// %w", "claw backend: structured generation: %w", "claw backend: text+tools
// generation: %w" (claw_backend.go) — and every retry-loop reader —
// errors.As, isRetryable, the transient budget, the delegate loop, the
// fallback chain — sees the error in THAT form, not as it left the
// aggregator. A %s at any ONE of the three strips the type for that
// strategy alone, while a witness through the other two stays green — how a
// one-character regression hides. So the wrap is asserted against the
// producer that owns it, at every site that builds it: this witness drives
// ClawBackend.Execute itself with a client whose streams end on a watchdog
// report, once per dispatch shape, and reads the verdict off the error the
// CALLER receives.
func TestClawStreamStall_TypedThroughTheBackendsOwnWrap(t *testing.T) {
	schemaJSON, err := SchemaToJSON(&ir.Schema{
		Name:   "verdict",
		Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}},
	})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, shape := range []struct {
		name         string
		outputSchema json.RawMessage
		hasTools     bool
		wantWrap     string
	}{
		{"a bare prompt: the text generation wrap", nil, false, "claw backend: text generation: "},
		{"a schema without tools: the structured generation wrap", schemaJSON, false, "claw backend: structured generation: "},
		{"a schema with tools: the text+tools generation wrap", schemaJSON, true, "claw backend: text+tools generation: "},
	} {
		t.Run(shape.name, func(t *testing.T) {
			chans := make([]<-chan api.StreamEvent, DefaultMaxAttemptsTransient)
			for i := range chans {
				chans[i] = stallStream()
			}
			mock := &execMockClient{streams: chans}
			reg := NewRegistry()
			reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
			// BackoffBase pinned tiny: the typed stall is retried to the full
			// transient budget here, and the witness must not spend seconds of
			// backoff proving it.
			policy := RetryPolicy{BackoffBase: time.Millisecond}

			_, err := NewClawBackend(reg, EventHooks{}, policy).Execute(context.Background(), delegate.Task{
				NodeID:       "agent1",
				Model:        "test/test-model",
				UserPrompt:   "Say hello",
				OutputSchema: shape.outputSchema,
				HasTools:     shape.hasTools,
			})
			if err == nil {
				t.Fatal("the backend returned no error for a stream its client ended on a watchdog report")
			}
			// The dispatch landed in THIS shape's strategy: a routing change
			// that funnels two shapes through one wrap would otherwise leave
			// the third site unwitnessed while every row stays green.
			if !strings.HasPrefix(err.Error(), shape.wantWrap) {
				t.Fatalf("the error left through %q, want %q — this shape no longer drives the wrap under witness", err.Error(), shape.wantWrap)
			}
			var idle *StreamIdleError
			if !errors.As(err, &idle) {
				t.Fatalf("the stall is not typed through the backend's own wrap: %T %v", err, err)
			}
			// The typed budget is not only readable, it is SPENT: the backend's own
			// loop took the transient budget on the wrapped verdict — the half
			// budget the ticket counts is what a flattened wrap returns to.
			if mock.calls != DefaultMaxAttemptsTransient {
				t.Errorf("the backend's loop took %d attempts, want the transient %d the typed verdict earns", mock.calls, DefaultMaxAttemptsTransient)
			}
			if idle.Phase != StreamIdleWire {
				t.Errorf("phase = %q, want %q (the wire measured the silence, not us)", idle.Phase, StreamIdleWire)
			}
			if idle.Idle != 5*time.Minute {
				t.Errorf("the typed window is %v, want the 5m0s the report names", idle.Idle)
			}
			if !strings.Contains(err.Error(), "no data for 5m0s") {
				t.Errorf("the backend dropped the wire's own stall report: %v", err)
			}
			if !isRetryable(err) {
				t.Errorf("the wrapped stall lost its retryable verdict: %v", err)
			}
			if got := policy.effectiveMaxAttempts(err); got != DefaultMaxAttemptsTransient {
				t.Errorf("inner budget = %d attempts, want the transient %d", got, DefaultMaxAttemptsTransient)
			}
			if !isDelegateRetryable(err) {
				t.Errorf("the outer delegate loop does not retry the stall: %v", err)
			}
			if got := delegate.ClassifyFallback(err, isDelegateRetryable(err)); got != delegate.FallbackTransientExhausted {
				t.Errorf("fallback category = %q, want %q (iterion's own stall's category)", got, delegate.FallbackTransientExhausted)
			}
		})
	}
}

// The verdict keys on the WATCHDOG'S REPORT, not on the stall phrase alone
// and not on the wires that carry a watchdog today — openaiwire.StreamEvents
// serves the OpenAI chat and foundry readers and the Responses reader wires
// its own: the phrase plus
// either the window the watchdog measured or the knob it names. A wire claw
// instruments next is priced the same with no iterion-side change, while a
// message carrying the phrase and nothing else stays the retryable
// transport failure it has always been — on the standard budget, which the
// outer delegate loop does not retry.
//
// One row per marker, isolated: a message with both markers would leave
// either arm of the predicate free to disappear unnoticed.
func TestClawStreamStall_KeysOnTheWatchdogsOwnReport(t *testing.T) {
	for _, c := range []struct {
		name   string
		msg    string
		typed  bool
		window time.Duration
	}{
		{"a wire claw instruments next, reporting in full", "anthropic stream stalled: no data for 1m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true, time.Minute},
		{"the measured window alone", "anthropic stream stalled: no data for 1m0s", true, time.Minute},
		{"the knob alone", "anthropic stream stalled — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true, 0},
		{"the phrase alone, which any provider can relay", "the upstream stream stalled while validating", false, 0},
		// Refusals quoting a marker's WORDS outside the watchdog's shape:
		// the words arrive unanchored, so neither arm may take them and
		// each keeps the phrase-alone verdict above.
		{"a refusal quoting the window's words unanchored", "request rejected: the stream stalled and we have no data for the requested range", false, 0},
		{"a refusal quoting the knob's words unanchored", "refused: your stream stalled, tune via the dashboard", false, 0},
		// Prose that shrinks by two bytes under ToLower (İ, two bytes, maps
		// to i) ahead of the window: an index found in the lowered text and
		// used on the original lands inside "for" and reads "r" instead of
		// the 15m the wire named.
		{"a report whose prose does not survive lowercasing byte for byte", "İİstanbul-edge stream stalled: no data for 15m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true, 15 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := classifyStreamEventError(c.msg)
			var idle *StreamIdleError
			switch typed := errors.As(err, &idle); {
			case typed != c.typed:
				t.Fatalf("typed as the idle verdict = %v, want %v: %T %v", typed, c.typed, err, err)
			case !c.typed:
				// The pre-existing transport verdict, unchanged: retried
				// in place on the standard budget, not by the outer loop.
				if !isRetryable(err) || isDelegateRetryable(err) {
					t.Errorf("the transport verdict moved: isRetryable=%v isDelegateRetryable=%v, want true/false (%v)", isRetryable(err), isDelegateRetryable(err), err)
				}
				if got := (RetryPolicy{}).effectiveMaxAttempts(err); got != DefaultMaxAttempts {
					t.Errorf("budget = %d attempts, want the standard %d (%v)", got, DefaultMaxAttempts, err)
				}
				return
			}
			if idle.Idle != c.window {
				t.Errorf("the typed window is %v, want %v", idle.Idle, c.window)
			}
			if !isRetryable(err) || !isDelegateRetryable(err) {
				t.Errorf("a stall on another wire is not retried by both loops: %v", err)
			}
			if got := (RetryPolicy{}).effectiveMaxAttempts(err); got != DefaultMaxAttemptsTransient {
				t.Errorf("inner budget = %d attempts, want the transient %d (%v)", got, DefaultMaxAttemptsTransient, err)
			}
		})
	}
}

// A provider verdict is never re-read as a stall because its prose mentions
// one: the failure keeps the classification its own type and code earn — a
// refusal stays terminal, a transient frame keeps the standard budget rather
// than the watchdog's larger one.
//
// The third row is the one the ORDER of the branches answers for, not the
// second marker: a gateway relaying an upstream stall report inside its own
// typed frame carries the watchdog's full wording, and the provider-verdict
// branch has to read it first for the frame's own type to decide.
func TestClawStreamStall_DoesNotOverMatchAProviderVerdict(t *testing.T) {
	for _, f := range []struct {
		name      string
		frame     string
		retryable bool
	}{
		{"refusal naming a stall", `{"error":{"message":"the upstream stream stalled while validating","type":"invalid_request_error","param":null,"code":null}}`, false},
		{"transient frame naming a stall", `{"error":{"message":"stream stalled upstream","type":"server_error","param":null,"code":null}}`, true},
		{"refusal quoting a whole watchdog report", `{"error":{"message":"rejected upstream: openai stream stalled: no data for 5m0s - aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)","type":"invalid_request_error","param":null,"code":null}}`, false},
	} {
		t.Run(f.name, func(t *testing.T) {
			err := streamEventErrorOf(t, "gpt-4o", func(c net.Conn) {
				_, _ = io.WriteString(c, sseHeadClose+sseContent+"data: "+f.frame+"\n\n")
			})
			if err == nil {
				t.Fatal("the stream ended without an error")
			}
			// Without the phrase in the message the guard below proves
			// nothing about over-matching.
			if !strings.Contains(err.Error(), "stream stalled") {
				t.Fatalf("the frame's prose no longer reaches the classifier: %v", err)
			}
			if errors.As(err, new(*StreamIdleError)) {
				t.Fatalf("a provider verdict was re-read as a stall: %v", err)
			}
			if got := isRetryable(err); got != f.retryable {
				t.Errorf("isRetryable = %v, want %v (err %v)", got, f.retryable, err)
			}
			if got := (RetryPolicy{}).effectiveMaxAttempts(err); got != DefaultMaxAttempts {
				t.Errorf("budget = %d attempts, want the standard %d", got, DefaultMaxAttempts)
			}
		})
	}
}

// The wire above keeps the provider's type and code, so branch ORDER is
// what protects its verdict. On claw's generic SSE client nothing does:
// the provider's `error.message` arrives verbatim with the type and code
// stripped, so the stall predicate reads it first. There the second marker
// is the whole guard — and it must not refuse a real watchdog report that
// same client would relay, which is the other row.
func TestClawStreamStall_OnTheGenericWireTheSecondMarkerIsTheGuard(t *testing.T) {
	for _, c := range []struct {
		name  string
		msg   string
		typed bool
	}{
		{"a refusal quoting the phrase from a log", "request refused: your prompt quotes 'stream stalled' from a log", false},
		{"a watchdog report this client relays", "openai stream stalled: no data for 5m0s — aborting (retryable; tune via CLAW_STREAM_IDLE_TIMEOUT)", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			frame, err := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": c.msg}})
			if err != nil {
				t.Fatalf("frame: %v", err)
			}
			got := genericWireErrorOf(t, "event: error\ndata: "+string(frame)+"\n\n")
			if got == nil {
				t.Fatal("the error frame ended the stream without an error")
			}
			// Without the phrase reaching the classifier the guards below
			// prove nothing: the client must have forwarded the message.
			if !strings.Contains(got.Error(), "stream stalled") {
				t.Fatalf("the frame's message no longer reaches the classifier: %v", got)
			}
			if typed := errors.As(got, new(*StreamIdleError)); typed != c.typed {
				t.Fatalf("typed as the idle verdict = %v, want %v: %T %v", typed, c.typed, got, got)
			}
			if c.typed {
				if !isDelegateRetryable(got) {
					t.Errorf("the outer delegate loop does not retry a relayed watchdog report: %v", got)
				}
				if n := (RetryPolicy{}).effectiveMaxAttempts(got); n != DefaultMaxAttemptsTransient {
					t.Errorf("inner budget = %d attempts, want the transient %d", n, DefaultMaxAttemptsTransient)
				}
				return
			}
			// A refusal keeps the verdict it had before the stall was
			// typed: retried in place on the standard budget, and the
			// outer loop — which spends a second credential — does not
			// take it at all.
			if !isRetryable(got) {
				t.Errorf("the relayed refusal lost its transport verdict: %v", got)
			}
			if n := (RetryPolicy{}).effectiveMaxAttempts(got); n != DefaultMaxAttempts {
				t.Errorf("budget = %d attempts, want the standard %d", n, DefaultMaxAttempts)
			}
			if isDelegateRetryable(got) {
				t.Errorf("the outer delegate loop re-issues a terminal refusal: %v", got)
			}
			if cat := delegate.ClassifyFallback(got, isDelegateRetryable(got)); cat != delegate.FallbackUnclassified {
				t.Errorf("fallback category = %q, want %q", cat, delegate.FallbackUnclassified)
			}
		})
	}
}
