//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_RouterLLM exercises the LLM router mode via
// e2e/testdata/llm_router_task_dispatch.bot: an entry agent feeds a
// `router (mode: llm)` that classifies the task and dispatches to either
// simple_agent or complex_agent. The feature works when the router fires
// and routes to exactly one downstream agent.
//
// Requires: ANTHROPIC_API_KEY (the bot's model resolves to the configured
// default). Expected: ~3-8 min.
func TestLive_Feat_RouterLLM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-router-llm-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	vars := map[string]interface{}{"default_model": "anthropic/claude-sonnet-4-6"}
	inputs := map[string]interface{}{
		"task": "Design and migrate the entire authentication subsystem to OAuth2 with a phased rollout, backward-compatibility shims, and a rollback plan.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-router-llm",
		botFile:      "llm_router_task_dispatch.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       inputs,
		timeout:      8 * time.Minute,
	})

	assertNodesFinished(t, res.events, "entry_agent", "task_router")
	routed := countFinished(res.events, "simple_agent") + countFinished(res.events, "complex_agent")
	if routed == 0 {
		t.Errorf("expected the llm router to dispatch to simple_agent or complex_agent (neither fired)")
	} else {
		t.Logf("router dispatched: simple=%d complex=%d", countFinished(res.events, "simple_agent"), countFinished(res.events, "complex_agent"))
	}
}
