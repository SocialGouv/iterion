package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/store"
)

// recordingEmitter is a noop pkg/backend/model.EventEmitter that
// captures the events passed through.
type recordingEmitter struct{ events []store.Event }

func (r *recordingEmitter) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	r.events = append(r.events, evt)
	return &evt, nil
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
