//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/liveledger"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// workspaceAgentToken is the payload of the probe file: unguessable on
// purpose, so a model that never read probe-token.txt cannot emit it, and a
// child whose narrowed tool set could not read it cannot report it.
const workspaceAgentToken = "QUOKKA-5517"

// workspaceAgentMarker exists only inside probe-scout.md's body: the marker
// reaching the child's answer proves the file's body was the child
// conversation's system prompt — the discovery half of the proof, which no
// amount of token-reading shows on its own (a generic child holding the
// node's read_file would find the token too).
const workspaceAgentMarker = "SCOUT-REPORT"

// workspaceAgentDescription is probe-scout.md's frontmatter description,
// asserted in the delegation's tool result (workspace_agent_description) —
// the surface a delegating model reads to tell a resolved file-backed agent
// from the generic shape.
const workspaceAgentDescription = "Reads the probe token file and reports the token verbatim"

// TestLive_Feat_ClawWorkspaceAgents is the live witness behind the
// "Named subagents resolve from the workspace" cell of docs/backends.md and
// the subagents row of docs/plugins.md (#1714): a claw node whose model
// calls the `agent` tool with subagent_type "probe-scout" must have
// `<workspace>/.claude/agents/probe-scout.md` resolved through iterion's
// own resolver (os.Root at the node's workspace, 256 KiB ceiling), run as
// the child conversation's system prompt and narrowed tool set, and report
// the resolution back in the tool result.
//
// What it asserts, end to end:
//
//  1. DISCOVERY + OFFERED — the delegation's tool result carries
//     workspace_agent: probe-scout and the file's frontmatter description;
//     no tool error (the typed "unknown/unwired" refusal shape) occurred.
//  2. THE CHILD RAN THE FILE — the child's answer opens with the marker
//     only the file's body knows, and reports the unguessable token only
//     readable through the narrowed `tools: [Read]` → read_file set (child
//     tool events ride the run log under a subagent:<id> node).
//  3. END-TO-END — the coordinator's node output carries the token.
//  4. NARROWING HOLDS (counter-example) — blind_check delegates to
//     blind-scribe, whose file declares tools: [] while the node itself
//     holds no read_file: nothing in the run can reach the token, so no
//     blind_check surface may contain it, yet the resolution is still
//     reported (workspace_agent: blind-scribe) and the node still finishes.
//
// The deterministic half (resolver, containment, ceilings, refusals) lives
// in pkg/backend/model/workspace_agents_test.go; the backend wiring the
// live run exercises is pinned credential-free by
// TestTheBackendLoopHandsTheSubagentRunnerTheNodesWorkspace.
//
// Requires ANTHROPIC_API_KEY (both nodes and their children run
// claude-haiku-4-5). Expected: ~2-3 min, ~$0.02-0.05.
func TestLive_Feat_ClawWorkspaceAgents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	// This test builds its own executor (runBotLive's newLiveExecutor wires
	// no subagent runner — RegisterClawSubagents without one is the UNWIRED
	// witness of TestLive_Lite_ClawSubagents), so it hooks the ledger itself.
	tracker := liveledger.Track(t)
	tracker.SetStartedAt(time.Now().UTC())

	wf := compileFixture(t, "feat_claw_workspace_agents.bot")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-workspace-agents-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// The contributions under test, COMMITTED: the IR default `worktree:
	// auto` executes the run in a fresh checkout of HEAD, so an untracked
	// agent file never reaches the workspace the node's resolver reads.
	// (live_feat_workspace_commands_test.go paid for that lesson — a green
	// on an untracked file would have proven a path resolution, not the
	// feature.)
	writeWorkspaceFiles(t, workspaceDir, map[string]string{
		".claude/agents/probe-scout.md": "---\n" +
			"description: " + workspaceAgentDescription + "\n" +
			"tools: [Read]\n" +
			"---\n" +
			"You are probe-scout, a read-only scout. Begin every answer with \"" + workspaceAgentMarker + ":\"." +
			" When asked for the probe token, read the file probe-token.txt and quote the token verbatim.\n",
		".claude/agents/blind-scribe.md": "---\n" +
			"description: A scribe holding no tools at all\n" +
			"tools: []\n" +
			"---\n" +
			"You are blind-scribe. Answer from your own knowledge alone; you hold no tools.\n",
		"probe-token.txt": "The probe token is " + workspaceAgentToken + ".\n",
	})
	gitCommitAll(t, workspaceDir, "feat: contribute two probe agents and a token file")

	storeDir := resolveLiveStoreDir(t, workspaceDir)
	s, storeErr := store.New(storeDir)
	if storeErr != nil {
		t.Fatalf("store.New: %v", storeErr)
	}
	runID := uniqueRunID("live-feat-claw-workspace-agents")

	if err := mcp.PrepareWorkflow(wf, workspaceDir, workspaceDir); err != nil {
		t.Fatalf("mcp.PrepareWorkflow: %v", err)
	}

	logger := prepareLiveRunLog(t, storeDir, runID)
	reg := model.NewRegistry()
	hooks := model.NewStoreEventHooks(context.Background(), s, runID, logger, nil, nil)
	backendReg := delegate.DefaultRegistry(logger)
	backendReg.Register(delegate.BackendClaw, model.NewClawBackend(reg, hooks, model.RetryPolicy{}))

	// The production wiring, as pkg/runview and cmd/iterion build it: the
	// full claw tool set PLUS a real subagent runner — the shape whose
	// absence TestLive_Lite_ClawSubagents proves refuses. The runner names
	// no default model: the child's model is the node's, as in production.
	toolReg := tool.NewRegistry()
	clawDefaults := tool.ClawDefaults{
		Workspace: workspaceDir,
		Subagent:  model.NewSubagentRunner(reg, toolReg, hooks, nil, ""),
	}
	if err := tool.RegisterClawAll(toolReg, clawDefaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}

	executor := model.NewClawExecutor(reg, wf,
		model.WithBackendRegistry(backendReg),
		model.WithToolRegistry(toolReg),
		model.WithWorkDir(workspaceDir),
		model.WithEventHooks(hooks),
	)
	defer executor.Close()

	eng := runtime.New(wf, s, executor,
		runtime.WithLogger(logger),
		runtime.WithWorkDir(workspaceDir),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Logf("[live] starting %s (workspace %s)…", runID, workspaceDir)
	start := time.Now()
	runErr := eng.Run(ctx, runID, nil)
	t.Logf("[live] finished in %s", time.Since(start).Round(time.Second))

	// A dead credential is a missing prerequisite, not a failing feature.
	if why := deadCredentialReason(runErr); why != "" {
		t.Skipf("live prerequisite unavailable — %s (re-authenticate, then re-run)", why)
	}
	if runErr != nil {
		t.Fatalf("Unexpected run error: %v", runErr)
	}

	r, loadErr := s.LoadRun(context.Background(), runID)
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("Expected run status 'finished', got %q", r.Status)
	}
	events, evtErr := s.LoadEvents(context.Background(), runID)
	if evtErr != nil {
		t.Fatalf("LoadEvents: %v", evtErr)
	}
	writeLiveTestReport(t, runID, workspaceDir, storeDir, s, events)
	feedLedgerCost(t, tracker, s, runID)

	assertNodesFinished(t, events, "coordinator", "blind_check")

	// agentCallResult parses the recorded output of the node's `agent` tool
	// call. The store hooks emit tool_called with the tool's own JSON, so
	// the event IS the delegation's result document.
	agentCallResult := func(nodeID string) map[string]any {
		t.Helper()
		var out map[string]any
		for _, evt := range events {
			if evt.NodeID != nodeID || evt.Data == nil {
				continue
			}
			if name, _ := evt.Data["tool"].(string); name != "agent" {
				continue
			}
			switch evt.Type {
			case store.EventToolError:
				msg, _ := evt.Data["error"].(string)
				t.Errorf("%s: the agent call errored — no typed refusal is expected on the wired host: %s", nodeID, msg)
			case store.EventToolCalled:
				raw, _ := evt.Data["output"].(string)
				parsed := map[string]any{}
				if jErr := json.Unmarshal([]byte(raw), &parsed); jErr != nil {
					t.Errorf("%s: the agent tool result is not JSON: %v (%q)", nodeID, jErr, raw)
					continue
				}
				out = parsed
			}
		}
		if out == nil {
			t.Errorf("%s: no successful agent tool call recorded — the delegation never happened", nodeID)
		}
		return out
	}

	// 1+2. The named agent was discovered, offered back (description), and
	// its body drove a child that really read the file.
	res1 := agentCallResult("coordinator")
	if res1 != nil {
		if got, _ := res1["workspace_agent"].(string); got != "probe-scout" {
			t.Errorf("workspace_agent = %q, want probe-scout — the file-backed resolution was not reported", got)
		}
		if got, _ := res1["workspace_agent_description"].(string); got != workspaceAgentDescription {
			t.Errorf("workspace_agent_description = %q, want the frontmatter's %q", got, workspaceAgentDescription)
		}
		if got, _ := res1["status"].(string); got != "completed" {
			t.Errorf("status = %q, want completed — claw's never-started stub reads running", got)
		}
		childText, _ := res1["text"].(string)
		t.Logf("probe-scout answered: %q", childText)
		if !strings.Contains(childText, workspaceAgentMarker) {
			t.Errorf("the child's answer lacks %q — the agent file's body was not its system prompt (discovery broken)", workspaceAgentMarker)
		}
		if !strings.Contains(childText, workspaceAgentToken) {
			t.Errorf("the child's answer lacks %s — it never read probe-token.txt through its narrowed tools", workspaceAgentToken)
		}
	}

	// The child turn rode the run's own event stream under a subagent:<id>
	// node — the observable trace a reviewer reads.
	childEvents := 0
	for _, evt := range events {
		if strings.HasPrefix(evt.NodeID, "subagent:") {
			childEvents++
		}
	}
	if childEvents == 0 {
		t.Error("no subagent:<id> events in the run log — the child conversation executed outside the recorded stream")
	}

	// 3. End-to-end: the workflow output carries the token.
	out1, ok := lastNodeOutput(events, "coordinator")
	if !ok {
		t.Error("coordinator produced no output")
	} else if got, _ := out1["token"].(string); !strings.Contains(got, workspaceAgentToken) {
		t.Errorf("coordinator.token = %q — the token never reached the workflow output", got)
	}

	// 4. Counter-example: blind-scribe's `tools: []` narrows its child to
	// nothing, and blind_check holds no read_file itself, so NO conversation
	// in the run can name the token — while the resolution is still reported.
	res2 := agentCallResult("blind_check")
	if res2 != nil {
		if got, _ := res2["workspace_agent"].(string); got != "blind-scribe" {
			t.Errorf("blind_check's workspace_agent = %q, want blind-scribe", got)
		}
		childText, _ := res2["text"].(string)
		t.Logf("blind-scribe answered: %q", childText)
		if strings.Contains(childText, workspaceAgentToken) {
			t.Errorf("a tool-less child reported %s — the frontmatter narrowing did not hold", workspaceAgentToken)
		}
	}
	if out2, ok := lastNodeOutput(events, "blind_check"); !ok {
		t.Error("blind_check produced no output")
	} else {
		if got, _ := out2["token"].(string); strings.Contains(got, workspaceAgentToken) {
			t.Errorf("blind_check.token = %q — the token escaped a run where no tool could read it", got)
		}
	}

	logRunRecap(t, events)
}
