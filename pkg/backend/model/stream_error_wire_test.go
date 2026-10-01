package model

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	openaiprovider "github.com/SocialGouv/claw-code-go/pkg/api/providers/openai"
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
// aggregator classifies the stream's end as.
func streamErrorOf(t *testing.T, write func(c net.Conn)) error {
	t.Helper()
	client, err := openaiprovider.New().NewClient(api.ProviderConfig{APIKey: "test-key", Model: "gpt-4o", BaseURL: serveRawSSE(t, write)})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{
		Model:     "gpt-4o",
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
// condition, and on the chat wire a connection cut in the middle of a data
// line or left silent is retried like any truncated stream.
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
