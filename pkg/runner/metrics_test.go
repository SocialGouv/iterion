package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

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
