package model

import (
	"bytes"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/secrets"

	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestStructuredExtractionSettlement(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "scripted-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	for _, tc := range []struct {
		name, mode    string
		calls, tokens int
		ok            bool
	}{{"success", "success", 1, 20, true}, {"reask", "reask", 2, 50, true}, {"error2", "error2", 2, 30, false}, {"error1", "error1", 1, 0, false}, {"stream error", "stream", 1, 15, false}, {"invalid result", "invalid", 1, 20, false}, {"schema refused", "schema", 0, 0, false}, {"cancel before call", "cancel", 0, 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.CreateRun(context.Background(), "run", "wf", nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := NewStoreEventHooks(ctx, st, "run", iterlog.Nop(), nil, nil)
			other := 0
			hooks := ChainHooks(events, EventHooks{OnLLMRequest: func(string, LLMRequestInfo) { other++ }, OnLLMStepFinish: func(string, LLMStepInfo) { other++ }, OnDelegateFinished: func(string, DelegateInfo) { other++ }})
			e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithEventHooks(hooks))
			spec := e.detectorSuggestedModel()
			if spec == "" {
				t.Fatal("missing route")
			}
			client := &funcClient{fn: func(n int, r api.CreateMessageRequest) ([]api.StreamEvent, error) {
				if r.Model != strings.SplitN(spec, "/", 2)[1] {
					t.Errorf("wire route changed: %q", r.Model)
				}
				if tc.mode == "error1" || tc.mode == "error2" && n == 2 {
					return nil, errors.New("secret raw error that must not reach settlement")
				}
				if tc.mode == "stream" {
					cancel()
					return []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 15}, {Type: api.EventError, ErrorMessage: "secret stream error"}}, nil
				}
				if n == 1 && (tc.mode == "reask" || tc.mode == "error2") {
					return textEvents("ordinary prose", 23, 7), nil
				}
				obj := `{"verdict":true,"reason":"ok"}`
				if tc.mode == "invalid" {
					obj = `{"verdict":true}`
				}
				return toolUseEvents("x", "structured_output", obj, 15, 5), nil
			}}
			e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
			s := e.schemas["verdict_schema"]
			wire, _ := SchemaToJSON(s)
			if tc.mode == "schema" {
				wire = []byte(`{`)
			}
			if tc.mode == "cancel" {
				cancel()
			}
			collector := &ExecutionUsageCollector{}
			invoke := WithExecutionUsageCollector(ctx, collector)
			_, spent, ok, _ := e.extractStructuredCorrection(invoke, "judge", &delegate.Task{Model: "test/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished secret prose"}}, delegate.Result{}, s, delegate.BackendClaudeCode)
			// A cancellation racing an already buffered stream may prevent its first
			// token from being read; the event must preserve exactly the returned usage.
			if tc.mode == "stream" && spent.UnreportedCalls != 1 {
				t.Fatalf("cancelled stream usage: %+v", spent)
			}
			if tc.mode != "stream" && (spent.Tokens != tc.tokens || ok != tc.ok) {
				t.Fatalf("spent=%+v ok=%v", spent, ok)
			}
			if len(client.calls) != tc.calls || other != 0 {
				t.Fatalf("calls=%d other hooks=%d", len(client.calls), other)
			}
			usage, uerr := collector.Snapshot()
			if uerr != nil || usage.Tokens != spent.Tokens {
				t.Fatalf("collector %+v/%v vs %+v", usage, uerr, spent)
			}
			got, err := st.LoadEvents(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			wantEvents := 0
			if tc.calls > 0 {
				wantEvents = 1
			}
			if len(got) != wantEvents {
				t.Fatalf("settlements=%d want=%d: %+v", len(got), wantEvents, got)
			}
			if len(got) == 0 {
				return
			}
			ev := got[0]
			data := ev.Data
			if ev.Type != store.EventStructuredExtraction || ev.NodeID != "judge" || data["model"] != spec || data["provider"] != "zai" || data["backend"] != delegate.BackendClaw || data["call_id"] == "" {
				t.Fatalf("identity: %+v", ev)
			}
			if data["outcome"] != map[bool]string{true: "success", false: "error"}[ok] {
				t.Fatalf("outcome: %+v", data)
			}
			b, _ := json.Marshal(data)
			for _, forbidden := range []string{"secret", "finished", "fingerprint", "prompt", "reason"} {
				if strings.Contains(string(b), forbidden) {
					t.Fatalf("payload leaked %q: %s", forbidden, b)
				}
			}
			if int(data["input_tokens"].(float64)+data["output_tokens"].(float64)) != spent.Tokens {
				t.Fatalf("event=%v spent=%+v", data, spent)
			}
			if data["cost_known"] != false {
				t.Fatalf("unknown GLM rates certified: %+v", data)
			}
			run, err := st.LoadRun(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if len(run.NodesServed) != 0 {
				t.Fatalf("extractor replaced serving node: %+v", run.NodesServed)
			}
		})
	}
}

func TestStructuredExtractionClientFence(t *testing.T) {
	c := &funcClient{fn: func(int, api.CreateMessageRequest) ([]api.StreamEvent, error) {
		return nil, errors.New("attempt reached provider")
	}}
	observed := &extractionClient{inner: c}
	if observed.finish() {
		t.Fatal("unstarted client was counted")
	}
	if _, err := observed.StreamResponse(context.Background(), api.CreateMessageRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(c.calls) != 0 {
		t.Fatal("late watchdog goroutine crossed settlement fence")
	}
	started := &extractionClient{inner: c}
	if _, err := started.StreamResponse(context.Background(), api.CreateMessageRequest{}); err == nil {
		t.Fatal("expected refusal")
	}
	if !started.finish() || len(c.calls) != 1 {
		t.Fatal("provider refusal lost its settlement")
	}
}

func TestStructuredExtractionPricePresence(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	known := newStructuredExtraction(t.Context(), "openai/gpt-6-sol", Usage{}, false)
	if !known.CostKnown || known.CostUSD != 0 {
		t.Fatalf("zero usage of known route: %+v", known)
	}
	unknown := newStructuredExtraction(t.Context(), "openai/not-priced", Usage{InputTokens: 10}, true)
	if unknown.CostKnown || unknown.CostUSD != 0 {
		t.Fatalf("unknown route: %+v", unknown)
	}
	if known.CallID == unknown.CallID || known.CallID == "" {
		t.Fatal("call IDs not unique")
	}
}

func TestStructuredExtractionOwnedRoundUsesSameSettlement(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "scripted-test-key")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	const sentinel = "token-that-must-be-redacted-before-extraction"
	guard := secretguard.New([]secretguard.Secret{{Name: "sentinel", Value: sentinel}}, secretguard.DefaultConfig())
	client := newMockClient(textEvents("prose without JSON", 23, 7), toolUseEvents("extract", "structured_output", `{"verdict":true,"reason":"ok"}`, 15, 5))
	calls, finished, settled := 0, 0, 0
	var info StructuredExtractionInfo
	registry := delegate.NewRegistry()
	registry.Register(delegate.BackendClaudeCode, persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"text": "finished " + sentinel}, ParseFallback: true, Tokens: 100, CostUSD: 2}, nil
	}))
	e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithBackendRegistry(registry), WithSecretGuard(guard), WithWorkDir(t.TempDir()), WithEventHooks(EventHooks{OnStructuredExtraction: func(_ string, i StructuredExtractionInfo) { settled++; info = i }, OnNodeFinished: func(_ string, o map[string]any) {
		finished++
		if o["_tokens"] != 250 {
			t.Errorf("node aggregate=%+v", o)
		}
	}}))
	spec := e.detectorSuggestedModel()
	e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
	collector := &ExecutionUsageCollector{}
	ctx := WithExecutionUsageCollector(WithRunID(context.Background(), "owned"), collector)
	ctx = WithOutputCorrectionOwnership(ctx, OutputCorrectionOwnership{Protocol: OutputCorrectionProtocol, Authority: store.RunExecutionAuthority{RunID: "owned", Epoch: "epoch"}})
	node := verdictJudge(delegate.BackendClaudeCode)
	_, err := e.Execute(ctx, node, nil)
	var failure *OutputCorrectionFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no owned failure: %v", err)
	}
	defer failure.Handle().Release()
	if settled != 0 || finished != 0 || calls != 1 {
		t.Fatalf("initial calls=%d settled=%d finished=%d", calls, settled, finished)
	}
	ctx = WithOutputCorrectionHandle(ctx, failure.Handle())
	result, usage, err := e.CorrectOutputWithUsage(ctx, node, failure.Output(), failure.validation)
	if err != nil || calls != 2 || settled != 0 || usage.Tokens != 100 || !failure.Handle().HasPendingExtraction() {
		t.Fatalf("reask crossed extraction reservation: calls=%d settled=%d usage=%+v err=%v", calls, settled, usage, err)
	}
	result, usage, err = e.CorrectOutputWithUsage(ctx, node, result, failure.validation)
	if err != nil || calls != 2 || settled != 1 || usage.Tokens != 50 || info.InputTokens != 38 || info.OutputTokens != 12 || info.Model != spec {
		t.Fatalf("calls=%d settlement=%d info=%+v usage=%+v err=%v", calls, settled, info, usage, err)
	}
	if err = e.FinalizeCorrectedOutput(ctx, node, result); err != nil {
		t.Fatal(err)
	}
	if err = e.FinalizeCorrectedOutput(ctx, node, result); err != nil {
		t.Fatal(err)
	}
	if finished != 1 || settled != 1 {
		t.Fatalf("finalization duplicated settlement: %d/%d", finished, settled)
	}
	for _, r := range client.getCalls() {
		wire, _ := json.Marshal(r)
		if strings.Contains(string(wire), sentinel) {
			t.Fatal("generationForModel boundary bypassed")
		}
		if !strings.Contains(string(wire), secretguard.PlaceholderForName("sentinel")) {
			t.Fatalf("placeholder absent: %s", wire)
		}
	}
}

type cancelledExtractionClient struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelledExtractionClient) StreamResponse(ctx context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.calls++
	c.cancel()
	return nil, ctx.Err()
}
func TestStructuredExtractionCancellationAfterClientEntry(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &cancelledExtractionClient{cancel: cancel}
	var event StructuredExtractionInfo
	settled := 0
	e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithEventHooks(EventHooks{OnStructuredExtraction: func(_ string, i StructuredExtractionInfo) { event = i; settled++ }}))
	spec := e.detectorSuggestedModel()
	e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
	s := e.schemas["verdict_schema"]
	wire, _ := SchemaToJSON(s)
	_, spent, _, err := e.extractStructuredCorrection(ctx, "n", &delegate.Task{Model: "test/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished prose"}}, delegate.Result{}, s, delegate.BackendClaudeCode)
	t.Logf("calls=%d settlements=%d spent=%+v event=%+v err=%v", client.calls, settled, spent, event, err)
	if event.UnreportedCalls != 1 || spent.UnreportedCalls != 1 {
		t.Fatal("cancelled entered call is not marked unreported")
	}
}

func TestStructuredExtractionUnknownUsagePerTurn(t *testing.T) {
	for _, tc := range []struct {
		name            string
		entered         int
		response        ResponseInfo
		total, expected int
	}{
		{"cancel before entry", 0, ResponseInfo{Error: context.Canceled}, 0, 0},
		{"cancel after entry", 1, ResponseInfo{Error: context.Canceled}, 0, 1},
		{"deadline after entry", 1, ResponseInfo{Error: context.DeadlineExceeded}, 0, 1},
		{"cancelled stream already unknown", 1, ResponseInfo{Error: context.Canceled, Usage: Usage{UnreportedCalls: 1}}, 1, 1},
		{"provider refusal", 1, ResponseInfo{Error: errors.New("refused")}, 0, 0},
		{"cold never started", 0, ResponseInfo{Error: &StreamIdleError{Phase: StreamIdleCold}}, 1, 0},
		{"cold started", 1, ResponseInfo{Error: &StreamIdleError{Phase: StreamIdleCold}}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &extractionClient{entered: tc.entered}
			c.observeResponse(tc.response)
			got := c.settleUsage(Usage{UnreportedCalls: tc.total})
			if got.UnreportedCalls != tc.expected {
				t.Fatalf("unknown usage=%d want=%d", got.UnreportedCalls, tc.expected)
			}
		})
	}
	// First turn was already unreported; a second interrupted request is a
	// separate unknown bill, so max(total,1) would silently lose it.
	c := &extractionClient{entered: 1}
	c.observeResponse(ResponseInfo{Usage: Usage{UnreportedCalls: 1}})
	c.entered++
	c.observeResponse(ResponseInfo{Error: context.Canceled})
	if got := c.settleUsage(Usage{UnreportedCalls: 1}); got.UnreportedCalls != 2 {
		t.Fatalf("two unknown calls collapsed: %+v", got)
	}
	// Cancellation before the second client entry does not use the first entry
	// as evidence of a second paid request.
	d := &extractionClient{entered: 1}
	d.observeResponse(ResponseInfo{})
	d.observeResponse(ResponseInfo{Error: context.Canceled})
	if got := d.settleUsage(Usage{}); got.UnreportedCalls != 0 {
		t.Fatalf("new request invented from old entry: %+v", got)
	}
}

func TestStructuredExtractionCompleteResponseBeforeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &extractionClient{inner: newMockClient(toolUseEvents("x", "structured_output", `{"ok":true}`, 15, 5))}
	callbacks := 0
	obj, err := GenerateObjectDirect[map[string]any](ctx, c, GenerationOptions{Model: "openai/gpt-6-sol", ExplicitSchema: []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`), Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "yes"}}}}, OnResponse: func(r ResponseInfo) { callbacks++; c.observeResponse(r); cancel() }})
	if err != nil || callbacks != 1 || ctx.Err() != context.Canceled {
		t.Fatalf("callbacks=%d ctx=%v err=%v", callbacks, ctx.Err(), err)
	}
	u := c.settleUsage(objectUsage(obj))
	if u.InputTokens != 15 || u.OutputTokens != 5 || u.UnreportedCalls != 0 {
		t.Fatalf("post-response cancellation changed usage: %+v", u)
	}
}

func TestStructuredExtractionFirstReportedSecondCancelled(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "fixture")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var event StructuredExtractionInfo
	events := 0
	client := &funcClient{fn: func(n int, _ api.CreateMessageRequest) ([]api.StreamEvent, error) {
		if n == 1 {
			return textEvents("first complete answer without JSON", 23, 7), nil
		}
		cancel()
		return nil, context.Canceled
	}}
	e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithEventHooks(EventHooks{OnStructuredExtraction: func(_ string, i StructuredExtractionInfo) { event = i; events++ }}))
	spec := e.detectorSuggestedModel()
	e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
	schema := e.schemas["verdict_schema"]
	wire, _ := SchemaToJSON(schema)
	collector := &ExecutionUsageCollector{}
	_, spent, _, err := e.extractStructuredCorrection(WithExecutionUsageCollector(ctx, collector), "n", &delegate.Task{Model: "test/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished"}}, delegate.Result{}, schema, delegate.BackendClaudeCode)
	u, uerr := collector.Snapshot()
	if err == nil || uerr != nil || len(client.calls) != 2 || events != 1 || event.InputTokens != 23 || event.OutputTokens != 7 || event.UnreportedCalls != 1 || spent.UnreportedCalls != 1 || spent.Tokens != 30 || u.Tokens != 30 || u.UnreportedCalls != 1 {
		t.Fatalf("calls=%d events=%d event=%+v spent=%+v collector=%+v err=%v/%v", len(client.calls), events, event, spent, u, err, uerr)
	}
}

func TestStructuredExtractionKnownZeroPriceCollector(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "fixture")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	root := t.TempDir()
	dir := filepath.Join(root, "claw-code-go")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"entries": []any{map[string]any{"canonical": "glm-5.3", "provider": "zai", "input_usd_per_million": 0, "output_usd_per_million": 2}}, "fetched_at": time.Now().UTC().Format(time.RFC3339Nano), "source": "test"})
	if err := os.WriteFile(filepath.Join(dir, "models-cache.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "")
	var event StructuredExtractionInfo
	e := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithEventHooks(EventHooks{OnStructuredExtraction: func(_ string, i StructuredExtractionInfo) { event = i }}))
	spec := e.detectorSuggestedModel()
	client := newMockClient(toolUseEvents("x", "structured_output", `{"verdict":true,"reason":"ok"}`, 15, 0))
	e.registry.Register(strings.SplitN(spec, "/", 2)[0], func(string) (api.APIClient, error) { return client, nil })
	schema := e.schemas["verdict_schema"]
	wire, _ := SchemaToJSON(schema)
	collector := &ExecutionUsageCollector{}
	_, spent, ok, err := e.extractStructuredCorrection(WithExecutionUsageCollector(context.Background(), collector), "n", &delegate.Task{Model: "test/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished"}}, delegate.Result{}, schema, delegate.BackendClaudeCode)
	u, uerr := collector.Snapshot()
	t.Logf("ok=%v err=%v event=%+v spent=%+v collector=%+v/%v", ok, err, event, spent, u, uerr)
	if !ok || err != nil || !event.CostKnown || event.CostUSD != 0 || spent.Tokens != 15 || u.Tokens != 15 || u.UnpricedTokens != 0 {
		t.Fatal("known-zero extraction was presented as unpriced to budget")
	}
}

func TestStructuredExtractionPricingFactIsLocal(t *testing.T) {
	legacy := &ExecutionUsageCollector{}
	priced := &ExecutionUsageCollector{}
	r := delegate.Result{Tokens: 15, CostUSD: 0, UnreportedCalls: 2}
	observeExecutionUsage(WithExecutionUsageCollector(context.Background(), legacy), delegate.Task{}, r)
	observeExecutionUsagePriced(WithExecutionUsageCollector(context.Background(), priced), delegate.Task{}, r, true)
	a, ae := legacy.Snapshot()
	b, be := priced.Snapshot()
	if ae != nil || be != nil || a.Tokens != 15 || a.UnpricedTokens != 15 || a.UnreportedCalls != 2 || b.Tokens != 15 || b.UnpricedTokens != 0 || b.UnreportedCalls != 2 || b.OtherCostUSD != 0 {
		t.Fatalf("legacy=%+v/%v priced=%+v/%v", a, ae, b, be)
	}
}

func TestExtractionCredentialIdentity(t *testing.T) {
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "ambient-sentinel")
	t.Setenv("MOONSHOT_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			e := NewClawExecutor(NewRegistry(), verdictWorkflow())
			spec := e.detectorSuggestedModel()
			if !strings.HasPrefix(spec, "openai/") {
				t.Fatalf("fixture route %s", spec)
			}
			var keys []string
			var infos []StructuredExtractionInfo
			e.hooks.OnStructuredExtraction = func(_ string, i StructuredExtractionInfo) { infos = append(infos, i) }
			e.registry.Register("openai", func(string) (api.APIClient, error) {
				t.Fatal("ambient factory used instead of run key")
				return nil, nil
			})
			e.registry.providersWithKey["openai"] = func(_ string, key string) (api.APIClient, error) {
				keys = append(keys, key)
				if fail {
					return &funcClient{fn: func(int, api.CreateMessageRequest) ([]api.StreamEvent, error) {
						return nil, errors.New("scripted refusal")
					}}, nil
				}
				return newMockClient(toolUseEvents("x", "structured_output", `{"verdict":true,"reason":"ok"}`, 15, 5)), nil
			}
			schema := e.schemas["verdict_schema"]
			wire, err := SchemaToJSON(schema)
			if err != nil {
				t.Fatal(err)
			}
			for _, fp := range []string{"fp-a", "fp-b"} {
				secret := "secret-" + fp
				creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: secret}, Fingerprints: map[string]string{string(secrets.ProviderOpenAI): fp}}
				ctx := secrets.WithCredentials(t.Context(), creds)
				_, spent, ok, err := e.extractStructuredCorrection(ctx, "judge", &delegate.Task{Model: "test/original", OutputSchema: wire}, delegate.Result{Output: map[string]any{"text": "finished answer"}}, delegate.Result{}, schema, delegate.BackendClaudeCode)
				if (err != nil) != fail || ok == fail {
					t.Fatalf("outcome ok=%v err=%v", ok, err)
				}
				if len(infos) != len(keys) {
					t.Fatalf("settlements %d factory keys %d", len(infos), len(keys))
				}
				got := infos[len(infos)-1]
				want := credroute.KeySpend(creds, secrets.ProviderOpenAI)
				if got.Credential != want || spent.Credential != want || keys[len(keys)-1] != secret {
					t.Fatalf("identity=%+v spent=%+v want=%+v keys=%d", got.Credential, spent.Credential, want, len(keys))
				}
				raw, _ := json.Marshal(got)
				if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("ambient-sentinel")) {
					t.Fatal("secret exposed in settlement")
				}
			}
			if len(infos) != 2 || infos[0].CallID == infos[1].CallID {
				t.Fatalf("settlement identities %+v", infos)
			}
		})
	}
}
