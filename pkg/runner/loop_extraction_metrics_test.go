package runner

import (
	"github.com/SocialGouv/iterion/pkg/backend/credroute"

	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestStructuredExtractionMetersOwnRoute(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"success", "error"} {
		t.Run(outcome, func(t *testing.T) {
			meter := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, t.Context())
			log := &eventLog{inner: meter}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			hooks := model.NewStoreEventHooks(ctx, log, "run", iterlog.Nop(), nil, nil)
			hooks.OnDelegateStarted("judge", model.DelegateInfo{BackendName: delegate.BackendClaudeCode, DeclaredModel: "anthropic/claude-opus-5", RouteModel: "anthropic/claude-opus-5"})
			// Keep a prior step flag: extraction must not reset it or set a different node.
			meter.mu.Lock()
			meter.stepsSeen["prior"] = true
			meter.mu.Unlock()
			hooks.OnStructuredExtraction("judge", model.StructuredExtractionInfo{CallID: "extract", Backend: delegate.BackendClaw, Model: "openai/gpt-6-sol", Provider: "openai", InputTokens: 30, OutputTokens: 7, UnreportedCalls: 2, CostKnown: true, CostUSD: .5, Outcome: outcome})
			if len(log.events) != 2 {
				t.Fatalf("events=%+v", log.events)
			}
			ev := log.events[1]
			raw, _ := json.Marshal(ev)
			var replay store.Event
			if err := json.Unmarshal(raw, &replay); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() { meter.observe(replay) })
			}
			wg.Wait()
			if meter.modelByNode["judge"] != "anthropic/claude-opus-5" || meter.declaredByNode["judge"] != "anthropic/claude-opus-5" || meter.stepsSeen["judge"] || !meter.stepsSeen["prior"] {
				t.Fatalf("provenance changed: %+v %+v %+v", meter.modelByNode, meter.declaredByNode, meter.stepsSeen)
			}
			// The original delegate is billed independently, after extraction.
			hooks.OnDelegateFinished("judge", model.DelegateInfo{BackendName: delegate.BackendClaudeCode, DeclaredModel: "anthropic/claude-opus-5", RouteModel: "anthropic/claude-opus-5", Tokens: 100, CostUSD: 2})
			price, in, out, aggregate := meter.RunTotals()
			if price != 2.5 || in != 30 || out != 7 || aggregate != 100 {
				t.Fatalf("totals=%g/%d/%d/%d", price, in, out, aggregate)
			}
			route := meter.RouteTotals()[routeKey{backend: delegate.BackendClaw, model: "openai/gpt-6-sol"}]
			if route.costUSD != .5 || route.inputTokens != 30 || route.outputTokens != 7 || route.unreportedCalls != 2 {
				t.Fatalf("route=%+v", route)
			}
			creds := credusage.NewMemoryCounter()
			orgs := orgusage.NewMemoryCounter()
			runner := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: creds, OrgUsage: orgs}}
			held := secrets.WithCredentials(ctx, secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "fake"}, OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/fixture"}, Fingerprints: map[string]string{string(secrets.ProviderOpenAI): "fp-openai", delegate.BackendClaudeCode: "fp-forfait"}})
			runner.recordOrgSpend(held, &queue.RunMessage{RunID: "run", TenantID: "team", OrgID: "org"}, meter)
			now := time.Now().UTC()
			rows, err := creds.List(context.Background(), now, "team")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("credential rows: %+v", rows)
			}
			for _, row := range rows {
				switch row.Fingerprint {
				case "fp-openai":
					if row.CostUSD != .5 || row.InputTokens != 30 || row.OutputTokens != 7 || row.AggregateTokens != 0 {
						t.Fatalf("extract credential: %+v", row)
					}
				case "fp-forfait":
					if row.CostUSD != 2 || row.AggregateTokens != 100 || row.InputTokens != 0 {
						t.Fatalf("parent credential: %+v", row)
					}
				default:
					t.Fatalf("unexpected credential: %+v", row)
				}
			}
			org, err := orgs.Usage(context.Background(), orgusage.OrgSubject("org"), now)
			if err != nil || org.CostUSD != 2.5 || org.InputTokens != 30 || org.OutputTokens != 7 || org.AggregateTokens != 100 {
				t.Fatalf("org=%+v err=%v", org, err)
			}
		})
	}
}

func TestStructuredExtractionUnpricedAndZero(t *testing.T) {
	t.Parallel()
	m := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, t.Context())
	h := model.NewStoreEventHooks(context.Background(), m, "run", iterlog.Nop(), nil, nil)
	h.OnStructuredExtraction("n", model.StructuredExtractionInfo{CallID: "unknown", Backend: "claw", Model: "openai/unknown", InputTokens: 9, Outcome: "error", UnreportedCalls: 1})
	h.OnStructuredExtraction("n", model.StructuredExtractionInfo{CallID: "free", Backend: "claw", Model: "openai/free", InputTokens: 2, CostKnown: true, Outcome: "success"})
	c, in, out, agg := m.RunTotals()
	if c != 0 || in != 11 || out != 0 || agg != 0 {
		t.Fatalf("totals %g/%d/%d/%d", c, in, out, agg)
	}
	rec := httptest.NewRecorder()
	m.reg.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `iterion_llm_cost_usd_total{backend="claw",model="openai/free"} 0`) || strings.Contains(body, `iterion_llm_cost_usd_total{backend="claw",model="openai/unknown"}`) {
		t.Fatalf("price presence lost: %s", body)
	}
	routes := m.RouteTotals()
	if len(routes) != 2 || routes[routeKey{backend: "claw", model: "openai/unknown"}].unreportedCalls != 1 {
		t.Fatalf("routes=%+v", routes)
	}
}

// The actual local executor folds the extraction into OnNodeFinished while
// its delegate events remain raw. The route meter must see each component once.
func TestStructuredExtractionOwnedRealExecutor(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "fixture")
	t.Setenv("MOONSHOT_API_KEY", "")
	t.Setenv("XAI_API_KEY", "")
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("ITERION_MODEL_SPECS", "off")
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed provider"}[fail], func(t *testing.T) {
			meter := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, t.Context())
			events := &eventLog{inner: meter}
			hooks := model.NewStoreEventHooks(context.Background(), events, "run", iterlog.Nop(), nil, nil)
			finished := 0
			var aggregate map[string]any
			hooks = model.ChainHooks(hooks, model.EventHooks{OnNodeFinished: func(_ string, o map[string]any) { finished++; aggregate = o }})
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, &scriptedBackend{res: delegate.Result{Output: map[string]any{"text": "finished answer"}, ParseFallback: true, Tokens: 100, CostUSD: 2}})
			client := &extractionMeterClient{fail: fail}
			clients := model.NewRegistry()
			clients.Register("openai", func(string) (api.APIClient, error) { return client, nil })
			workflow := &ir.Workflow{Prompts: map[string]*ir.Prompt{"ask": {Body: "go"}}, Schemas: map[string]*ir.Schema{"verdict": {Name: "verdict", Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}}}}}
			executor := model.NewClawExecutor(clients, workflow, model.WithBackendRegistry(reg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()), model.WithWorkDir(t.TempDir()), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1}))
			node := &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "judge"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", UserPrompt: "ask"}, SchemaFields: ir.SchemaFields{OutputSchema: "verdict"}}
			ctx := model.WithOutputCorrectionOwnership(model.WithRunID(t.Context(), "run"), model.OutputCorrectionOwnership{Protocol: model.OutputCorrectionProtocol, Authority: store.RunExecutionAuthority{RunID: "run", Epoch: "epoch"}})
			_, err := executor.Execute(ctx, node, map[string]any{})
			var failure *model.OutputCorrectionFailure
			if !errors.As(err, &failure) {
				t.Fatalf("missing owned handoff: %v", err)
			}
			defer failure.Handle().Release()
			ctx = model.WithOutputCorrectionHandle(ctx, failure.Handle())
			output, _, err := executor.CorrectOutputWithUsage(ctx, node, failure.Output(), err)
			if err != nil || !failure.Handle().HasPendingExtraction() || client.calls != 0 || finished != 0 {
				t.Fatalf("reask must leave extraction pending: err=%v calls=%d finished=%d", err, client.calls, finished)
			}
			for _, ev := range events.events {
				if ev.Type == store.EventStructuredExtraction {
					t.Fatal("reask settled extraction before its reservation")
				}
			}
			output, _, err = executor.CorrectOutputWithUsage(ctx, node, output, model.ValidateOutput(output, workflow.Schemas["verdict"]))
			if err == nil {
				err = model.ValidateOutput(output, workflow.Schemas["verdict"])
			}
			if err == nil {
				err = executor.FinalizeCorrectedOutput(ctx, node, output)
			}
			if (err != nil) != fail {
				t.Fatalf("fail=%v err=%v", fail, err)
			}
			count := 0
			for _, ev := range events.events {
				if ev.Type == store.EventStructuredExtraction {
					count++
					meter.observe(ev)
				}
			}
			if count != 1 || client.calls != 1 {
				t.Fatalf("settlements=%d calls=%d", count, client.calls)
			}
			routes := meter.RouteTotals()
			parent := routes[routeKey{backend: delegate.BackendClaudeCode, model: "anthropic/claude-opus-5"}]
			var extract routeTotals
			for route, totals := range routes {
				if route.backend == delegate.BackendClaw && route.model == "openai/gpt-6-sol" {
					extract = totals
				}
			}
			if parent.aggregateTokens != 200 || parent.costUSD != 4 {
				t.Fatalf("parent includes extraction or lost retry: %+v all=%+v", parent, routes)
			}
			wantIn, wantOut := int64(30), int64(7)
			if fail {
				wantIn = 0
				wantOut = 0
			}
			if extract.inputTokens != wantIn || extract.outputTokens != wantOut {
				t.Fatalf("extract=%+v", extract)
			}
			if meter.modelByNode["judge"] != "anthropic/claude-opus-5" || meter.stepsSeen["judge"] {
				t.Fatalf("node attribution=%v steps=%v", meter.modelByNode, meter.stepsSeen)
			}
			if !fail && (finished != 1 || toFloat(aggregate["_tokens"]) != 237) {
				t.Fatalf("aggregate hook=%d output=%+v", finished, aggregate)
			}
			if fail && finished != 0 {
				t.Fatal("failure finalized a node")
			}
		})
	}
}

type extractionMeterClient struct {
	fail  bool
	calls int
}

func (c *extractionMeterClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.calls++
	if c.fail {
		return nil, errors.New("scripted extraction refusal")
	}
	ch := make(chan api.StreamEvent, 6)
	for _, e := range []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 30}, {Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "extract", Name: "structured_output"}}, {Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":true,"reason":"ok"}`}}, {Type: api.EventContentBlockStop, Index: 0}, {Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 7}}, {Type: api.EventMessageStop}} {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestExtractionCredentialPartitions(t *testing.T) {
	m := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, t.Context())
	log := &eventLog{inner: m}
	h := model.NewStoreEventHooks(t.Context(), log, "run", iterlog.Nop(), nil, nil)
	for _, fp := range []string{"fp-a", "fp-b"} {
		spend := credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "openai", Fingerprint: fp}
		h.OnStructuredExtraction("n", model.StructuredExtractionInfo{CallID: fp, Backend: delegate.BackendClaw, Model: "openai/gpt-6-sol", Credential: spend, InputTokens: 10, OutputTokens: 2, CostKnown: true, CostUSD: .5, Outcome: "success"})
		ev := log.events[len(log.events)-1]
		got, ok := credroute.SpendFromEvent(ev.Data)
		if !ok || got != spend {
			t.Fatalf("event lost credential %+v", ev.Data)
		}
		raw, _ := json.Marshal(ev)
		var replay store.Event
		if err := json.Unmarshal(raw, &replay); err != nil {
			t.Fatal(err)
		}
		m.observe(replay)
	}
	routes := m.RouteTotals()
	if len(routes) != 2 {
		t.Fatalf("merged credentials: %+v", routes)
	}
	for key, totals := range routes {
		if key.credential.Fingerprint == "" || totals.costUSD != .5 || totals.inputTokens != 10 || totals.outputTokens != 2 {
			t.Fatalf("partition=%+v totals=%+v", key, totals)
		}
	}
	cost, in, out, agg := m.RunTotals()
	if cost != 1 || in != 20 || out != 4 || agg != 0 {
		t.Fatalf("duplicate replay billed %g %d %d %d", cost, in, out, agg)
	}
}
