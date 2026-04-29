package benchmark

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/store"
)

func scrape(t *testing.T, exp *PrometheusExporter) string {
	t.Helper()
	srv := httptest.NewServer(exp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func TestExporter_LLMRequestRetryToolCounters(t *testing.T) {
	exp := NewPrometheusExporter("run-abc", nil)
	hooks := exp.EventHooks()

	hooks.OnLLMRequest("agent_a", model.LLMRequestInfo{Model: "anthropic/claude-sonnet-4-6"})
	hooks.OnLLMRequest("agent_a", model.LLMRequestInfo{Model: "anthropic/claude-sonnet-4-6"})
	hooks.OnLLMRetry("agent_a", model.RetryInfo{Attempt: 1})
	hooks.OnToolCall("agent_a", model.LLMToolCallInfo{ToolName: "bash"})
	hooks.OnToolCall("agent_a", model.LLMToolCallInfo{ToolName: "bash"})
	hooks.OnToolCall("agent_a", model.LLMToolCallInfo{ToolName: "read_file"})

	body := scrape(t, exp)
	wants := []string{
		`iterion_llm_request_total{model="anthropic/claude-sonnet-4-6",node_id="agent_a"} 2`,
		`iterion_llm_retry_total{model="",node_id="agent_a"} 1`,
		`iterion_tool_call_total{node_id="agent_a",tool="bash"} 2`,
		`iterion_tool_call_total{node_id="agent_a",tool="read_file"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing line %q\n--full--\n%s", w, body)
		}
	}
}

func TestExporter_NodeFinishedTokensAndCost(t *testing.T) {
	exp := NewPrometheusExporter("run-xyz", nil)
	hooks := exp.EventHooks()

	hooks.OnNodeFinished("plan", map[string]interface{}{
		"_tokens":   1234,
		"_model":    "anthropic/claude-sonnet-4-6",
		"_cost_usd": 0.0012,
	})

	body := scrape(t, exp)
	wants := []string{
		`iterion_node_tokens_total{model="anthropic/claude-sonnet-4-6",node_id="plan",run_id="run-xyz"} 1234`,
		`iterion_node_cost_usd_total{node_id="plan",run_id="run-xyz"} 0.0012`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("metrics missing line %q\n--full--\n%s", w, body)
		}
	}
}

func TestExporter_LLMResponseDurationHistogram(t *testing.T) {
	exp := NewPrometheusExporter("run-1", nil)
	hooks := exp.EventHooks()

	hooks.OnLLMResponse("agent_b", model.LLMResponseInfo{Latency: 250 * time.Millisecond})
	hooks.OnLLMResponse("agent_b", model.LLMResponseInfo{Latency: 1100 * time.Millisecond})

	body := scrape(t, exp)
	if !strings.Contains(body, `iterion_node_duration_ms_count{node_id="agent_b"} 2`) {
		t.Errorf("expected histogram count=2 for agent_b, body:\n%s", body)
	}
	// 250ms falls in the 500ms bucket; 1100ms in the 2500ms bucket.
	if !strings.Contains(body, `iterion_node_duration_ms_bucket{node_id="agent_b",le="500"} 1`) {
		t.Errorf("expected le=500 bucket=1, body:\n%s", body)
	}
}

func TestExporter_BranchGaugeFollowsBranchEvents(t *testing.T) {
	exp := NewPrometheusExporter("run-1", nil)
	observe := exp.EventObserver()

	observe(store.Event{Type: store.EventBranchStarted, BranchID: "b1"})
	observe(store.Event{Type: store.EventBranchStarted, BranchID: "b2"})

	body := scrape(t, exp)
	if !strings.Contains(body, `iterion_parallel_branches 2`) {
		t.Errorf("expected gauge=2 after two branch_started, body:\n%s", body)
	}

	observe(store.Event{Type: store.EventBranchFinished, BranchID: "b1"})
	body = scrape(t, exp)
	if !strings.Contains(body, `iterion_parallel_branches 1`) {
		t.Errorf("expected gauge=1 after one branch_finished, body:\n%s", body)
	}

	// Defensive: extra branch_finished should clamp to 0, not go negative.
	observe(store.Event{Type: store.EventBranchFinished, BranchID: "b2"})
	observe(store.Event{Type: store.EventBranchFinished, BranchID: "b3"})
	body = scrape(t, exp)
	if !strings.Contains(body, `iterion_parallel_branches 0`) {
		t.Errorf("expected gauge clamped to 0 on negative, body:\n%s", body)
	}
}

func TestChainHooksComposeEventHooks(t *testing.T) {
	calls := map[string]int{}
	a := model.EventHooks{
		OnLLMRequest: func(string, model.LLMRequestInfo) { calls["a"]++ },
	}
	b := model.EventHooks{
		OnLLMRequest:   func(string, model.LLMRequestInfo) { calls["b"]++ },
		OnNodeFinished: func(string, map[string]interface{}) { calls["b_node"]++ },
	}
	merged := model.ChainHooks(a, b)
	merged.OnLLMRequest("n", model.LLMRequestInfo{})
	if merged.OnNodeFinished == nil {
		t.Fatal("expected OnNodeFinished to be inherited from b")
	}
	merged.OnNodeFinished("n", nil)
	if calls["a"] != 1 || calls["b"] != 1 || calls["b_node"] != 1 {
		t.Errorf("unexpected call counts: %+v", calls)
	}
}
