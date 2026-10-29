package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	clawtools "github.com/SocialGouv/claw-code-go/pkg/api/tools"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// agentCall is a well-formed `agent` tool input: claw's own executor accepts
// it, so a refusal this input earns is the capability's, never a validation
// complaint the model could fix by rewriting its arguments.
var agentCall = json.RawMessage(`{"description":"summarise go interfaces","prompt":"Summarise Go interfaces in two lines","subagent_type":"general-purpose"}`)

// executeAgentTool resolves `agent` on reg and calls it the way the claw tool
// loop does — the registry's own ToolDef.Execute, with the model's raw JSON.
func executeAgentTool(t *testing.T, reg *Registry) (string, error) {
	t.Helper()
	td, err := reg.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	return td.Execute(context.Background(), agentCall)
}

// assertSubagentRefusal fails unless the outcome is the typed capability
// refusal: an error of the shared class, naming claw and the tool, with no
// output the model could mistake for a started subagent.
func assertSubagentRefusal(t *testing.T, out string, err error) {
	t.Helper()
	var unsupported *delegate.ErrCapabilityUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("agent tool returned (%q, %v); want a *delegate.ErrCapabilityUnsupported — "+
			"claw's own executor answers {\"status\":\"running\"} without starting anything", out, err)
	}
	if unsupported.Backend != "claw" {
		t.Errorf("refusal names backend %q, want %q", unsupported.Backend, "claw")
	}
	if !strings.Contains(unsupported.Capability, "agent") {
		t.Errorf("refusal names capability %q, which does not name the `agent` tool", unsupported.Capability)
	}
	if strings.TrimSpace(unsupported.Remedy) == "" {
		t.Error("the refusal carries no remedy, so the model is told what it cannot do and nothing it can")
	}
	// The exact sentence docs/backends.md, docs/ultracode.md and the parity
	// page quote: a doc that quotes a message nobody asserts drifts silently.
	const want = `backend "claw" cannot serve the agent tool (subagents) — `
	if !strings.HasPrefix(unsupported.Error(), want) {
		t.Errorf("Error() = %q, want the sentence the docs quote: %q", unsupported.Error(), want)
	}
	if strings.Contains(out, "running") || strings.Contains(out, "agent_id") {
		t.Errorf("agent tool output = %q; a refusal must not hand the model the stub's start metadata", out)
	}
}

// TestClawAgentToolRefusesWithoutASubagentRunner: with no host runner the
// `agent` tool must refuse in the open, not report a start. The refusal blames
// the host that built the registry, not iterion, whose hosts wire one (#1940).
func TestClawAgentToolRefusesWithoutASubagentRunner(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawSubagents(r, nil); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	out, err := executeAgentTool(t, r)
	assertSubagentRefusal(t, out, err)
}

// TestAHostThatWiresNoRunnerRefusesTheAgentTool takes the shape an embedding
// that forgets builds: RegisterClawAll over a ClawDefaults whose Subagent
// nobody sets. iterion's own hosts DO set it since #1940 — pkg/runview for an
// in-process run, cmd/iterion for the sandboxed one, each guarded in its own
// package — so this is no longer the registry an agent node calls into here;
// it is the parity rule's other half, kept honest for the hosts that wire
// nothing.
func TestAHostThatWiresNoRunnerRefusesTheAgentTool(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterClawAll(reg, ClawDefaults{}); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	out, err := executeAgentTool(t, reg)
	assertSubagentRefusal(t, out, err)
}

// TestTheRefusedAgentToolSaysSoInItsDescription: the description is the FIRST
// surface the model reads, and claw's own promises a spawn that returns a
// task_id and notifies the parent at a later turn. Left as is, the model pays
// a tool call to discover the capability is not there — and a description that
// advertises what the executor refuses is the same lie one step earlier.
func TestTheRefusedAgentToolSaysSoInItsDescription(t *testing.T) {
	r := NewRegistry()
	if err := RegisterClawSubagents(r, nil); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	td, err := r.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	for _, want := range []string{"UNAVAILABLE", "claude_code"} {
		if !strings.Contains(td.Description, want) {
			t.Errorf("agent description = %q, missing %q", td.Description, want)
		}
	}
	// tool_search matches on the description, so the words that make the tool
	// findable have to survive: a model looking for delegation must land on
	// the explanation, not on nothing.
	if !strings.Contains(td.Description, "sub-agent") {
		t.Errorf("agent description = %q; `sub-agent` has to stay for tool_search to surface it", td.Description)
	}
	if strings.Contains(td.Description, "keep working") {
		t.Errorf("agent description = %q; it still promises a background spawn that notifies the parent", td.Description)
	}
}

// TestAWiredAgentToolKeepsClawsOwnDescription: with a runner the capability is
// real, so nothing is rewritten.
func TestAWiredAgentToolKeepsClawsOwnDescription(t *testing.T) {
	r := NewRegistry()
	runner := func(_ context.Context, _ map[string]any) (string, error) { return "ok", nil }
	if err := RegisterClawSubagents(r, runner); err != nil {
		t.Fatalf("RegisterClawSubagents: %v", err)
	}
	td, err := r.Resolve("agent")
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	if td.Description != clawtools.AgentTool().Description {
		t.Errorf("agent description = %q, want claw's own", td.Description)
	}
}

// TestAWiredSubagentRunnerStillServesTheAgentTool keeps the refusal narrow: a
// host that supplies a runner — both of iterion's do, and
// e2e/claw_tool_coverage_live_test.go builds one over the live registry — must
// reach it untouched.
func TestAWiredSubagentRunnerStillServesTheAgentTool(t *testing.T) {
	reg := NewRegistry()
	called := false
	defaults := ClawDefaults{Subagent: func(_ context.Context, in map[string]any) (string, error) {
		called = true
		if in["description"] != "summarise go interfaces" {
			t.Errorf("runner received %v, want the model's own input", in)
		}
		return "child result", nil
	}}
	if err := RegisterClawAll(reg, defaults); err != nil {
		t.Fatalf("RegisterClawAll: %v", err)
	}
	out, err := executeAgentTool(t, reg)
	if err != nil || out != "child result" {
		t.Fatalf("agent tool = (%q, %v); want the runner's own result", out, err)
	}
	if !called {
		t.Error("the wired runner never ran")
	}
}
