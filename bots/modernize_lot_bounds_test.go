package bots

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The bounds a modernize lot meets when campaign runs it as its run_lot
// subbot (#3181): the wall and the spend cap of the child's own workflow
// budget, and the routes its one agent may take when the provider it starts
// on hits its wall.

// measuredGateCommands and measuredGateTimeout are the contract a lot was
// measured on: three gate commands, each walled at gate_timeout_s 14400 by the
// contract, and lot_verify killed by a 4h workflow wall before the third one
// returned a verdict.
const (
	measuredGateCommands = 3
	measuredGateTimeout  = 14400 * time.Second
)

// lotPassCostShare is the spend the cap gives each agent pass of a lot. The
// cap bounds the lot's model spend, and that spend is upgrade_campaign's —
// modernize's one model node: the gate's commands, its suites and the oracle
// replay are subprocesses that spend no tokens, and reanchor and extend are
// subbots that budget themselves. So the cap follows the passes a lot may
// take, not the wall: the first pass, then one per crossing repair_loop
// permits (max_passes crossings, docs/dsl.md « Counting crossings »).
const lotPassCostShare = 30.0

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

// intVarDefault reads an int var's default off a compiled workflow, failing
// when the var is gone or carries no int default: the bound below is computed
// from it.
func intVarDefault(t *testing.T, bot string, wf *ir.Workflow, name string) int64 {
	t.Helper()
	v := wf.Vars[name]
	if v == nil || !v.HasDefault {
		t.Fatalf("%s declares no default for var %q — the bound computed from it has nothing to read", bot, name)
	}
	n, ok := v.Default.(int64)
	if !ok {
		t.Fatalf("%s var %q defaults to %v (%T), not an int", bot, name, v.Default, v.Default)
	}
	return n
}

// TestModernizeLotWallHoldsTheMeasuredGateCommands holds the child's wall at
// or above the measured contract's gate commands run end to end, each to its
// gate_timeout_s: under campaign a subbot budgets itself from its own .bot —
// no launch override and no parent budget reaches it — and a lot_verify the
// wall cuts leaves the lot with no verdict at all.
//
// What this arithmetic does not hold, and no literal can: the agent pass that
// precedes lot_verify inside the same wall, a `suites:` command beyond the
// measured three, and a repair pass, which replays the agent and the whole
// gate. gate_timeout_s reaches 86400 s per command, so the contract sizes its
// gate to the wall (plan-contract.md, `gate_timeout_s`).
//
// Campaign's wall counts its children's time but never interrupts a running
// lot: a subbot node takes no node deadline, and the engine checks the
// parent's budget only before the parent's next node. Under a campaign wall
// shorter than one lot's, a lot that outlasts it runs on to its end, and
// campaign then refuses observe, the node that measures the lot: the lot's
// work is never measured.
func TestModernizeLotWallHoldsTheMeasuredGateCommands(t *testing.T) {
	t.Parallel()
	modernize := compileBot(t, "modernize")
	campaign := compileBot(t, "campaign")
	if modernize == nil || campaign == nil {
		t.Fatal("modernize or campaign did not compile")
	}

	lot := literalWall(t, "modernize", modernize.Budget)
	if need := measuredGateCommands * measuredGateTimeout; lot < need {
		t.Errorf("modernize's wall is %s: a contract of %d gate commands walled at %s each needs up to %s for those commands alone, and under campaign no launch override or parent budget reaches the child's wall",
			lot, measuredGateCommands, measuredGateTimeout, need)
	}

	parent := literalWall(t, "campaign", campaign.Budget)
	if parent < lot {
		t.Errorf("campaign's wall (%s) is shorter than one modernize lot's (%s): the parent's wall counts its children's time but does not interrupt a running lot, so a lot that outlasts it runs on to its end and campaign then refuses observe — that lot's work is never measured", parent, lot)
	}
}

// TestModernizeLotCostCapFundsTheForwardedPasses holds the child's spend cap
// to the agent passes campaign asks for (lot_max_passes, forwarded as
// modernize's max_passes): the first pass and one repair pass per red gate the
// loop permits, each given lotPassCostShare — and under campaign no launch
// override raises the child's cap. A direct launch reads modernize's own
// max_passes, held the same way.
func TestModernizeLotCostCapFundsTheForwardedPasses(t *testing.T) {
	t.Parallel()
	modernize := compileBot(t, "modernize")
	campaign := compileBot(t, "campaign")
	if modernize == nil || campaign == nil {
		t.Fatal("modernize or campaign did not compile")
	}
	if modernize.Budget == nil {
		t.Fatal("modernize declares no budget — its lots would run without a spend cap")
	}
	got := modernize.Budget.MaxCostUSD
	for _, passes := range []struct {
		who   string
		count int64
	}{
		{"campaign's lot_max_passes", intVarDefault(t, "campaign", campaign, "lot_max_passes")},
		{"modernize's max_passes", intVarDefault(t, "modernize", modernize, "max_passes")},
	} {
		agentPasses := 1 + passes.count
		if need := float64(agentPasses) * lotPassCostShare; got < need {
			t.Errorf("modernize's cost cap is %.2f $, below the %.2f $ that %d agent passes (the first, then the %d repairs %s = %d permits) of %.2f $ each need: the cap stops the lot before the repair loop has spent the passes it was given",
				got, need, agentPasses, passes.count, passes.who, passes.count, lotPassCostShare)
		}
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

// clawDeploymentGated names the tools of modernize's list that claw registers
// only where the deployment enables them, each with the switch that does.
// The claw route resolves every other name on any deployment. On one that
// leaves these off it fails the node at tool resolution, naming the tool —
// and the launch screen admits the claw stage either way, since it judges the
// declared list, not the registry of the process that runs the node. The
// node's comment and the 0.7.1 manifest note say so.
var clawDeploymentGated = map[string]string{
	"web_search": "tool.ResolveWebSearchEnabled: ITERION_WEB_SEARCH=on, or a configured search backend (SEARXNG_URL, SEARXNG_ENDPOINT, BRAVE_API_KEY)",
}

// clawUnresolved registers claw's whole tool set the way a host does —
// tool.RegisterClawAll, web_search on or off — and returns the names of
// tools that the registry does not resolve.
func clawUnresolved(t *testing.T, tools []string, webSearch bool) []string {
	t.Helper()
	reg := tool.NewRegistry()
	if err := tool.RegisterClawAll(reg, tool.ClawDefaults{IncludeWebSearch: webSearch}); err != nil {
		t.Fatalf("RegisterClawAll(IncludeWebSearch=%t): %v", webSearch, err)
	}
	var out []string
	for _, name := range tools {
		if _, err := reg.Resolve(name); err != nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestModernizeAgentToolsAreCapabilityNeutral holds the `tools:` list of every
// claude_code agent of modernize to the promise its comment makes, on both
// backends of the parity pair:
//
//   - claude_code: the spawn keeps exactly the native roster it keeps with no
//     declaration — read off delegate.ClaudeCodeToolSurface, the disclosure
//     built from the same projection the spawn's --disallowedTools is. A name
//     missing from the list withholds its tool there.
//   - claw: every name resolves in the registry a host builds
//     (tool.RegisterClawAll) where the deployment enables web search, and
//     exactly the clawDeploymentGated names fail where it does not; every
//     name projects onto a claude_code native, so the claw route gains no
//     capability the primary does not have. A claude_code spelling claw does
//     not register (`write`, `edit`, `task`) passes the launch screen and
//     then fails the node at tool resolution, exactly when the run is
//     falling back.
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

		if notClaw := clawUnresolved(t, tools, true); len(notClaw) > 0 {
			t.Errorf("agent %q declares %s, which claw does not register even with web search enabled — the claw route fails the node at tool resolution; spell each tool as claw's built-in (write_file, file_edit, agent, …)",
				n.NodeID(), quoteNames(notClaw))
		}
		var gated []string
		for name := range clawDeploymentGated {
			if slices.Contains(tools, name) {
				gated = append(gated, name)
			}
		}
		sort.Strings(gated)
		if off := clawUnresolved(t, tools, false); !slices.Equal(off, gated) {
			t.Errorf("agent %q: with web search off, claw leaves %s unresolved where clawDeploymentGated names %s of this list — a name claw registers only on a deployment switch fails the claw route wherever the switch is off; name it there with its switch (and in the node's comment and the manifest note), or drop it from the list",
				n.NodeID(), quoteNames(off), quoteNames(gated))
		}

		var noNative []string
		for _, name := range tools {
			one := delegate.ClaudeCodeToolSurface(delegate.Task{AllowedTools: []string{name}, ToolsDeclared: true})
			if len(one.Granted) == 0 {
				noNative = append(noNative, name)
			}
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
