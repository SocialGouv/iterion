package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// gatewayServer answers every request with a minimal successful chat
// completions stream and records what it received.
type gatewayServer struct {
	srv     *httptest.Server
	path    atomic.Value // string
	headers atomic.Value // http.Header
	body    atomic.Value // map[string]any
}

func newGatewayServer(t *testing.T) *gatewayServer {
	t.Helper()
	g := &gatewayServer{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.path.Store(r.URL.Path)
		g.headers.Store(r.Header.Clone())
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		g.body.Store(body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gatewayServer) observedPath() string {
	p, _ := g.path.Load().(string)
	return p
}

func (g *gatewayServer) observedHeaders() http.Header {
	h, _ := g.headers.Load().(http.Header)
	return h
}

func drain(t *testing.T, c api.APIClient, req api.CreateMessageRequest) {
	t.Helper()
	ch, err := c.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	for ev := range ch {
		if ev.Type == api.EventError {
			t.Fatalf("stream error: %s", ev.ErrorMessage)
		}
	}
}

func effortWithTools(model string) api.CreateMessageRequest {
	return api.CreateMessageRequest{
		Model:           model,
		ReasoningEffort: "high",
		Tools:           []api.Tool{{Name: "read_file", InputSchema: api.InputSchema{Type: "object"}}},
		Messages:        []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
}

// A gateway that serves no /v1/responses is reached on chat completions
// even for the request shape the automatic dispatch sends to Responses.
func TestNewClient_WireChatForcesChatCompletions(t *testing.T) {
	g := newGatewayServer(t)
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL, Model: "scaleway/gpt-oss-120b",
		OpenAIWireAPI: api.OpenAIWireChat, HTTPClient: g.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, effortWithTools("scaleway/gpt-oss-120b"))
	if got := g.observedPath(); got != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", got)
	}
}

// Without an explicit wire choice, dispatch stays what it was on every host:
// effort + tools still goes to Responses on a custom base URL.
func TestNewClient_DefaultWireKeepsDispatchOnCustomHost(t *testing.T) {
	g := newGatewayServer(t)
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL, Model: "gpt-5.5", HTTPClient: g.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, effortWithTools("gpt-5.5"))
	if got := g.observedPath(); got != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses (zero value must keep today's dispatch)", got)
	}
}

func TestNewClient_WireAPIValidation(t *testing.T) {
	if _, err := New().NewClient(api.ProviderConfig{
		OAuthToken: "t", OpenAIChatGPTAccountID: "a", OpenAIWireAPI: api.OpenAIWireChat,
	}); err == nil {
		t.Error("chat wire with the ChatGPT forfait must be refused: its backend has no chat endpoint")
	}
	if _, err := New().NewClient(api.ProviderConfig{APIKey: "k", OpenAIWireAPI: "completions"}); err == nil {
		t.Error("an unknown wire API must be refused, not ignored")
	}
	for _, wire := range []string{"", api.OpenAIWireChat, api.OpenAIWireResponses} {
		if _, err := New().NewClient(api.ProviderConfig{APIKey: "k", OpenAIWireAPI: wire}); err != nil {
			t.Errorf("wire %q refused: %v", wire, err)
		}
	}
}

// The configured HTTP client carries the requests: it is the embedder's
// egress policy (dial guard, no redirects), so bypassing it is a hole.
func TestNewClient_HTTPClientCarriesRequests(t *testing.T) {
	g := newGatewayServer(t)
	var roundTrips atomic.Int32
	base := g.srv.Client().Transport
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		roundTrips.Add(1)
		return base.RoundTrip(r)
	})}
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL, OpenAIWireAPI: api.OpenAIWireChat, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, api.CreateMessageRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if roundTrips.Load() != 1 {
		t.Fatalf("configured HTTP client carried %d requests, want 1", roundTrips.Load())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The operator's ambient identity headers stay home when the endpoint is not
// the operator's; without the flag they still apply (control).
func TestNewClient_NoAmbientHeaders(t *testing.T) {
	t.Setenv(api.EnvCustomHeaders, "X-Operator-Secret: s3cr3t")
	t.Setenv(api.EnvUserAgent, "operator-agent/1")
	for _, noAmbient := range []bool{true, false} {
		t.Run(fmt.Sprintf("noAmbient=%t", noAmbient), func(t *testing.T) {
			g := newGatewayServer(t)
			c, err := New().NewClient(api.ProviderConfig{
				APIKey: "k", BaseURL: g.srv.URL, OpenAIWireAPI: api.OpenAIWireChat,
				HTTPClient: g.srv.Client(), NoAmbientHeaders: noAmbient,
				ExtraHeaders: map[string]string{"X-Explicit": "yes"},
			})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, c, api.CreateMessageRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
			h := g.observedHeaders()
			if h.Get("X-Explicit") != "yes" {
				t.Errorf("explicit header lost: %q", h.Get("X-Explicit"))
			}
			leaked := h.Get("X-Operator-Secret") != "" || h.Get("User-Agent") == "operator-agent/1"
			if noAmbient && leaked {
				t.Errorf("ambient identity reached the endpoint: secret=%q ua=%q", h.Get("X-Operator-Secret"), h.Get("User-Agent"))
			}
			if !noAmbient && !leaked {
				t.Errorf("control: ambient identity should still apply without the flag (secret=%q ua=%q)", h.Get("X-Operator-Secret"), h.Get("User-Agent"))
			}
		})
	}
}

func TestNewClient_TrailingSlashBaseURL(t *testing.T) {
	g := newGatewayServer(t)
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL + "/", OpenAIWireAPI: api.OpenAIWireChat, HTTPClient: g.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, api.CreateMessageRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if got := g.observedPath(); got != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", got)
	}
}

func marshalBuilt(t *testing.T, c *Client, req api.CreateMessageRequest) map[string]any {
	t.Helper()
	built, err := c.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildRequest_ModelVerbatim(t *testing.T) {
	cases := []struct {
		model, verbatim, stripped string
	}{
		{"scaleway/deepseek-v4-flash-0731", "scaleway/deepseek-v4-flash-0731", "scaleway/deepseek-v4-flash-0731"},
		{"openai/gpt-4o", "openai/gpt-4o", "gpt-4o"},
		{"qwen/qwen3-coder", "qwen/qwen3-coder", "qwen3-coder"},
		{"claude-sonnet-gateway-alias", "claude-sonnet-gateway-alias", DefaultOpenAIModel},
	}
	for _, tc := range cases {
		for _, verbatim := range []bool{true, false} {
			c := &Client{Model: DefaultOpenAIModel, BaseURL: "https://gateway.example", ModelVerbatim: verbatim}
			got := marshalBuilt(t, c, api.CreateMessageRequest{Model: tc.model})["model"]
			want := tc.stripped
			if verbatim {
				want = tc.verbatim
			}
			if got != want {
				t.Errorf("model %q verbatim=%t: wire model = %v, want %q", tc.model, verbatim, got, want)
			}
		}
	}
	// Capability checks still read the bare name.
	c := &Client{Model: DefaultOpenAIModel, BaseURL: "https://gateway.example", ModelVerbatim: true}
	m := marshalBuilt(t, c, api.CreateMessageRequest{Model: "openai/gpt-5.5", MaxTokens: 100})
	if _, ok := m["max_completion_tokens"]; !ok {
		t.Errorf("gpt-5 behind a prefix must still use max_completion_tokens: %v", m)
	}
}

func TestBuildRequest_StreamUsageOptIn(t *testing.T) {
	for _, optIn := range []bool{true, false} {
		c := &Client{Model: "m", BaseURL: "https://gateway.example", StreamUsage: optIn}
		opts, has := marshalBuilt(t, c, api.CreateMessageRequest{Model: "m"})["stream_options"].(map[string]any)
		if optIn && (!has || opts["include_usage"] != true) {
			t.Errorf("opted in: stream_options = %v, want include_usage=true", opts)
		}
		if !optIn && has {
			t.Errorf("not opted in on a custom host: stream_options must stay absent, got %v", opts)
		}
	}
}

func TestBuildRequest_ToolChoice(t *testing.T) {
	tools := []api.Tool{{Name: "structured_output", InputSchema: api.InputSchema{Type: "object"}}}
	cases := []struct {
		choice *api.ToolChoice
		want   any
	}{
		{&api.ToolChoice{Type: "auto"}, "auto"},
		{&api.ToolChoice{Type: "any"}, "required"},
		{&api.ToolChoice{Type: "none"}, "none"},
		{&api.ToolChoice{Type: "tool", Name: "structured_output"}, map[string]any{"type": "function", "function": map[string]any{"name": "structured_output"}}},
	}
	c := &Client{Model: "m", BaseURL: "https://gateway.example", WireAPI: api.OpenAIWireChat}
	for _, tc := range cases {
		got := marshalBuilt(t, c, api.CreateMessageRequest{Model: "m", Tools: tools, ToolChoice: tc.choice})["tool_choice"]
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(tc.want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("tool_choice %q: got %s, want %s", tc.choice.Type, gotJSON, wantJSON)
		}
	}
	if _, ok := marshalBuilt(t, c, api.CreateMessageRequest{Model: "m", ToolChoice: &api.ToolChoice{Type: "any"}})["tool_choice"]; ok {
		t.Error("tool_choice without tools must be left out")
	}
}

// An error body is read up to a bound, whatever the endpoint sends.
func TestReadErrorBody_Bounded(t *testing.T) {
	huge := io.LimitReader(zeroReader{}, 4*maxErrorBodyBytes)
	if got := len(readErrorBody(huge)); got != maxErrorBodyBytes {
		t.Fatalf("read %d bytes of an oversized error body, want the bound %d", got, maxErrorBodyBytes)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestNewClient_RefusesHostlessBaseURL(t *testing.T) {
	for _, base := range []string{"/", "//", "///"} {
		if _, err := New().NewClient(api.ProviderConfig{APIKey: "k", BaseURL: base}); err == nil {
			t.Errorf("BaseURL %q must be refused: trimmed empty, it would fall back to api.openai.com", base)
		}
	}
}

// Under NoAmbientHeaders the operator's ChatGPT forfait is never sent to a
// caller-chosen URL; the forfait's own backend, and the flag-less case, stay.
func TestNewClient_ForfaitNotSentToCallerURLUnderNoAmbientHeaders(t *testing.T) {
	forfait := api.ProviderConfig{OAuthToken: "t", OpenAIChatGPTAccountID: "a"}
	refused := forfait
	refused.NoAmbientHeaders, refused.BaseURL = true, "https://tenant-gateway.example"
	if _, err := New().NewClient(refused); err == nil {
		t.Error("forfait credentials with NoAmbientHeaders and a caller-chosen BaseURL must be refused")
	}
	ownBackend := forfait
	ownBackend.NoAmbientHeaders = true
	if _, err := New().NewClient(ownBackend); err != nil {
		t.Errorf("forfait on its own backend under NoAmbientHeaders refused: %v", err)
	}
	legacy := forfait
	legacy.BaseURL = "https://proxy.example"
	if _, err := New().NewClient(legacy); err != nil {
		t.Errorf("forfait with a BaseURL and no NoAmbientHeaders changed behaviour: %v", err)
	}
}

func TestNewClient_VerbatimNeedsAModel(t *testing.T) {
	if _, err := New().NewClient(api.ProviderConfig{APIKey: "k", OpenAIModelVerbatim: true}); err == nil {
		t.Error("OpenAIModelVerbatim with no model must be refused: the default is not a gateway id")
	}
}

func TestBuildRequest_GPT6OnChatUsesMaxCompletionTokens(t *testing.T) {
	c := &Client{Model: "m", BaseURL: "https://gateway.example", WireAPI: api.OpenAIWireChat}
	for _, model := range []string{"gpt-6-astra", "openai/gpt-6-sol", "o3", "o1-mini", "o4-mini"} {
		m := marshalBuilt(t, c, api.CreateMessageRequest{Model: model, MaxTokens: 1000})
		if _, ok := m["max_completion_tokens"]; !ok {
			t.Errorf("%s on chat: want max_completion_tokens, got %v", model, m)
		}
	}
}

// The verbatim rule holds on the dispatch path too, not only in the builder.
func TestStreamResponse_VerbatimClaudeAliasReachesTheWire(t *testing.T) {
	g := newGatewayServer(t)
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL, Model: "fallback-model", OpenAIWireAPI: api.OpenAIWireChat,
		OpenAIModelVerbatim: true, HTTPClient: g.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, api.CreateMessageRequest{Model: "claude-sonnet-gateway-alias", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	body, _ := g.body.Load().(map[string]any)
	if body["model"] != "claude-sonnet-gateway-alias" {
		t.Fatalf("wire model = %v, want the verbatim alias", body["model"])
	}
}

// The configured client carries the Responses path and image generation too.
func TestNewClient_HTTPClientCarriesResponsesAndImages(t *testing.T) {
	g := newGatewayServer(t)
	var paths []string
	base := g.srv.Client().Transport
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return base.RoundTrip(r)
	})}
	c, err := New().NewClient(api.ProviderConfig{
		APIKey: "k", BaseURL: g.srv.URL, OpenAIWireAPI: api.OpenAIWireResponses, HTTPClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, c, api.CreateMessageRequest{Model: "m", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	_, _ = c.(*Client).GenerateImage(context.Background(), "a cat")
	want := []string{"/v1/responses", "/v1/images/generations"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("configured client carried %v, want %v", paths, want)
	}
}

type countingBody struct {
	remaining int
	read      *int
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > b.remaining {
		n = b.remaining
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.remaining -= n
	*b.read += n
	return n, nil
}

func (b *countingBody) Close() error { return nil }

// Both streaming call sites bound what an error response makes them read.
func TestStreamResponse_ErrorBodyBoundedAtCallSites(t *testing.T) {
	for _, wire := range []string{api.OpenAIWireChat, api.OpenAIWireResponses} {
		t.Run(wire, func(t *testing.T) {
			read := 0
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     http.Header{"Content-Type": []string{"text/plain"}},
					Body:       &countingBody{remaining: 16 * maxErrorBodyBytes, read: &read},
					Request:    r,
				}, nil
			})}
			c, err := New().NewClient(api.ProviderConfig{
				APIKey: "k", BaseURL: "https://gateway.example", OpenAIWireAPI: wire, HTTPClient: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{Model: "m"}); err == nil {
				t.Fatal("a 500 must surface as an error")
			}
			if read > maxErrorBodyBytes {
				t.Fatalf("%s call site read %d bytes of an error body, bound is %d", wire, read, maxErrorBodyBytes)
			}
		})
	}
}

func TestBuildRequest_ToolChoiceAbsentWhenNilOrUnknown(t *testing.T) {
	tools := []api.Tool{{Name: "t", InputSchema: api.InputSchema{Type: "object"}}}
	c := &Client{Model: "m", BaseURL: "https://gateway.example", WireAPI: api.OpenAIWireChat}
	for _, choice := range []*api.ToolChoice{nil, {Type: "bogus"}} {
		if v, ok := marshalBuilt(t, c, api.CreateMessageRequest{Model: "m", Tools: tools, ToolChoice: choice})["tool_choice"]; ok {
			t.Errorf("tool_choice %+v must be left out, got %v", choice, v)
		}
	}
}

func TestConvertToolChoiceToResponses_None(t *testing.T) {
	if got := convertToolChoiceToResponses(&api.ToolChoice{Type: "none"}); got != "none" {
		t.Fatalf("none = %v, want \"none\"", got)
	}
}

// tool_choice reaches api.openai.com and callers that chose the chat wire;
// any other host keeps the field out, as before the chat path honoured it
// (DashScope's Qwen rejects "required" and a named choice).
func TestBuildRequest_ToolChoiceOnlyWhereItIsHonoured(t *testing.T) {
	tools := []api.Tool{{Name: "t", InputSchema: api.InputSchema{Type: "object"}}}
	req := api.CreateMessageRequest{Model: "m", Tools: tools, ToolChoice: &api.ToolChoice{Type: "any"}}
	cases := []struct {
		name   string
		client *Client
		want   bool
	}{
		{"api.openai.com", &Client{Model: "m", BaseURL: defaultBaseURL}, true},
		{"explicit chat wire", &Client{Model: "m", BaseURL: "https://gateway.example", WireAPI: api.OpenAIWireChat}, true},
		{"other host, default wire", &Client{Model: "m", BaseURL: "https://dashscope.example"}, false},
	}
	for _, tc := range cases {
		_, got := marshalBuilt(t, tc.client, req)["tool_choice"]
		if got != tc.want {
			t.Errorf("%s: tool_choice present = %t, want %t", tc.name, got, tc.want)
		}
	}
}
