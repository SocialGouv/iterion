package bots

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The bounds a modernize lot meets when campaign runs it as its run_lot
// subbot (#3181): the wall of the child's own workflow budget, and the routes
// its one agent may take when the provider it starts on hits its wall.

// measuredGateCommands and measuredGateTimeout are the contract a lot was
// measured on: three gate commands, each walled at gate_timeout_s 14400 by the
// contract, and lot_verify killed by a 4h workflow wall before the third one
// returned a verdict.
const (
	measuredGateCommands = 3
	measuredGateTimeout  = 14400 * time.Second
)

// lotWallCostFloor is the cost cap the 12h wall carries: a lot given the time
// is not stopped by a cap sized for a third of it.
const lotWallCostFloor = 150.0

// literalWall parses a workflow's max_duration as the literal it must be: a
// `${…}` dial reads the engine's environment (ADR-132) and a typo is a run
// with no wall at all, so neither is a wall this test can certify.
func literalWall(t *testing.T, bot string, b *ir.Budget) time.Duration {
	t.Helper()
	if b == nil || b.MaxDuration == "" {
		t.Fatalf("%s declares no max_duration — its lots would run without a wall", bot)
	}
	d, err := time.ParseDuration(b.MaxDuration)
	if err != nil {
		t.Fatalf("%s max_duration %q is not a literal duration (%v) — a dial answers from the environment and a typo enforces nothing", bot, b.MaxDuration, err)
	}
	return d
}

// TestModernizeLotWallHoldsTheMeasuredGate holds the child's wall above what a
// contract measured to need: a subbot budgets itself from its own .bot, so
// under campaign no launch override and no parent budget can raise it, and the
// wall that kills lot_verify mid-gate leaves the lot with no verdict at all.
// The parent's wall clock does contain its children, so it must hold at
// least one whole lot, or the child's wall is one no lot can reach.
func TestModernizeLotWallHoldsTheMeasuredGate(t *testing.T) {
	t.Parallel()
	modernize := compileBot(t, "modernize")
	campaign := compileBot(t, "campaign")
	if modernize == nil || campaign == nil {
		t.Fatal("modernize or campaign did not compile")
	}

	lot := literalWall(t, "modernize", modernize.Budget)
	if need := measuredGateCommands * measuredGateTimeout; lot < need {
		t.Errorf("modernize's wall is %s: a contract of %d gate commands walled at %s each needs up to %s for its gate alone, and campaign's run_lot child has no other wall to raise",
			lot, measuredGateCommands, measuredGateTimeout, need)
	}
	if got := modernize.Budget.MaxCostUSD; got < lotWallCostFloor {
		t.Errorf("modernize's cost cap is %.2f $, below %.2f $: a lot given %s is stopped by a cap sized for a shorter wall", got, lotWallCostFloor, lot)
	}

	parent := literalWall(t, "campaign", campaign.Budget)
	if parent < lot {
		t.Errorf("campaign's wall (%s) is shorter than one modernize lot's (%s): the parent's clock contains its children, so the child's wall is one no lot can reach", parent, lot)
	}
}

// claudeCodeAgents returns modernize's agent nodes that run on claude_code,
// sorted by id, failing when there is none: the checks below would hold over
// nothing.
func claudeCodeAgents(t *testing.T, wf *ir.Workflow) []ir.LLMNode {
	t.Helper()
	var out []ir.LLMNode
	for _, n := range wf.Nodes {
		llm, ok := n.(ir.LLMNode)
		if !ok || llm.NodeKind() != ir.NodeAgent {
			continue
		}
		if ir.SourceNodeBackendName(llm.GetLLMFields().Backend, wf.DefaultBackend) != delegate.BackendClaudeCode {
			continue
		}
		out = append(out, llm)
	}
	if len(out) == 0 {
		t.Fatal("modernize has no claude_code agent node — the route and surface checks below would hold over nothing")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID() < out[j].NodeID() })
	return out
}

// TestModernizeAgentTakesALaunchRouteOntoClaw drives the launch screen an
// operator's `--fallback` goes through (ir.ApplyRunFallback, whose crossing
// verdict a routing-policy ladder stage and a routing cell share) with a claw
// stage, and requires every claude_code agent of modernize to take it. A node
// with no `tools:` list is refused there: the list means every native tool on
// claude_code and none on claw, so a lot that hits its provider's wall would
// keep no route past it.
func TestModernizeAgentTakesALaunchRouteOntoClaw(t *testing.T) {
	t.Parallel()
	wf := compileBot(t, "modernize")
	if wf == nil {
		t.Fatal("modernize did not compile")
	}
	agents := claudeCodeAgents(t, wf)

	stage := ir.Fallback{Backend: delegate.BackendClaw, Model: "zai/glm-5.3"}
	refusals := ir.ApplyRunFallback(wf, []ir.Fallback{stage}, false, nil, nil)
	for _, r := range refusals {
		t.Errorf("the launch screen refuses a claw stage: %s", r)
	}
	for _, n := range agents {
		var routed bool
		for _, fb := range n.GetFallbacks() {
			if fb.RunStageSet && fb.Backend == delegate.BackendClaw {
				routed = true
			}
		}
		if !routed {
			t.Errorf("agent %q took no claw stage from the launch screen (refusals: %v)", n.NodeID(), refusals)
		}
	}
}

// TestModernizeAgentToolsAreCapabilityNeutral holds the `tools:` list of every
// claude_code agent of modernize to the promise its comment makes, on both
// backends of the parity pair:
//
//   - claude_code: the spawn keeps exactly the native roster it keeps with no
//     declaration — read off delegate.ClaudeCodeToolSurface, the disclosure
//     built from the same projection the spawn's --disallowedTools is. A name
//     missing from the list withholds its tool there.
//   - claw: every name is a claw built-in AND projects onto a claude_code
//     native, so the claw route resolves each one and gains no capability the
//     primary does not have. A claude_code spelling claw does not register
//     (`write`, `edit`, `task`) passes the launch screen and then fails the
//     node at tool resolution, exactly when the run is falling back.
func TestModernizeAgentToolsAreCapabilityNeutral(t *testing.T) {
	t.Parallel()
	wf := compileBot(t, "modernize")
	if wf == nil {
		t.Fatal("modernize did not compile")
	}
	for _, n := range claudeCodeAgents(t, wf) {
		tools := n.GetTools()
		if !toolcatalog.ToolsDeclared(tools) {
			t.Errorf("agent %q declares no tools: list — a claw route is refused on it (TestModernizeAgentTakesALaunchRouteOntoClaw)", n.NodeID())
			continue
		}
		readonly := n.GetLLMFields().Readonly
		ambient := delegate.ClaudeCodeToolSurface(delegate.Task{Readonly: readonly})
		declared := delegate.ClaudeCodeToolSurface(delegate.Task{
			AllowedTools:    tools,
			ToolsDeclared:   true,
			Readonly:        readonly,
			DiagnosticShell: slices.Contains(tools, "diagnostic_shell"),
		})
		if !slices.Equal(declared.Granted, ambient.Granted) {
			var lost []string
			for _, name := range ambient.Granted {
				if !slices.Contains(declared.Granted, name) {
					lost = append(lost, name)
				}
			}
			t.Errorf("agent %q: on claude_code the declared list keeps %v where no declaration keeps %v — it withholds %v, so the list is not capability-neutral",
				n.NodeID(), declared.Granted, ambient.Granted, lost)
		}

		var notClaw, noNative []string
		for _, name := range tools {
			if !toolcatalog.IsBuiltin(name) {
				notClaw = append(notClaw, name)
			}
			one := delegate.ClaudeCodeToolSurface(delegate.Task{AllowedTools: []string{name}, ToolsDeclared: true})
			if len(one.Granted) == 0 {
				noNative = append(noNative, name)
			}
		}
		if len(notClaw) > 0 {
			t.Errorf("agent %q declares %s, which claw does not register — the claw route fails the node at tool resolution; spell each tool as claw's built-in (write_file, file_edit, agent, …)",
				n.NodeID(), quoteNames(notClaw))
		}
		if len(noNative) > 0 {
			t.Errorf("agent %q declares %s, which projects onto no claude_code native — the claw route would hold a capability the primary does not", n.NodeID(), quoteNames(noNative))
		}
	}
}

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}
