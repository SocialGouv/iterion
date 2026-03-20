package e2e

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
	"github.com/zendev-sh/goai/provider/anthropic"
	"github.com/zendev-sh/goai/provider/openai"

	"github.com/iterion-ai/iterion/benchmark"
	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/model"
	"github.com/iterion-ai/iterion/runtime"
	"github.com/iterion-ai/iterion/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// loadDotEnv reads a .env file from the project root and sets each KEY=VALUE
// pair via t.Setenv (automatically cleaned up after the test). Silently
// returns if the file does not exist.
func loadDotEnv(t *testing.T) {
	t.Helper()
	path := filepath.Join("..", ".env")
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		t.Setenv(strings.TrimSpace(k), strings.TrimSpace(v))
	}
}

// requireLiveKeys loads .env and ensures both API keys are present.
// Skips the test if either key is missing.
func requireLiveKeys(t *testing.T) {
	t.Helper()
	loadDotEnv(t)
	if os.Getenv("OPENAI_API_KEY") == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY or ANTHROPIC_API_KEY not set — skipping live test")
	}
}

// resolveModelRefs replaces ${VAR_NAME} model references in compiled IR nodes
// with actual "provider/model-id" specs. The IR compiler stores these
// references verbatim; they must be resolved before execution.
func resolveModelRefs(t *testing.T, wf *ir.Workflow, mapping map[string]string) {
	t.Helper()
	for _, node := range wf.Nodes {
		if node.Model == "" {
			continue
		}
		if strings.HasPrefix(node.Model, "${") && strings.HasSuffix(node.Model, "}") {
			varName := node.Model[2 : len(node.Model)-1]
			spec, ok := mapping[varName]
			if !ok {
				t.Fatalf("resolveModelRefs: no mapping for %q (node %s)", varName, node.ID)
			}
			node.Model = spec
		}
	}
}

// liveRegistry creates a model.Registry with real OpenAI and Anthropic
// provider factories. Both providers read API keys from the environment.
func liveRegistry() *model.Registry {
	reg := model.NewRegistry()
	reg.Register("openai", func(modelID string) (provider.LanguageModel, error) {
		return openai.Chat(modelID), nil
	})
	reg.Register("anthropic", func(modelID string) (provider.LanguageModel, error) {
		return anthropic.Chat(modelID), nil
	})
	return reg
}

// ---------------------------------------------------------------------------
// Live E2E test
// ---------------------------------------------------------------------------

// TestLive_DualParallel_RealModels executes the pr_refine_dual_model_parallel
// workflow with real OpenAI (GPT 5.4) and Anthropic (Claude Opus 4.6) API
// calls. Tools are stripped (no real workspace); the test validates the full
// pipeline: parsing, compilation, parallel execution, join, judge verdicts,
// event emission, and metrics collection.
//
// Requires OPENAI_API_KEY and ANTHROPIC_API_KEY in the environment or .env.
// Automatically skipped when keys are absent or in -short mode.
func TestLive_DualParallel_RealModels(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireLiveKeys(t)

	// Compile the reference workflow with tools stripped.
	wf := compileFixtureStubSafe(t, "pr_refine_dual_model_parallel.iter")

	// Map model variables to real provider specs.
	resolveModelRefs(t, wf, map[string]string{
		"CONTEXT_MODEL":     "anthropic/claude-opus-4-6",
		"CLAUDE_MODEL":      "anthropic/claude-opus-4-6",
		"GPT_MODEL":         "openai/gpt-5.4",
		"ACT_MODEL":         "anthropic/claude-opus-4-6",
		"FINAL_MERGE_MODEL": "anthropic/claude-opus-4-6",
		"FINAL_JUDGE_MODEL": "anthropic/claude-opus-4-6",
	})

	// Create real executor with observability hooks.
	reg := liveRegistry()
	executor := model.NewGoaiExecutor(reg, wf,
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
			OnLLMRetry: func(nodeID string, info model.RetryInfo) {
				t.Logf("[LLM] retry    node=%-30s attempt=%d status=%d delay=%s err=%v",
					nodeID, info.Attempt, info.StatusCode, info.Delay.Round(time.Millisecond), info.Error)
			},
		}),
	)

	// Set workflow variables for prompt template resolution.
	executor.SetVars(map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware including token validation, session management, and error handling.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity. Ensure tests are deterministic and do not depend on external services.",
		"final_review_rules": "Verify all review findings have been addressed. Check that no regressions were introduced.",
	})

	// Create store and engine.
	s := tmpStore(t)
	eng := runtime.New(wf, s, executor)

	// Run with a generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	runID := "live-dual-parallel"
	inputs := map[string]interface{}{
		"pr_title":           "test: add unit tests for auth middleware",
		"pr_description":     "This PR adds comprehensive unit tests for the authentication middleware including token validation, session management, and error handling.",
		"base_ref":           "origin/main",
		"head_ref":           "HEAD",
		"review_rules":       "Check for test coverage, error handling, and code clarity. Ensure tests are deterministic and do not depend on external services.",
		"final_review_rules": "Verify all review findings have been addressed. Check that no regressions were introduced.",
	}

	t.Log("Starting live dual-parallel workflow run...")
	start := time.Now()
	err := eng.Run(ctx, runID, inputs)
	elapsed := time.Since(start)
	t.Logf("Run completed in %s", elapsed.Round(time.Second))

	// --- Assertions ---
	// The run may finish successfully or fail due to budget/loop limits.
	// Both are acceptable for a live test. Infrastructure errors are not.

	if err != nil {
		// Budget exceeded or loop exhausted are acceptable outcomes.
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

	// Verify both providers were called.
	finishedNodes := eventNodeIDs(events, store.EventNodeFinished)
	claudeCalled := false
	gptCalled := false
	for _, id := range finishedNodes {
		if strings.HasPrefix(id, "claude_") {
			claudeCalled = true
		}
		if strings.HasPrefix(id, "gpt_") {
			gptCalled = true
		}
	}
	if !claudeCalled {
		t.Error("No claude_* node finished — Anthropic provider may not have been called")
	}
	if !gptCalled {
		t.Error("No gpt_* node finished — OpenAI provider may not have been called")
	}

	// Verify metrics.
	metrics, mErr := benchmark.CollectMetrics(s, runID, "live-dual-parallel", "")
	if mErr != nil {
		t.Fatalf("Failed to collect metrics: %v", mErr)
	}

	t.Logf("Metrics: tokens=%d cost=$%.4f model_calls=%d iterations=%d duration=%s",
		metrics.TotalTokens, metrics.TotalCostUSD, metrics.ModelCalls, metrics.Iterations, metrics.DurationStr)

	if metrics.TotalTokens <= 0 {
		t.Error("TotalTokens should be > 0")
	}
	if metrics.ModelCalls <= 0 && metrics.Iterations <= 0 {
		t.Error("Expected at least one model call or iteration")
	}
}
