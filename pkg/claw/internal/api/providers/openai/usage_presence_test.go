package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/httputil"
)

// responsesEvents serves frames as a Responses API stream to a GPT-6 client
// (which dispatches to /v1/responses) and returns what the client emits.
func responsesEvents(t *testing.T, frames []string) []api.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "wrong path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
	}))
	t.Cleanup(srv.Close)
	client := &Client{APIKey: "test-key", BaseURL: srv.URL, Model: "gpt-6-sol", HTTPClient: srv.Client()}
	ch, err := client.StreamResponse(context.Background(), api.CreateMessageRequest{Model: "gpt-6-sol", MaxTokens: 64,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	return events
}

func eventOf(events []api.StreamEvent, typ api.StreamEventType) *api.StreamEvent {
	for i := range events {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

var textFrames = []string{
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`,
}

func withText(frames ...string) []string {
	return append(append([]string(nil), textFrames...), frames...)
}

// A completed response's usage is the provider's account only when its
// usage object names a counter.
func TestStreamResponses_UsagePresence(t *testing.T) {
	cases := map[string]struct {
		completed string
		reported  bool
		input     int
	}{
		"no usage":           {`{"type":"response.completed","response":{"id":"r","status":"completed"}}`, false, 0},
		"with usage":         {`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}}`, true, 10},
		"empty usage object": {`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{}}}`, false, 0},
		"undecodable usage":  {`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":"10"}}}`, false, 0},
		"only output_tokens": {`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"output_tokens":3}}}`, true, 0},
		"only input_tokens":  {`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":4}}}`, true, 4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			events := responsesEvents(t, withText(tc.completed))
			if ev := eventOf(events, api.EventError); ev != nil {
				t.Fatalf("stream failed: %s", ev.ErrorMessage)
			}
			delta := eventOf(events, api.EventMessageDelta)
			if delta == nil {
				t.Fatal("no message_delta")
			}
			if delta.Usage.Reported != tc.reported || delta.Usage.InputTokens != tc.input {
				t.Errorf("usage = %+v, want Reported %v with %d input tokens", delta.Usage, tc.reported, tc.input)
			}
		})
	}
}

// A stream that closes before response.completed was cut, whatever it
// streamed before — a [DONE] some proxies append changes nothing.
func TestStreamResponses_ClosedBeforeCompletionIsTruncated(t *testing.T) {
	for name, frames := range map[string][]string{
		"closed":         withText(),
		"closed on DONE": withText("[DONE]"),
	} {
		t.Run(name, func(t *testing.T) {
			ev := eventOf(responsesEvents(t, frames), api.EventError)
			if ev == nil || !strings.HasPrefix(ev.ErrorMessage, "openai responses stream truncated") {
				t.Fatalf("error = %+v, want a truncated stream", ev)
			}
		})
	}
}

// response.completed is the last event: a connection held open after it
// does not park the reader until the idle watchdog.
func TestStreamResponses_CompletedEndsTheStream(t *testing.T) {
	t.Setenv("CLAW_STREAM_IDLE_TIMEOUT", "30s")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range withText(`{"type":"response.completed","response":{"id":"r","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`) {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := &Client{APIKey: "test-key", BaseURL: srv.URL, Model: "gpt-6-sol", HTTPClient: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{Model: "gpt-6-sol", MaxTokens: 64,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if ctx.Err() != nil {
		t.Fatal("the reader waited past response.completed")
	}
	if eventOf(events, api.EventMessageStop) == nil || eventOf(events, api.EventError) != nil {
		t.Errorf("events = %+v, want a clean completion", events)
	}
}

// An error frame is never dropped for the shape of its fields: a numeric
// code, a bare-string error, or a value that does not decode at all still
// fails the stream, naming what could be read.
func TestStreamResponses_ErrorFramesOfAnyShape(t *testing.T) {
	cases := map[string]string{
		`{"type":"error","code":"server_error","message":"boom","param":null,"sequence_number":3}`:                 "openai stream error: server_error: boom",
		`{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`:                            "openai stream error: rate_limit_exceeded: slow down",
		`{"type":"error","code":429,"message":"slow down"}`:                                                        "openai stream error: 429: slow down",
		`{"type":"error","error":"Too Many Requests"}`:                                                             "openai stream error: Too Many Requests",
		`{"type":"error","code":null,"message":"boom"}`:                                                            "openai stream error: boom",
		`{"type":"response.failed","response":{"id":"r","status":"failed","error":"upstream gone"}}`:               "openai response failed: upstream gone",
		`{"type":"error","error":{"code":503,"message":"unavailable"}}`:                                            "openai stream error: 503: unavailable",
		`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":500,"message":"boom"}}}`: "openai response failed: 500: boom",
		`{"type":"error","error":[1]}`:                                                                             "openai stream error: [1]",
		`{"type":"response.failed","response":{"error":[1]}}`:                                                      `openai response failed: {"type":"response.failed","response":{"error":[1]}}`,
		`{"type":"response.incomplete","response":{"incomplete_details":[1]}}`:                                     "openai response incomplete: unknown",
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`:          "openai response incomplete: max_output_tokens",
	}
	for frame, want := range cases {
		ev := eventOf(responsesEvents(t, withText(frame)), api.EventError)
		if ev == nil || ev.ErrorMessage != want {
			t.Errorf("frame %s: error %+v, want %q", frame, ev, want)
		}
	}
}

// A failure frame that carries usage is the provider's account of the
// failed call; one that carries none leaves the usage unreported.
func TestStreamResponses_AFailureCarriesItsUsage(t *testing.T) {
	cases := map[string]struct {
		frame    string
		reported bool
		output   int
	}{
		"incomplete with usage":           {`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":64,"output_tokens_details":{"reasoning_tokens":60}}}}`, true, 64},
		"failed with usage":               {`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"},"usage":{"input_tokens":9,"output_tokens":3}}}`, true, 3},
		"failed without usage":            {`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`, false, 0},
		"failed, usage naming no counter": {`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"},"usage":{}}}`, false, 0},
		"error event":                     {`{"type":"error","code":"server_error","message":"boom"}`, false, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := eventOf(responsesEvents(t, withText(tc.frame)), api.EventError)
			if ev == nil {
				t.Fatal("no error event")
			}
			if ev.Usage.Reported != tc.reported || ev.Usage.OutputTokens != tc.output {
				t.Errorf("error %q carries %+v, want Reported %v with %d output tokens", ev.ErrorMessage, ev.Usage, tc.reported, tc.output)
			}
			if name == "incomplete with usage" && ev.Usage.OutputTokensDetails.ThinkingTokens != 60 {
				t.Errorf("error %q carries %+v, want its 60 reasoning tokens", ev.ErrorMessage, ev.Usage)
			}
		})
	}
}

// A generic OpenAI-compatible endpoint gets a request shaped without
// reading the model id: an alias spelled like an o-series model keeps
// max_tokens and the temperature it was given. api.openai.com's shaping —
// max_completion_tokens, no tuning parameters for a reasoning model — stays
// the default.
func TestBuildRequest_GenericRequestIgnoresTheModelsSpelling(t *testing.T) {
	temp := 0.2
	for _, generic := range []bool{true, false} {
		c := &Client{Model: "m", BaseURL: "https://gateway.example", ModelVerbatim: true, GenericRequest: generic}
		m := marshalBuilt(t, c, api.CreateMessageRequest{Model: "o3-local", MaxTokens: 100, Temperature: &temp})
		_, maxTokens := m["max_tokens"]
		_, maxCompletion := m["max_completion_tokens"]
		_, temperature := m["temperature"]
		if generic && (!maxTokens || maxCompletion || !temperature) {
			t.Errorf("generic request: %v, want max_tokens and temperature, no max_completion_tokens", m)
		}
		if !generic && (maxTokens || !maxCompletion || temperature) {
			t.Errorf("default request for an o-series id: %v, want max_completion_tokens and no temperature", m)
		}
	}
	// The option reaches a client built through the constructor.
	built, err := New().NewClient(api.ProviderConfig{APIKey: "k", Model: "o3-local", BaseURL: "https://gateway.example", OpenAIModelVerbatim: true, OpenAIGenericRequest: true, OpenAIWireAPI: api.OpenAIWireChat})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The automatic dispatch picks the endpoint from the model id, so the
	// option is refused off the chat wire.
	for _, wire := range []string{"", api.OpenAIWireResponses} {
		if _, err := New().NewClient(api.ProviderConfig{APIKey: "k", Model: "o3-local", BaseURL: "https://gateway.example", OpenAIGenericRequest: true, OpenAIWireAPI: wire}); err == nil || !strings.Contains(err.Error(), "OpenAIGenericRequest needs OpenAIWireAPI") {
			t.Errorf("wire %q: NewClient error = %v, want the chat-wire requirement", wire, err)
		}
	}
	if m := marshalBuilt(t, built.(*Client), api.CreateMessageRequest{Model: "o3-local", MaxTokens: 100}); m["max_tokens"] == nil {
		t.Errorf("a client built with OpenAIGenericRequest sends %v, want max_tokens", m)
	}
	if !strings.Contains(fmt.Sprint(api.RefuseOpenAIOnlyOptions("zai", api.ProviderConfig{OpenAIGenericRequest: true})), "OpenAIGenericRequest") {
		t.Error("OpenAIGenericRequest is not refused by the other providers' option guard")
	}
}

// response.completed ends the stream even when it does not decode: the
// reader stops there, and a connection held open after it does not park it.
func TestStreamResponses_AnUndecodableCompletionEndsTheStream(t *testing.T) {
	t.Setenv("CLAW_STREAM_IDLE_TIMEOUT", "30s")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range withText(`{"type":"response.completed","response":{"usage":{"input_tokens":"3"}}}`) {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	client := &Client{APIKey: "test-key", BaseURL: srv.URL, Model: "gpt-6-sol", HTTPClient: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := client.StreamResponse(ctx, api.CreateMessageRequest{Model: "gpt-6-sol", MaxTokens: 64,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if ctx.Err() != nil {
		t.Fatal("the reader waited past an undecodable response.completed")
	}
	if eventOf(events, api.EventMessageStop) == nil || eventOf(events, api.EventError) != nil {
		t.Errorf("events = %+v, want a clean completion", events)
	}
}

// No field of a failure frame unbounds the error built from it.
func TestStreamResponses_FailureTextIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	for _, frame := range []string{
		`{"type":"error","code":"` + huge + `","message":"m"}`,
		`{"type":"error","code":"c","message":"` + huge + `"}`,
		`{"type":"error","code":{"k":"` + huge + `"},"message":"m"}`,
		`{"type":"response.failed","response":{"error":{"code":"` + huge + `","message":"` + huge + `"}}}`,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"` + huge + `"}}}`,
	} {
		ev := eventOf(responsesEvents(t, withText(frame)), api.EventError)
		if ev == nil || len([]rune(ev.ErrorMessage)) > 1400 {
			t.Errorf("frame of %d bytes: error %d runes, want an error of at most 1400", len(frame), len([]rune(ev.ErrorMessage)))
		}
	}
}

// responsesErrorOf runs the Responses reader over a raw body and returns the
// error it emits, if any.
func responsesErrorOf(body string) string {
	ch := make(chan api.StreamEvent, 64)
	go (&Client{}).streamResponsesEvents(context.Background(), &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, ch)
	var msg string
	for ev := range ch {
		if ev.Type == api.EventError {
			msg = ev.ErrorMessage
		}
	}
	return msg
}

// An error event names its failure from the nested error when that names
// one, from the top level otherwise — a null code is no code, an empty
// nested object no error.
func TestStreamResponses_WhereAnErrorEventNamesItsFailure(t *testing.T) {
	for frame, want := range map[string]string{
		`{"type":"error","code":null,"message":"m","param":null}`:                              "openai stream error: m",
		`{"type":"error","error":{"code":"inner","message":"i"},"code":"outer","message":"o"}`: "openai stream error: inner: i",
		`{"type":"error","error":{},"code":"server_error","message":"boom"}`:                   "openai stream error: server_error: boom",
	} {
		if got := responsesErrorOf("data: " + frame + "\n\n"); got != want {
			t.Errorf("frame %s: error %q, want %q", frame, got, want)
		}
	}
}

// A failure that does not decode is bounded like any other.
func TestStreamResponses_AnUndecodableFailureIsBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	got := responsesErrorOf("data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":5}},\"pad\":\"" + huge + "\"}\n\n")
	if limit := len("openai response failed: ") + httputil.BodyTruncateForLog + 8; got == "" || len([]rune(got)) > limit {
		t.Errorf("error is %d runes, want 1..%d", len([]rune(got)), limit)
	}
}
