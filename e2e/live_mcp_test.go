package e2e

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	goai "github.com/zendev-sh/goai"

	"github.com/iterion-ai/iterion/benchmark"
	"github.com/iterion-ai/iterion/delegate"
	"github.com/iterion-ai/iterion/model"
	"github.com/iterion-ai/iterion/runtime"
	"github.com/iterion-ai/iterion/store"
)

// requireCLI skips the test if the given CLI binary is not found in PATH.
func requireCLI(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s CLI not found in PATH — skipping MCP delegation test", name)
	}
}

// TestLive_DualParallel_MCPDelegation executes the pr_refine_dual_model_parallel_mcp
// workflow with real claude-code and codex CLI delegation.
//
// Requires:
//   - `claude` and `codex` CLIs installed and in PATH
//   - ANTHROPIC_API_KEY and OPENAI_API_KEY in the environment or .env
//
// Automatically skipped when CLIs are absent or in -short mode.
func TestLive_DualParallel_MCPDelegation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireLiveKeys(t)
	requireCLI(t, "claude")
	requireCLI(t, "codex")

	// Compile the MCP variant workflow.
	// Use compileFixture (not StubSafe) since delegates handle their own tools.
	wf := compileFixture(t, "pr_refine_dual_model_parallel_mcp.iter")

	// No model resolution needed — delegated nodes don't use model refs.

	// Create executor with delegate registry and observability hooks.
	reg := liveRegistry() // still needed for potential non-delegated nodes
	delegateReg := delegate.DefaultRegistry()

	executor := model.NewGoaiExecutor(reg, wf,
		model.WithDelegateRegistry(delegateReg),
		model.WithRetryPolicy(model.RetryPolicy{
			MaxAttempts: 3,
			BackoffBase: 2 * time.Second,
		}),
		model.WithEventHooks(model.EventHooks{
			OnLLMRequest: func(nodeID string, info goai.RequestInfo) {
				t.Logf("[LLM] request  node=%-30s model=%s", nodeID, info.Model)
			},
			OnLLMResponse: func(nodeID string, info goai.ResponseInfo) {
				tokens := info.Usage.InputTokens + info.Usage.OutputTokens
				t.Logf("[LLM] response node=%-30s tokens=%d latency=%s", nodeID, tokens, info.Latency.Round(time.Millisecond))
			},
		}),
	)

	// Set workflow variables.
	executor.SetVars(map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity.",
		"final_review_rules": "Verify all review findings have been addressed.",
	})

	// Create store and engine.
	s := tmpStore(t)
	eng := runtime.New(wf, s, executor)

	// Run with a generous timeout (delegation is slower than direct API).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	runID := "live-dual-parallel-mcp"
	inputs := map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity.",
		"final_review_rules": "Verify all review findings have been addressed.",
	}

	t.Log("Starting live dual-parallel MCP delegation workflow run...")
	start := time.Now()
	err := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// --- Assertions ---
	if err != nil {
		if errors.Is(err, runtime.ErrBudgetExceeded) {
			t.Logf("Run ended with budget exceeded (acceptable): %v", err)
		} else if errors.Is(err, runtime.ErrRunCancelled) {
			t.Fatalf("Run was cancelled (timeout?): %v", err)
		} else {
			t.Fatalf("Unexpected run error: %v", err)
		}
	}

	// Load run metadata.
	r, loadErr := s.LoadRun(runID)
	if loadErr != nil {
		t.Fatalf("Failed to load run: %v", loadErr)
	}
	t.Logf("Run status: %s", r.Status)

	if r.Status != store.RunStatusFinished && r.Status != store.RunStatusFailed {
		t.Errorf("Unexpected run status: %s (expected finished or failed)", r.Status)
	}

	// Load events.
	events, evtErr := s.LoadEvents(runID)
	if evtErr != nil {
		t.Fatalf("Failed to load events: %v", evtErr)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Error("Missing run_started event")
	}

	// Verify metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-dual-parallel-mcp", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls, metrics.Iterations, metrics.DurationStr)
}
