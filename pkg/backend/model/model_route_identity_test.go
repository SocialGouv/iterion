package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A claw node's routing prefix comes off exactly once, in buildRequest: a
// model id that holds slashes of its own reaches the provider whole, and the
// node's llm_request names both the route and the id the request carried.
// Red when claw_backend strips the prefix before GenerationOptions again —
// the second strip cut "meta-llama/Llama-3.3-70B" to "Llama-3.3-70B".
func TestClawBackend_WireKeepsTheModelIDsOwnSlashes(t *testing.T) {
	cases := []struct{ spec, wire string }{
		{"openai/meta-llama/Llama-3.3-70B", "meta-llama/Llama-3.3-70B"},
		{"moonshot/kimi-code/kimi-for-coding", "kimi-code/kimi-for-coding"},
		{"anthropic/claude-opus-5-5", "claude-opus-5-5"},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			reg := NewRegistry()
			mock := &captureRequestClient{stream: mockStreamEvents("ok", "end_turn")}
			for _, p := range []string{"openai", "moonshot", "anthropic"} {
				reg.Register(p, func(string) (api.APIClient, error) { return mock, nil })
			}
			var mu sync.Mutex
			var reqs []LLMRequestInfo
			hooks := EventHooks{OnLLMRequest: func(_ string, info LLMRequestInfo) {
				mu.Lock()
				defer mu.Unlock()
				reqs = append(reqs, info)
			}}
			backend := NewClawBackend(reg, hooks, RetryPolicy{})
			if _, err := backend.Execute(context.Background(), delegate.Task{NodeID: "n", Model: tc.spec, UserPrompt: "ping"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := mock.last().Model; got != tc.wire {
				t.Errorf("wire model = %q, want %q", got, tc.wire)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reqs) == 0 {
				t.Fatal("no llm_request fired")
			}
			for _, r := range reqs {
				if r.Model != tc.spec || r.WireModel != tc.wire {
					t.Errorf("llm_request model %q wire_model %q, want the route %q and the wire id %q", r.Model, r.WireModel, tc.spec, tc.wire)
				}
			}
		})
	}
}

// The prefixes modelroute calls routing providers are exactly the claw
// registry's factories: a provider added to one and not the other would make
// the runner's meters read its spec as a model id, or a model id as a route.
func TestRoutingProvidersArePinnedToTheRegistry(t *testing.T) {
	reg := NewRegistry()
	var got []string
	for p := range reg.providers {
		got = append(got, p)
	}
	want := modelroute.RoutingProviders()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry factories %v, modelroute.RoutingProviders() %v — keep them the same set", got, want)
	}
}

// Threshold compaction asks claw about the route's capability id: a full
// vendor spec compacts like its bare id, and a gateway alias never borrows a
// vendor's window from its spelling. Red when compactionConfig hands claw
// the spec itself (claw knows no "anthropic/…" id: the unknown-model
// threshold), or when the gateway's capability id is its wire id.
func TestCompactionReadsTheCapabilityIDNotTheSpec(t *testing.T) {
	unknown := clawrt.DefaultCompactionConfig().MaxEstimatedTokens
	bare := compactionConfig("claude-opus-4-8", 0, 0).MaxEstimatedTokens
	if bare <= unknown {
		t.Fatalf("fixture: claw no longer knows claude-opus-4-8's window (threshold %d)", bare)
	}
	if got := compactionConfig("anthropic/claude-opus-4-8", 0, 0).MaxEstimatedTokens; got != bare {
		t.Errorf("anthropic/claude-opus-4-8 compacts at %d, want %d — its bare id's window", got, bare)
	}
	if got := compactionConfig("openai_compatible/claude-opus-4-8", 0, 0).MaxEstimatedTokens; got != unknown {
		t.Errorf("gateway alias openai_compatible/claude-opus-4-8 compacts at %d, want the unknown-model threshold %d", got, unknown)
	}
}

// Adaptive thinking is a vendor profile, read on the capability id. Red when
// the spec itself is looked up (no profile knows "anthropic/…"), or when a
// gateway alias gets the profile of the vendor model it is spelled like.
func TestAdaptiveThinkingReadsTheCapabilityID(t *testing.T) {
	if !requiresAdaptiveThinking("claude-opus-5-5") {
		t.Fatal("fixture: claude-opus-5-5 no longer requires adaptive thinking")
	}
	if !requiresAdaptiveThinking("anthropic/claude-opus-5-5") {
		t.Error("anthropic/claude-opus-5-5 lost its adaptive-thinking profile")
	}
	if !requiresAdaptiveThinking("openai/anthropic/claude-opus-5-5") {
		t.Error("claude-opus-5-5 behind an OpenAI-compatible host lost its adaptive-thinking profile")
	}
	if requiresAdaptiveThinking("openai_compatible/claude-opus-5-5") {
		t.Error("a gateway alias spelled like claude-opus-5-5 got its vendor profile")
	}
	if requiresAdaptiveThinking("openai_compatible/anthropic/claude-opus-5-5") {
		t.Error("a nested gateway alias got a vendor profile through its last segment")
	}
}

// Every client of claw's OpenAI provider sends the request's model verbatim:
// iterion stripped the routing prefix once, and claw must strip nothing more
// — "qwen/" is a routing prefix claw would otherwise cut. Red when an env or
// keyed factory drops OpenAIModelVerbatim; the ChatGPT forfait, which serves
// only chatgpt.com, is pinned on its configuration below.
func TestOpenAIProviderClientsSendTheWireIDVerbatim(t *testing.T) {
	if !openAIForfaitConfig("gpt-5.4-mini", secrets.CodexCredentialsView{}).OpenAIModelVerbatim {
		t.Error("the ChatGPT-forfait client does not send the model verbatim")
	}
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("XAI_BASE_URL", srv.URL)
	t.Setenv("XAI_API_KEY", "test-key")

	reg := NewRegistry()
	cases := []struct {
		name   string
		client func() (api.APIClient, error)
		spec   string
	}{
		{"env openai", func() (api.APIClient, error) { return reg.Resolve("openai/qwen/qwen-max") }, "openai/qwen/qwen-max"},
		{"keyed openai", func() (api.APIClient, error) { return reg.providersWithKey["openai"]("qwen/qwen-max", "k") }, "openai/qwen/qwen-max"},
		{"env xai", func() (api.APIClient, error) { return reg.Resolve("xai/qwen/qwen-max") }, "xai/qwen/qwen-max"},
		{"keyed xai", func() (api.APIClient, error) { return reg.providersWithKey["xai"]("qwen/qwen-max", "k") }, "xai/qwen/qwen-max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, err := tc.client()
			if err != nil {
				t.Fatalf("client: %v", err)
			}
			mu.Lock()
			seen = nil
			mu.Unlock()
			_, err = GenerateTextDirect(context.Background(), client, GenerationOptions{
				Model:    tc.spec,
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "ping"}}}},
			})
			if err != nil {
				t.Fatalf("GenerateTextDirect: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 || seen[0] != "qwen/qwen-max" {
				t.Errorf("wire models %v, want exactly [qwen/qwen-max]", seen)
			}
		})
	}
}

// A gateway id is opaque: two gateway models sharing a last segment are two
// models, so a backend reporting the other one is a drift; the declared
// gateway route reported by its wire id is not. Red when the comparison falls
// back to snapshot-alias equality on the last segment.
func TestModelDriftComparesGatewayRoutesExactly(t *testing.T) {
	cases := []struct {
		name, declared, effective string
		drift                     bool
	}{
		{"other gateway model, same last segment", "openai_compatible/team-a/m", "openai_compatible/team-b/m", true},
		{"gateway route reported by its wire id", "openai_compatible/team-a/m", "team-a/m", false},
		{"other wire id, same last segment", "openai_compatible/team-a/m", "team-b/m", true},
		{"vendor snapshot alias", "openai/gpt-5.5", "gpt-5.5-2026", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store.New: %v", err)
			}
			if _, err := st.CreateRun(ctx, "run-drift", "wf", nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			hooks := NewStoreEventHooks(ctx, st, "run-drift", iterlog.New(iterlog.LevelError, nil), nil)
			hooks.OnDelegateFinished("n", DelegateInfo{BackendName: "pi", DeclaredModel: tc.declared, EffectiveModel: tc.effective})
			evts, err := st.LoadEvents(ctx, "run-drift")
			if err != nil {
				t.Fatalf("LoadEvents: %v", err)
			}
			got := len(eventsOfType(evts, store.EventModelDrift)) > 0
			if got != tc.drift {
				t.Errorf("model_drift emitted = %v, want %v", got, tc.drift)
			}
		})
	}
}

// The sandbox relay carries the wire id beside the route, so a sandboxed
// node's llm_request reads like an in-process one.
func TestSandboxRelay_CarriesTheWireModel(t *testing.T) {
	var sent []delegate.Envelope
	relay := SandboxRelayHooks(func(env delegate.Envelope) error {
		sent = append(sent, env)
		return nil
	}, func(err error) { t.Errorf("relay error: %v", err) })
	relay.OnLLMRequest("n", LLMRequestInfo{
		Model: "openai/meta-llama/Llama-3.3-70B", WireModel: "meta-llama/Llama-3.3-70B", MessageCount: 1, Timestamp: time.Now(),
	})
	if len(sent) != 1 {
		t.Fatalf("relayed %d envelopes, want 1", len(sent))
	}
	var ev delegate.EventData
	if err := json.Unmarshal(sent[0].Data, &ev); err != nil {
		t.Fatalf("decode relayed envelope: %v", err)
	}
	var got LLMRequestInfo
	h := EventHooks{OnLLMRequest: func(_ string, info LLMRequestInfo) { got = info }}
	if handled, err := ApplyRelayedEvent(h, "n", ev.Type, ev.Payload); err != nil || !handled {
		t.Fatalf("ApplyRelayedEvent: handled=%v err=%v", handled, err)
	}
	if got.Model != "openai/meta-llama/Llama-3.3-70B" || got.WireModel != "meta-llama/Llama-3.3-70B" {
		t.Errorf("relayed llm_request model %q wire_model %q", got.Model, got.WireModel)
	}
}

// The llm_request event a store persists carries the wire id beside the
// route when they differ, and no wire_model for a bare spec. Red when the
// store hooks drop the field.
func TestLLMRequest_PersistedEventCarriesTheWireID(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(ctx, "run-wire", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, "run-wire", iterlog.New(iterlog.LevelError, nil), nil)
	hooks.OnLLMRequest("nested", LLMRequestInfo{Model: "openai/meta-llama/Llama-3.3-70B", WireModel: "meta-llama/Llama-3.3-70B", Timestamp: time.Now()})
	hooks.OnLLMRequest("bare", LLMRequestInfo{Model: "gpt-5.5", Timestamp: time.Now()})
	evts, err := st.LoadEvents(ctx, "run-wire")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range eventsOfType(evts, store.EventLLMRequest) {
		seen[e.NodeID] = true
		wire, has := e.Data["wire_model"]
		switch e.NodeID {
		case "nested":
			if wire != "meta-llama/Llama-3.3-70B" || e.Data["model"] != "openai/meta-llama/Llama-3.3-70B" {
				t.Errorf("nested llm_request = %v", e.Data)
			}
		case "bare":
			if has {
				t.Errorf("a bare spec's llm_request carries wire_model %v", wire)
			}
		}
	}
	if !seen["nested"] || !seen["bare"] {
		t.Fatalf("llm_request events for %v, want both nodes", seen)
	}
}

// Every llm_request a claw node emits names the route and its wire id —
// the nudge re-run and the structured recovery pass included, which the
// backend announces itself before their calls.
func TestClawBackend_NudgeAndRecoveryPassesNameTheWireID(t *testing.T) {
	schema := &ir.Schema{Name: "verdict", Fields: []*ir.SchemaField{{Name: "approved", Type: ir.FieldTypeBool}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	toolDefs := []delegate.ToolDef{{
		Name:        "noop",
		Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	const spec, wire = "test/meta/model", "meta/model"
	run := func(t *testing.T, client api.APIClient) []LLMRequestInfo {
		t.Helper()
		reg := NewRegistry()
		reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
		var mu sync.Mutex
		var infos []LLMRequestInfo
		hooks := EventHooks{OnLLMRequest: func(_ string, info LLMRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			infos = append(infos, info)
		}}
		backend := NewClawBackend(reg, hooks, RetryPolicy{MaxAttempts: 1})
		_, _ = backend.Execute(context.Background(), delegate.Task{
			NodeID: "reviewer", Model: spec, UserPrompt: "Review.",
			OutputSchema: schemaJSON, HasTools: true, ToolMaxSteps: 5, ToolDefs: toolDefs,
		})
		mu.Lock()
		defer mu.Unlock()
		return append([]LLMRequestInfo(nil), infos...)
	}
	check := func(t *testing.T, infos []LLMRequestInfo, wantAtLeast int) {
		t.Helper()
		if len(infos) < wantAtLeast {
			t.Fatalf("%d llm_request, want at least %d", len(infos), wantAtLeast)
		}
		for i, info := range infos {
			if info.Model != spec || info.WireModel != wire {
				t.Errorf("llm_request #%d: model %q wire_model %q, want %q / %q", i, info.Model, info.WireModel, spec, wire)
			}
		}
	}
	t.Run("nudge", func(t *testing.T) {
		// Narration with no tool call, then the verdict: one nudge re-run.
		infos := run(t, &execMockClient{streams: []<-chan api.StreamEvent{
			mockStreamEvents("I will start by reviewing the diff.", "end_turn"),
			mockStreamEvents(`{"approved":true}`, "end_turn"),
		}})
		check(t, infos, 3) // the call, the announced nudge, the nudge call
	})
	t.Run("recovery", func(t *testing.T) {
		// A tool call keeps the nudge away; prose then forces the recovery pass.
		infos := run(t, newMockClient(
			toolUseEvents("tu_1", "noop", `{}`, 100, 30),
			textEvents("I reviewed the diff.", 40, 10),
			textEvents("Still narrating.", 20, 5),
		))
		check(t, infos, 4) // two loop calls, the announced recovery, the recovery call
	})
}

// A human node's reasoning effort is clamped on its model's capability id,
// like an agent's: "none" on Opus 5.5 floors to "low". Red when the human
// executor hands the spec itself to the effort matrix, which knows no
// "anthropic/…" id and lets "none" through.
func TestHumanNode_EffortClampsOnTheCapabilityID(t *testing.T) {
	reg := NewRegistry()
	mock := &captureRequestClient{stream: mockStreamEvents("ok", "end_turn")}
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Schemas: map[string]*ir.Schema{
		"answer_schema": {Name: "answer_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}},
	}}
	exec := NewClawExecutor(reg, wf)
	node := &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "gate"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM},
		Model:             "anthropic/claude-opus-5-5",
		SchemaFields:      ir.SchemaFields{OutputSchema: "answer_schema"},
	}
	_, _ = exec.Execute(context.Background(), node, map[string]any{"_reasoning_effort": "none"})
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.reqs) == 0 {
		t.Fatal("the human node sent no request")
	}
	if got := mock.reqs[0].ReasoningEffort; got != "low" {
		t.Errorf("wire reasoning_effort = %q, want low", got)
	}
}

// The subagent runner sends its model's wire id whole through the real claw
// client. Red when it strips the routing prefix itself again.
func TestSubagentRunner_SendsTheWireIDWhole(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")

	run := NewSubagentRunner(NewRegistry(), tool.NewRegistry(), EventHooks{}, nil, "openai/meta-llama/Llama-3.3-70B")
	if _, err := run(context.Background(), map[string]any{"description": "probe", "prompt": "say done"}); err != nil {
		t.Fatalf("subagent: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the subagent sent no request")
	}
	for _, m := range seen {
		if m != "meta-llama/Llama-3.3-70B" {
			t.Errorf("wire model %q, want meta-llama/Llama-3.3-70B", m)
		}
	}
}

// delegate_finished names the route of the chain element that served — what
// a meter cannot rebuild from the declared model and a backend's report.
// Red when the executor leaves RouteModel unset.
func TestDelegateFinished_NamesTheElementThatServed(t *testing.T) {
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode, fail: &delegate.ErrTransient{Reason: "boom"}, tokens: 10}
	tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 30}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	reg.Register(delegate.BackendClaw, tail)
	var finished []DelegateInfo
	e := newFallbackExecutor(reg, EventHooks{OnDelegateFinished: func(_ string, di DelegateInfo) { finished = append(finished, di) }})
	chain := []chainElement{
		{Label: "primary"},
		{Label: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
	}
	build := e.newElementBuilder("review", delegate.BackendClaudeCode, nil,
		func(_ context.Context, bn string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "review", Model: "anthropic/claude-opus-5"}, nil
		})
	if _, err := e.dispatchWithObservability(context.Background(), "review", delegate.BackendClaudeCode, "model: node", chain, "anthropic/claude-opus-5", build, nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(finished) != 1 {
		t.Fatalf("%d delegate_finished, want 1", len(finished))
	}
	if got := finished[0].RouteModel; got != "openai/gpt-5.5" {
		t.Errorf("RouteModel = %q, want the serving element's openai/gpt-5.5", got)
	}
}
