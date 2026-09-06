package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// recordingEmitter is a noop pkg/backend/model.EventEmitter that
// captures the events passed through.
type recordingEmitter struct{ events []store.Event }

func (r *recordingEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	r.events = append(r.events, evt)
	return &evt, nil
}

// planRecordingEmitter also satisfies model.PlanWriter, standing in for
// the Mongo cloud store whose plan-snapshot capability the metricsEmitter
// wrapper must forward rather than hide.
type planRecordingEmitter struct {
	recordingEmitter
	snaps []store.PlanSnapshot
}

func (p *planRecordingEmitter) AppendPlanSnapshot(_ context.Context, _ string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error) {
	snap.Seq = len(p.snaps)
	p.snaps = append(p.snaps, snap)
	return snap, true, nil
}

// TestMetricsEmitter_forwardsPlanWriter is the regression for the cloud
// "No plans captured for this run." bug: the capture hook detects the
// PlanWriter capability with `emitter.(PlanWriter)`, and the runner wraps
// the store in a metricsEmitter — so the wrapper must itself satisfy
// model.PlanWriter and delegate to the inner store, otherwise the plan
// sink is silently nil on every cloud run.
func TestMetricsEmitter_forwardsPlanWriter(t *testing.T) {
	inner := &planRecordingEmitter{}
	m := newMetricsEmitter(inner, metrics.New())

	// The wrapper must advertise the capability (this is exactly the
	// assertion NewStoreEventHooks performs on the emitter it receives).
	pw, ok := model.EventEmitter(m).(model.PlanWriter)
	if !ok {
		t.Fatal("metricsEmitter does not satisfy model.PlanWriter — cloud plan capture stays disabled")
	}

	snap := store.PlanSnapshot{NodeID: "n", Tool: "TodoWrite", Todos: []store.PlanTodo{{Content: "a", Status: "pending"}}}
	got, wrote, err := pw.AppendPlanSnapshot(context.Background(), "run-1", snap)
	if err != nil {
		t.Fatalf("forwarded append: %v", err)
	}
	if !wrote {
		t.Error("wrote=false, want true (delegated to inner)")
	}
	if got.Seq != 0 {
		t.Errorf("seq = %d, want 0", got.Seq)
	}
	if len(inner.snaps) != 1 {
		t.Errorf("inner received %d snapshots, want 1 (not forwarded)", len(inner.snaps))
	}
}

// TestMetricsEmitter_forwardsNodeServedRecorder locks in that the
// metricsEmitter wrapper satisfies model.NodeServedRecorder and
// delegates to the inner store; otherwise cloud runs drop NodesServed.
func TestMetricsEmitter_forwardsNodeServedRecorder(t *testing.T) {
	inner := &servedRecordingStore{}
	m := newMetricsEmitter(inner, metrics.New())
	rec, ok := model.EventEmitter(m).(model.NodeServedRecorder)
	if !ok {
		t.Fatal("metricsEmitter does not satisfy model.NodeServedRecorder — cloud runs would drop NodesServed")
	}
	want := store.NodeServed{Backend: "claude_code", Model: "glm-4.6"}
	if err := rec.RecordNodeServed(context.Background(), "run-1", "n1", want); err != nil {
		t.Fatalf("forwarded RecordNodeServed: %v", err)
	}
	if inner.got != want || inner.nodeID != "n1" {
		t.Errorf("inner got node=%q served=%+v, want n1 %+v", inner.nodeID, inner.got, want)
	}
}

// TestMetricsEmitter_nodeServedNoOpWhenInnerLacks locks in the benign
// no-op: an inner store that is NOT a NodeServedRecorder yields nil,
// matching today's nil-servedSink path, not a loud error.
func TestMetricsEmitter_nodeServedNoOpWhenInnerLacks(t *testing.T) {
	m := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	if err := m.RecordNodeServed(context.Background(), "run-1", "n", store.NodeServed{Backend: "x"}); err != nil {
		t.Fatalf("no-op forward: unexpected error %v", err)
	}
}

// servedRecordingStore also satisfies model.NodeServedRecorder, standing
// in for the Mongo cloud store whose NodesServed capability the
// metricsEmitter wrapper must forward rather than hide.
type servedRecordingStore struct {
	recordingEmitter
	nodeID string
	got    store.NodeServed
}

func (s *servedRecordingStore) RecordNodeServed(_ context.Context, _, nodeID string, served store.NodeServed) error {
	s.nodeID = nodeID
	s.got = served
	return nil
}

// TestMetricsEmitter_planWriterNoOpWhenInnerLacks locks in the benign
// no-op: an inner store that is NOT a PlanWriter yields (snap, false,
// nil) — identical to today's nil-planSink path, not a loud error.
func TestMetricsEmitter_planWriterNoOpWhenInnerLacks(t *testing.T) {
	m := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	snap := store.PlanSnapshot{NodeID: "n"}
	_, wrote, err := m.AppendPlanSnapshot(context.Background(), "run-1", snap)
	if err != nil {
		t.Fatalf("no-op forward: unexpected error %v", err)
	}
	if wrote {
		t.Error("wrote=true, want false (inner lacks PlanWriter)")
	}
}

func TestToFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{nil, 0},
		{float64(3.5), 3.5},
		{int(7), 7},
		{int64(11), 11},
		{int32(-2), 0},
		{float64(-1), 0},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		if got := toFloat(tc.in); got != tc.want {
			t.Errorf("toFloat(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeModelLabel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "unknown"},
		{"claude-sonnet-4-6", "claude-sonnet"},
		{"gpt-5.5-turbo-20260101", "gpt-5.5-turbo"},
		{"o3-mini", "o3-mini"}, // tail isn't all digits
		// The label normaliser strips every trailing -<digits> segment
		// recursively; on a model name where the digit suffix runs all
		// the way back to the prefix it can collapse to the head only.
		// Documented behaviour — Prometheus cardinality wins over fidelity.
		{"gpt-4-2026-01-01", "gpt"},
		{strings.Repeat("x", 80), strings.Repeat("x", 64)},
	}
	for _, tc := range cases {
		if got := normalizeModelLabel(tc.in); got != tc.want {
			t.Errorf("normalizeModelLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func TestMetricsEmitter_observeLLMStepFinished_tokensAndCost(t *testing.T) {
	reg := metrics.New()
	inner := &recordingEmitter{}
	m := newMetricsEmitter(inner, reg)

	// First request seeds modelByNode.
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type:   store.EventLLMRequest,
		RunID:  "run-1",
		NodeID: "n1",
		Data:   map[string]any{"model": "claude-sonnet-4-6"},
	})

	// Step uses a model known to the cost table; both the tokens and
	// the cost counters move.
	_, _ = m.AppendEvent(context.Background(), "run-1", store.Event{
		Type:   store.EventLLMStepFinished,
		RunID:  "run-1",
		NodeID: "n1",
		Data: map[string]any{
			"input_tokens":  float64(1000),
			"output_tokens": float64(500),
		},
	})

	tokens, err := reg.LLMTokensTotal.GetMetricWithLabelValues("claw", "claude-sonnet", "input")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if got := counterValue(t, tokens); got != 1000 {
		t.Errorf("input tokens = %v, want 1000", got)
	}

	cost, err := reg.LLMCostUSDTotal.GetMetricWithLabelValues("claw", "claude-sonnet")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues cost: %v", err)
	}
	if got := counterValue(t, cost); got <= 0 {
		t.Errorf("expected positive cost for known model, got %v", got)
	}

	if len(inner.events) != 2 {
		t.Errorf("inner emitter received %d events, want 2", len(inner.events))
	}
}

func TestMetricsEmitter_unknownModel_costStaysZero(t *testing.T) {
	reg := metrics.New()
	m := newMetricsEmitter(&recordingEmitter{}, reg)

	// No prior llm_request — modelByNode is empty so the step is
	// labelled "unknown" and the cost branch must NOT update the
	// counter (the cost table has no entry).
	_, _ = m.AppendEvent(context.Background(), "run-2", store.Event{
		Type:   store.EventLLMStepFinished,
		RunID:  "run-2",
		NodeID: "n9",
		Data: map[string]any{
			"input_tokens":  float64(100),
			"output_tokens": float64(50),
		},
	})

	cost, _ := reg.LLMCostUSDTotal.GetMetricWithLabelValues("claw", "unknown")
	if got := counterValue(t, cost); got != 0 {
		t.Errorf("unknown-model cost = %v, want 0 (counter must not be touched)", got)
	}
}

func TestMetricsEmitter_delegateFinished_aggregatedTokens(t *testing.T) {
	reg := metrics.New()
	m := newMetricsEmitter(&recordingEmitter{}, reg)

	_, _ = m.AppendEvent(context.Background(), "run-3", store.Event{
		Type:   store.EventDelegateFinished,
		RunID:  "run-3",
		NodeID: "n2",
		Data: map[string]any{
			"backend": "claude_code",
			"tokens":  float64(420),
		},
	})

	c, err := reg.LLMTokensTotal.GetMetricWithLabelValues("claude_code", "unknown", "input")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if got := counterValue(t, c); got != 420 {
		t.Errorf("delegate tokens = %v, want 420", got)
	}
}

func TestMetricsEmitter_addTokens_ignoresZeroOrNegative(t *testing.T) {
	reg := metrics.New()
	m := newMetricsEmitter(&recordingEmitter{}, reg)

	m.addTokens("claw", "x", "input", float64(0))
	m.addTokens("claw", "x", "input", float64(-5))
	m.addTokens("", "x", "input", float64(10))

	c, _ := reg.LLMTokensTotal.GetMetricWithLabelValues("claw", "x", "input")
	if got := counterValue(t, c); got != 0 {
		t.Errorf("expected counter untouched, got %v", got)
	}
}

func TestMetricsEmitter_lookupModel_emptyNodeID(t *testing.T) {
	m := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	if got := m.lookupModel(""); got != "" {
		t.Errorf("lookupModel(\"\") = %q, want empty", got)
	}
}

// A delegate backend's spend must reach RunTotals — that number is what
// charges the org's monthly cost cap and what decrements a credential-pool
// donor's quota. It used to stop at the tokens: the whole attempt of a
// claude_code run reported $0, so no cap could ever trip.
//
// The event here is produced by the PRODUCTION hook (model.NewStoreEventHooks
// → OnDelegateFinished), not hand-assembled, so the test fails if either side
// of the join drifts — the emitter dropping the field, or the consumer
// reading a different key.
func TestMetricsEmitter_delegateFinished_costReachesRunTotals(t *testing.T) {
	inner := &recordingEmitter{}
	usage := newMetricsEmitter(inner, metrics.New())

	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-4", iterlog.New(iterlog.LevelError, nil), nil,
	)
	hooks.OnDelegateFinished("n1", model.DelegateInfo{
		BackendName: "claude_code",
		Tokens:      1200,
		CostUSD:     0.4242,
	})

	if len(inner.events) != 1 {
		t.Fatalf("inner emitter received %d events, want 1", len(inner.events))
	}
	if got := inner.events[0].Data["cost_usd"]; got != 0.4242 {
		t.Errorf("emitted cost_usd = %v, want 0.4242 — the hook is dropping the delegation's cost", got)
	}

	costUSD, in, _ := usage.RunTotals()
	if costUSD != 0.4242 {
		t.Errorf("RunTotals cost = %v, want 0.4242 — delegate spend is not charged to the run", costUSD)
	}
	if in != 1200 {
		t.Errorf("RunTotals input tokens = %d, want 1200", in)
	}
}

// The counterpart: an unpriced model omits `_cost_usd`, so the hook omits
// `cost_usd`, so the run's cost stays at zero rather than recording a
// fabricated free call.
func TestMetricsEmitter_delegateFinished_unpricedStaysZero(t *testing.T) {
	inner := &recordingEmitter{}
	usage := newMetricsEmitter(inner, metrics.New())

	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-5", iterlog.New(iterlog.LevelError, nil), nil,
	)
	hooks.OnDelegateFinished("n1", model.DelegateInfo{BackendName: "claude_code", Tokens: 900})

	if _, present := inner.events[0].Data["cost_usd"]; present {
		t.Error("cost_usd present for an unpriced delegation — 'no data' must stay distinguishable from $0")
	}
	if costUSD, _, _ := usage.RunTotals(); costUSD != 0 {
		t.Errorf("RunTotals cost = %v, want 0", costUSD)
	}
}

// claw is in-process but still dispatches through the delegate hook, so it
// emits BOTH a per-step llm_step_finished and a delegation total. Counting
// both charged every claw run twice: an org's monthly cap would trip at
// half its budget, and a lending donor would be drained at twice the rate
// they agreed to.
func TestMetricsEmitter_clawCostIsNotCountedTwice(t *testing.T) {
	inner := &recordingEmitter{}
	usage := newMetricsEmitter(inner, metrics.New())
	ctx := context.Background()

	// A claw node: a priced step, then the delegation total for the same work.
	_, _ = usage.AppendEvent(ctx, "run-1", store.Event{
		Type: store.EventLLMRequest, RunID: "run-1", NodeID: "n1",
		Data: map[string]any{"model": "claude-sonnet-4-6"},
	})
	_, _ = usage.AppendEvent(ctx, "run-1", store.Event{
		Type: store.EventLLMStepFinished, RunID: "run-1", NodeID: "n1",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(500)},
	})
	perStep, _, _ := usage.RunTotals()
	if perStep <= 0 {
		t.Fatalf("the step itself priced to %v — the test cannot show a double count", perStep)
	}

	_, _ = usage.AppendEvent(ctx, "run-1", store.Event{
		Type: store.EventDelegateFinished, RunID: "run-1", NodeID: "n1",
		Data: map[string]any{"backend": "claw", "tokens": float64(1500), "cost_usd": perStep},
	})

	total, in, out := usage.RunTotals()
	if total != perStep {
		t.Errorf("cost = %v after the delegation total, want %v — claw was charged twice", total, perStep)
	}
	// The delegation total is the SUM of the steps already counted: booking
	// it again doubles the token figure on the org bucket and the ledger.
	if in != 1000 || out != 500 {
		t.Errorf("tokens = %d in / %d out after the delegation total, want 1000 / 500 — claw's tokens were counted twice", in, out)
	}
}

// A CLI delegate has no per-step events, so its delegation total is the
// only cost signal and must still be counted.
func TestMetricsEmitter_cliDelegateCostIsStillCounted(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	_, _ = usage.AppendEvent(context.Background(), "run-2", store.Event{
		Type: store.EventDelegateFinished, RunID: "run-2", NodeID: "n1",
		Data: map[string]any{"backend": "claude_code", "tokens": float64(900), "cost_usd": 0.42},
	})
	if cost, _, _ := usage.RunTotals(); cost != 0.42 {
		t.Errorf("cost = %v, want 0.42", cost)
	}
}

// #805 — the production shape of a sandboxed claw node. The LLM loop runs in
// `iterion __claw-runner` inside the container; an in-container runner that
// relays no per-step events leaves the host with the delegation pair alone:
// delegate_started names the model, delegate_finished carries one aggregate
// token count and, when the container could not price the call, no cost. The
// route must still be the node's provider-qualified model — an empty or bare
// model falls to the backend's default wire and charges an OpenAI forfait's
// tokens to the Anthropic credential — with the tokens booked and a price
// taken off the table rather than the $0 the claw double-count guard used to
// leave (a pool donor "reads $0 forever").
func TestMetricsEmitter_sandboxedClaw_delegateOnlyIsPricedAndRouted(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	ctx := context.Background()

	_, _ = usage.AppendEvent(ctx, "run-1", store.Event{
		Type: store.EventDelegateStarted, RunID: "run-1", NodeID: "plan_review",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol"},
	})
	_, _ = usage.AppendEvent(ctx, "run-1", store.Event{
		Type: store.EventDelegateFinished, RunID: "run-1", NodeID: "plan_review",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol", "tokens": float64(25000)},
	})

	routes := usage.RouteTotals()
	got, ok := routes[routeKey{backend: "claw", model: "openai/gpt-5.6-sol"}]
	if !ok {
		t.Fatalf("routes = %v, want one keyed (claw, openai/gpt-5.6-sol) — the declared model must name the route", routes)
	}
	if got.inputTokens != 25000 {
		t.Errorf("route input tokens = %d, want 25000", got.inputTokens)
	}
	if got.costUSD <= 0 {
		t.Errorf("route cost = %v, want > 0: the table prices gpt-5.6-sol, and a delegation the guard excluded is not a free call", got.costUSD)
	}
	cost, in, _ := usage.RunTotals()
	if cost != got.costUSD || in != 25000 {
		t.Errorf("RunTotals = ($%v, %d) — must mirror the route (%+v)", cost, in, got)
	}
}

// The delegation's own figure is exact (the container split input from output
// when it priced the call); the host-side table price is only for a delegation
// that carries none.
func TestMetricsEmitter_sandboxedClaw_delegateCostWinsOverTheTable(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "claw", "tokens": float64(25000), "cost_usd": 0.1234}})
	if cost, in, _ := usage.RunTotals(); cost != 0.1234 || in != 25000 {
		t.Fatalf("RunTotals = ($%v, %d), want ($0.1234, 25000)", cost, in)
	}
}

// Zero is unknown, never free: a delegation no source can price books its
// tokens on the route and leaves the cost at zero — no fabricated figure.
func TestMetricsEmitter_sandboxedClaw_unknownModelStaysUnpriced(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-99-nowhere"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "claw", "tokens": float64(400)}})
	cost, in, _ := usage.RunTotals()
	if cost != 0 {
		t.Errorf("cost = %v for a model no source prices, want 0 (unknown)", cost)
	}
	if in != 400 {
		t.Errorf("input tokens = %d, want 400 — the tokens are known even when the price is not", in)
	}
	if _, ok := usage.RouteTotals()[routeKey{backend: "claw", model: "openai/gpt-99-nowhere"}]; !ok {
		t.Errorf("routes = %v, want the unpriced route present so the credential still sees its tokens", usage.RouteTotals())
	}
}

// The in-process shape of the same class. claw strips the provider before the
// call, so its llm_request reports the BARE id; keyed on that, the route of a
// claw node on an OpenAI model falls to the anthropic wire exactly like the
// sandboxed one did. The declared model supplies the provider the report
// dropped — for the same model only; a different id (a fallback element) is
// kept as reported.
func TestMetricsEmitter_bareStepModelKeepsTheDeclaredProvider(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol"}})
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "gpt-5.6-sol"}})
	usage.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(100)}})
	routes := usage.RouteTotals()
	if _, ok := routes[routeKey{backend: "claw", model: "openai/gpt-5.6-sol"}]; !ok {
		t.Fatalf("routes = %v, want (claw, openai/gpt-5.6-sol): the bare step id must inherit the declared provider", routes)
	}

	other := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	other.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol"}})
	other.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "claude-opus-5"}})
	other.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n",
		Data: map[string]any{"input_tokens": float64(10), "output_tokens": float64(1)}})
	if _, ok := other.RouteTotals()[routeKey{backend: "claw", model: "claude-opus-5"}]; !ok {
		t.Fatalf("routes = %v, want (claw, claude-opus-5): a different model than declared is not re-labelled", other.RouteTotals())
	}
}

// delegate_finished.effective_model is what the provider reports it ran; when
// present it names the route over the declared model.
func TestMetricsEmitter_effectiveModelNamesTheRoute(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claude_code", "declared_model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "claude_code", "declared_model": "anthropic/claude-opus-5",
			"effective_model": "claude-sonnet-4-6", "tokens": float64(900), "cost_usd": 0.42}})
	routes := usage.RouteTotals()
	if r, ok := routes[routeKey{backend: "claude_code", model: "claude-sonnet-4-6"}]; !ok || r.costUSD != 0.42 {
		t.Fatalf("routes = %v, want (claude_code, claude-sonnet-4-6) carrying $0.42", routes)
	}
}

// The sandboxed shape WITH the relay, through the production hooks on both
// sides: the in-container runner's hooks encode each step, the host decodes
// and re-fires them through its store hooks — whose emitter is this metrics
// emitter on a runner pod — around the delegation pair the host emits
// itself. The route is named by the declared spec, the tokens are the
// steps' exact input/output split, the cost is the steps' price, and the
// delegation total (a summary of those steps) is not booked again.
func TestMetricsEmitter_relayedSandboxedClawStepsMeterLikeInProcess(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	host := model.NewStoreEventHooks(context.Background(), usage, "run-relay", iterlog.New(iterlog.LevelError, nil), nil)
	relay := model.SandboxRelayHooks(func(env delegate.Envelope) error {
		var ed delegate.EventData
		if err := json.Unmarshal(env.Data, &ed); err != nil {
			return err
		}
		_, err := model.ApplyRelayedEvent(host, "plan_review", ed.Type, ed.Payload)
		return err
	}, func(err error) { t.Errorf("relay: %v", err) })

	host.OnDelegateStarted("plan_review", model.DelegateInfo{BackendName: "claw", DeclaredModel: "openai/gpt-5.6-sol"})
	relay.OnLLMRequest("plan_review", model.LLMRequestInfo{Model: "gpt-5.6-sol"})
	relay.OnLLMStepFinish("plan_review", model.LLMStepInfo{Number: 1, InputTokens: 20000, OutputTokens: 3000})
	relay.OnLLMStepFinish("plan_review", model.LLMStepInfo{Number: 2, InputTokens: 20000, OutputTokens: 3000})
	stepsCost, _, _ := usage.RunTotals()
	if stepsCost <= 0 {
		t.Fatalf("the relayed steps priced to %v — gpt-5.6-sol is in the table", stepsCost)
	}
	host.OnDelegateFinished("plan_review", model.DelegateInfo{BackendName: "claw", DeclaredModel: "openai/gpt-5.6-sol", Tokens: 46000, CostUSD: 0.5})

	routes := usage.RouteTotals()
	got, ok := routes[routeKey{backend: "claw", model: "openai/gpt-5.6-sol"}]
	if !ok {
		t.Fatalf("routes = %v, want (claw, openai/gpt-5.6-sol)", routes)
	}
	if got.inputTokens != 40000 || got.outputTokens != 6000 {
		t.Errorf("route tokens = %d in / %d out, want 40000 / 6000 — the steps' split, counted once", got.inputTokens, got.outputTokens)
	}
	if got.costUSD != stepsCost {
		t.Errorf("route cost = %v, want the steps' %v — the delegation total was re-priced", got.costUSD, stepsCost)
	}
	if len(routes) != 1 {
		t.Errorf("routes = %v, want the one route", routes)
	}
	if cost, in, out := usage.RunTotals(); cost != stepsCost || in != 40000 || out != 6000 {
		t.Errorf("RunTotals = ($%v, %d, %d), want ($%v, 40000, 6000)", cost, in, out, stepsCost)
	}
}

// A node's second attempt (a loop iteration, a retry) opens with a fresh
// delegate_started; whether its steps were seen is decided per attempt, so a
// relayed first pass never hides an unrelayed second one.
func TestMetricsEmitter_newAttemptResetsTheStepGuard(t *testing.T) {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	start := store.Event{Type: store.EventDelegateStarted, NodeID: "n",
		Data: map[string]any{"backend": "claw", "declared_model": "openai/gpt-5.6-sol"}}
	usage.observe(start)
	usage.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n",
		Data: map[string]any{"input_tokens": float64(1000), "output_tokens": float64(0)}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "claw", "tokens": float64(1000)}})
	if _, in, _ := usage.RunTotals(); in != 1000 {
		t.Fatalf("after a summarised first attempt: input tokens = %d, want 1000", in)
	}
	usage.observe(start)
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "claw", "tokens": float64(500)}})
	if _, in, _ := usage.RunTotals(); in != 1500 {
		t.Fatalf("after an unrelayed second attempt: input tokens = %d, want 1500 — the guard must reset per attempt", in)
	}
}
