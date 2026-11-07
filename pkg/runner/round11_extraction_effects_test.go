package runner

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r11Extractor answers the extraction with a scripted tool block.
type r11Extractor struct {
	mu    sync.Mutex
	calls int
	json  string
}

func (c *r11Extractor) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	evs := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 200},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "x1", Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: c.json}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 30}},
		{Type: api.EventMessageStop},
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// r11Sink forwards to the meter and records what the store hooks persist
// beside the events: NodesServed (last write wins) and model_drift.
type r11Sink struct {
	mu     sync.Mutex
	inner  model.EventEmitter
	served []store.NodeServed
	evts   []store.Event
}

func (s *r11Sink) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	s.mu.Lock()
	s.evts = append(s.evts, evt)
	s.mu.Unlock()
	return s.inner.AppendEvent(ctx, runID, evt)
}

func (s *r11Sink) RecordNodeServed(_ context.Context, _, _ string, served store.NodeServed) error {
	s.mu.Lock()
	s.served = append(s.served, served)
	s.mu.Unlock()
	return nil
}

func r11Run(t *testing.T, node *ir.AgentNode, sink *r11Sink, ex *r11Extractor, regProvider string, bs ...*r8Backend) (map[string]any, error) {
	t.Helper()
	reg := model.NewRegistry()
	reg.Register(regProvider, func(string) (api.APIClient, error) { return ex, nil })
	hooks := model.NewStoreEventHooks(context.Background(), sink, "run", iterlog.Nop(), nil, nil)
	breg := delegate.NewRegistry()
	for _, b := range bs {
		b.hooks = &hooks
		breg.Register(b.name, b)
	}
	e := model.NewClawExecutor(reg, r8Workflow(),
		model.WithBackendRegistry(breg), model.WithEventHooks(hooks), model.WithLogger(iterlog.Nop()),
		model.WithDefaultBackend(node.Backend), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}))
	return e.Execute(model.WithRunID(context.Background(), "run"), node, map[string]any{})
}

func r11Dump(t *testing.T, sink *r11Sink, usage *metricsEmitter) {
	t.Helper()
	for _, e := range sink.evts {
		switch e.Type {
		case store.EventDelegateStarted, store.EventDelegateFinished, store.EventDelegateError, store.EventModelDrift, store.EventLLMStepFinished:
			t.Logf("  evt %s backend=%v reask=%v route=%v eff=%v decl=%v cost=%v tok=%v folded=%v err=%v", e.Type, e.Data["backend"], e.Data["reask"], e.Data["route_model"], e.Data["effective_model"], e.Data["declared_model"], e.Data["cost_usd"], e.Data["tokens"], e.Data["folded_spend"], e.Data["error"])
		}
	}
	for i, s := range sink.served {
		t.Logf("  served[%d] = %+v", i, s)
	}
	if usage != nil {
		for k, v := range usage.RouteTotals() {
			t.Logf("  route %+v -> $%.6f %d tok", k, v.costUSD, v.tokens())
		}
		c, in, out, agg := usage.RunTotals()
		t.Logf("  RunTotals: $%.6f in=%d out=%d agg=%d", c, in, out, agg)
	}
}

func r11Drifts(sink *r11Sink) int {
	n := 0
	for _, e := range sink.evts {
		if e.Type == store.EventModelDrift {
			n++
		}
	}
	return n
}

// P1 — post-chain extraction (X2 shape): the node's persisted served
// record must still name what served it (claude_code), and no drift event
// may claim the node's model drifted to the extraction's.
func TestR11_P1_PostChainExtractionServedRecord(t *testing.T) {
	r8Env(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
	sink := &r11Sink{inner: usage}
	ex := &r11Extractor{json: `{"verdict":true,"reason":"extracted"}`}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{missing: true, tokens: 100, cost: 1},
		{prose: "Really, I approve it.", tokens: 100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	out, err := r11Run(t, node, sink, ex, "anthropic", p)
	r11Dump(t, sink, usage)
	if err != nil || ex.calls != 1 {
		t.Fatalf("scenario broken: err=%v extraction calls=%d out=%v", err, ex.calls, out)
	}
	if len(sink.served) == 0 {
		t.Fatalf("no served record")
	}
	last := sink.served[len(sink.served)-1]
	t.Logf("FINAL NodesServed[impl] = %+v ; output _backend=%v _model=%v ; model_drift events=%d", last, out["_backend"], out["_model"], r11Drifts(sink))
	if last.Backend != delegate.BackendClaudeCode {
		t.Errorf("NodesServed[impl] names %s %s, the node was served by claude_code (output _backend=%v)", last.Backend, last.Model, out["_backend"])
	}
	if n := r11Drifts(sink); n != 0 {
		t.Errorf("%d model_drift event(s): the node's model never drifted", n)
	}
}

// P1b — in-chain extraction (X1 shape): drift claim only.
func TestR11_P1b_InChainExtractionDrift(t *testing.T) {
	r8Env(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
	sink := &r11Sink{inner: usage}
	ex := &r11Extractor{json: `{"verdict":true,"reason":"extracted"}`}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "I approve: verdict true, reason fine.", tokens: 100, cost: 1},
		{prose: "Really, I approve it.", tokens: 100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	out, err := r11Run(t, node, sink, ex, "anthropic", p)
	r11Dump(t, sink, usage)
	if err != nil || ex.calls != 1 {
		t.Fatalf("scenario broken: err=%v extraction calls=%d out=%v", err, ex.calls, out)
	}
	if len(sink.served) == 0 {
		t.Fatalf("no served record")
	}
	last := sink.served[len(sink.served)-1]
	t.Logf("FINAL NodesServed[impl] = %+v ; model_drift events=%d", last, r11Drifts(sink))
	if n := r11Drifts(sink); n != 0 {
		t.Errorf("%d model_drift event(s): the node's model never drifted", n)
	}
}

// P2 — in-chain extraction that FAILS (schema-invalid), then the budget is
// exhausted, the route is abandoned and a claw rescue serves. The meter must
// book exactly what the node paid; nothing twice, nothing lost.
func TestR11_P2_InChainFailedExtractionThenAbandon(t *testing.T) {
	r8Env(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), nil, nil)
	sink := &r11Sink{inner: usage}
	ex := &r11Extractor{json: `{"verdict":true}`}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "a", tokens: 100, cost: 1},
		{prose: "b", tokens: 100, cost: 1},
	}}
	r := &r8Backend{name: delegate.BackendClaw, calls: []r8Call{{valid: true, tokens: 50, cost: 0.5}}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"any"}})
	out, err := r11Run(t, node, sink, ex, "anthropic", p, r)
	r11Dump(t, sink, usage)
	cost, _, _, agg := usage.RunTotals()
	outCost, _ := out["_cost_usd"].(float64)
	outTok, _ := out["_tokens"].(int)
	t.Logf("err=%v cc.calls=%d claw.calls=%d extraction.calls=%d node _cost_usd=%v _tokens=%v ; meter $%.6f agg=%d", err, p.n, r.n, ex.calls, out["_cost_usd"], out["_tokens"], cost, agg)
	if ex.calls != 1 || r.n != 1 {
		t.Fatalf("scenario broken: extraction calls=%d rescue calls=%d err=%v", ex.calls, r.n, err)
	}
	if d := cost - outCost; d > 1e-9 || d < -1e-9 || agg != int64(outTok) {
		t.Errorf("meter $%.6f / %d vs node $%.6f / %d", cost, agg, outCost, outTok)
	}
	if n := r11Drifts(sink); n != 0 {
		t.Errorf("%d model_drift event(s): a failed extraction is no drift of the node's model", n)
	}
}

// P3 — the host-detected extraction stays off the credential plan: a plan
// whose (node, anthropic) row is the donor's lent subscription books the
// node's own answers there, never the extraction.
func TestR11_P3_ExtractionStaysOffTheNodesPlanRow(t *testing.T) {
	r8Env(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	plan := map[string]string{"impl" + model.PlanKeySep + "anthropic": "fp-donor"}
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), plan, nil)
	sink := &r11Sink{inner: usage}
	ex := &r11Extractor{json: `{"verdict":true,"reason":"extracted"}`}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "I approve: verdict true, reason fine.", tokens: 100, cost: 1},
		{prose: "Really, I approve it.", tokens: 100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	_, err := r11Run(t, node, sink, ex, "anthropic", p)
	r11Dump(t, sink, usage)
	rows, miss := fineFingerprintSpend(usage)
	t.Logf("err=%v fine rows=%+v miss=%+v", err, rows, miss)
	if err != nil || ex.calls != 1 {
		t.Fatalf("scenario broken: err=%v extraction calls=%d", err, ex.calls)
	}
	// The host-detected extraction is not the donor's: the donor's row books
	// the node's own two answers, $2, and nothing more.
	for _, r := range rows {
		if r.Fingerprint == "fp-donor" && math.Abs(r.CostUSD-2) > 1e-9 {
			t.Errorf("the donor's row books $%.6f for the node's $2: the extraction joined it", r.CostUSD)
		}
	}
}

// P3b — the detected provider is not in the node's plan: the extraction is
// no plan miss and the fine table stands.
func TestR11_P3b_ExtractionOnAnUnplannedProviderVoidsNothing(t *testing.T) {
	r8Env(t)
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
	plan := map[string]string{"impl" + model.PlanKeySep + "anthropic": "fp-donor"}
	usage := newMetricsEmitter(discardEmitter{}, metrics.New(), plan, nil)
	sink := &r11Sink{inner: usage}
	ex := &r11Extractor{json: `{"verdict":true,"reason":"extracted"}`}
	p := &r8Backend{name: delegate.BackendClaudeCode, calls: []r8Call{
		{prose: "I approve: verdict true, reason fine.", tokens: 100, cost: 1},
		{prose: "Really, I approve it.", tokens: 100, cost: 1},
	}}
	node := r8Node(delegate.BackendClaudeCode, "anthropic/claude-opus-5")
	_, err := r11Run(t, node, sink, ex, "openai", p)
	r11Dump(t, sink, usage)
	rows, miss := fineFingerprintSpend(usage)
	t.Logf("err=%v extraction.calls=%d fine rows=%+v (nil=%v) miss=%+v", err, ex.calls, rows, rows == nil, miss)
	if err != nil || ex.calls != 1 {
		t.Fatalf("scenario broken: err=%v extraction calls=%d", err, ex.calls)
	}
	// The plan names nobody for the host's extraction, by design: it is no
	// miss, and the fine table stands.
	if rows == nil || miss.observations != 0 {
		t.Errorf("the extraction voided the fine table: rows=%+v miss=%+v", rows, miss)
	}
}
