package model

// The resolver half of #1714 (workspace_agents_test.go) proves
// LookupWorkspaceAgent and the runner read it; THIS witness pins the last
// link: the backend's own loop hands the runner the node's workspace
// (ParentLoop.WorkDir, filled where claw_backend arms its loop). A train
// repair once restored claw_backend.go from a branch that predated the
// wiring and dropped the one field, leaving every production delegation
// resolving no file while every deterministic test stayed green — the
// resolver tests build ParentLoop by hand, so nothing below Execute ever
// saw the gap. Driving the REAL ClawBackend.Execute is what reddens on
// that mutation.

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// TestTheBackendLoopHandsTheSubagentRunnerTheNodesWorkspace drives one
// delegation through the real backend: the node asks for `subagent_type:
// auditor`, the workspace contributes .claude/agents/auditor.md, and the
// child conversation must run on the file's BODY as its system prompt while
// the tool result the parent reads reports the resolved agent. Both facts
// exist only when task.WorkDir reached ParentLoop.
func TestTheBackendLoopHandsTheSubagentRunnerTheNodesWorkspace(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	const agentBody = "AUDIT EVERYTHING. Report findings only."
	writeWorkspaceAgent(t, ws, "auditor", "---\ndescription: Audits a change\n---\n"+agentBody+"\n")

	mock := newMockClient(
		toolUseEvents("tu_1", "agent", `{"description":"audit","prompt":"Audit it","subagent_type":"auditor"}`, 10, 5),
		textEvents("the child's own answer", 10, 5),
		textEvents("the node's answer", 10, 5),
	)
	reg := NewRegistry()
	reg.Register("openai", func(string) (api.APIClient, error) { return mock, nil })

	runner := NewSubagentRunner(reg, tool.NewRegistry(), EventHooks{}, nil, "")
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:       "delegator",
		Model:        spendParentModel,
		UserPrompt:   "Delegate the audit.",
		WorkDir:      ws,
		HasTools:     true,
		ToolDefs:     []delegate.ToolDef{agentToolDefFor(t, runner)},
		ToolMaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, _ := res.Output["text"].(string); !strings.Contains(got, "the node's answer") {
		t.Fatalf("the node did not finish on its own answer: %v", res.Output)
	}

	calls := mock.getCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 (parent, child, parent)", len(calls))
	}
	if calls[1].System != agentBody {
		t.Errorf("the child's system prompt is %q, want the agent file's body — the workspace never reached the runner (ParentLoop.WorkDir)", calls[1].System)
	}
	// The parent's second turn carries the delegation's tool_result: the
	// resolved agent must be reported there, or the model cannot tell a
	// file-backed child from the generic shape.
	var toolResult string
	for _, m := range calls[2].Messages {
		for _, block := range m.Content {
			if block.Type == "tool_result" {
				toolResult += charToolResultText(t, block)
			}
		}
	}
	if !strings.Contains(toolResult, `"workspace_agent": "auditor"`) {
		t.Errorf("the parent reads no resolved-agent report in its tool_result: %q", toolResult)
	}
}
