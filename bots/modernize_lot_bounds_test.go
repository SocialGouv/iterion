package bots

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The bounds a modernize lot meets when campaign runs it as its run_lot
// subbot (#3181): the wall and the spend cap of the child's own workflow
// budget, the room campaign keeps before it starts one, and the routes its
// one agent may take when the provider it starts on hits its wall.

// measuredGateCommands and measuredGateTimeout are the contract a lot was
// measured on: three gate commands, each walled at gate_timeout_s 14400 by the
// contract, and lot_verify killed by a 4h workflow wall before the third one
// returned a verdict.
const (
	measuredGateCommands = 3
	measuredGateTimeout  = 14400 * time.Second
)

// lotPassCostShare is the spend the cap funds for each agent pass of a lot.
// The cap bounds the lot's model spend, and that spend is upgrade_campaign's —
// modernize's one model node: the gate's commands, its suites and the oracle
// replay are subprocesses that spend no tokens, and reanchor and extend are
// subbots that budget themselves. So the cap follows the passes a lot may
// take, not the wall: the first pass, then one per crossing repair_loop
// permits (max_passes crossings, docs/dsl.md « Counting crossings »). The
// loop guard prices a repair by the pass before it and declines it once the
// spend so far plus that price reaches runtime.BudgetHardThreshold of the
// cap, so n passes of this share are funded only while n times it stays
// under that share of the cap — not the whole cap.
const lotPassCostShare = 26.0

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
// measured three, a repair pass, which replays the agent and the whole gate,
// and a reanchor or extend child, after which lot_verify runs the whole gate
// again. gate_timeout_s reaches 86400 s per command, so the contract sizes
// its gate to the wall (plan-contract.md, `gate_timeout_s`). What the wall
// asks of campaign, which starts the lot, is TestCampaignStartsALotOnlyWhereOneCanFinish's.
func TestModernizeLotWallHoldsTheMeasuredGateCommands(t *testing.T) {
	t.Parallel()
	modernize := compileBot(t, "modernize")
	if modernize == nil {
		t.Fatal("modernize did not compile")
	}

	lot := literalWall(t, "modernize", modernize.Budget)
	if need := measuredGateCommands * measuredGateTimeout; lot < need {
		t.Errorf("modernize's wall is %s: a contract of %d gate commands walled at %s each needs up to %s for those commands alone, and under campaign no launch override or parent budget reaches the child's wall",
			lot, measuredGateCommands, measuredGateTimeout, need)
	}
}

// graced is a run's wall as the engine lets the run reach it: past the wall,
// the exit grace admits a node until the wall times (1 + the ratio), and that
// node's deadline is the graced ceiling. At a deployment's default grace.
func graced(wall time.Duration) time.Duration {
	return time.Duration(float64(wall) * (1 + runtime.DefaultBudgetExitGraceRatio))
}

// longestLot is the longest one modernize lot can hold its parent's sandbox,
// read off the production files: modernize's wall at the exit grace — no
// node of the lot starts past it, and the last one's deadline is that
// ceiling — plus one subbot child of modernize run to ITS graced wall. A
// subbot node takes no budget check before it starts and no node deadline,
// so the gate's route to reanchor or extend can start one at the ceiling;
// the lot_verify it routes back to is then refused, so no second child
// follows.
func longestLot(t *testing.T) time.Duration {
	t.Helper()
	modernize := compileBot(t, "modernize")
	if modernize == nil {
		t.Fatal("modernize did not compile")
	}
	var child time.Duration
	children := 0
	for id, n := range modernize.Nodes {
		sb, ok := n.(*ir.SubbotNode)
		if !ok {
			continue
		}
		children++
		wf := compileBotFile(t, filepath.Join(botsDir(t), "modernize", sb.Source))
		child = max(child, graced(literalWall(t, "modernize's subbot "+id, wf.Budget)))
	}
	if children == 0 {
		t.Fatal("modernize runs no subbot child — the lot's bound below counts one; revisit it")
	}
	return graced(literalWall(t, "modernize", modernize.Budget)) + child
}

// TestCampaignStartsALotOnlyWhereOneCanFinish holds the gate campaign starts
// each modernize lot through to the bound the lot imposes on its parent. The
// child budgets itself from its own .bot and runs in campaign's sandbox, and
// nothing interrupts it once started: no budget check precedes a subbot node
// and none runs inside it. So a lot started late ends where campaign can no
// longer judge it — past runtime.BudgetHardThreshold of the wall the engine
// starts no campaign node, so observe is refused and no handoff is written —
// and on Kubernetes the sandbox pod, whose deadline is the wall plus 30 min,
// is killed under it.
//
// Every edge into the lot comes from one compute gate, on one bool field of
// it; that field's expression, evaluated as the engine evaluates it, admits a
// start only where the longest lot still ends before the threshold — for
// campaign's own wall and for the shorter ones a launch may set — admits the
// first lot under campaign's wall, and never refuses a run with no wall.
func TestCampaignStartsALotOnlyWhereOneCanFinish(t *testing.T) {
	t.Parallel()
	campaign := compileBot(t, "campaign")
	if campaign == nil {
		t.Fatal("campaign did not compile")
	}
	lotID := ""
	for id, n := range campaign.Nodes {
		if sb, ok := n.(*ir.SubbotNode); ok && filepath.Base(filepath.Dir(sb.Source)) == "modernize" {
			lotID = id
		}
	}
	if lotID == "" {
		t.Fatal("campaign runs no modernize subbot — this test's premise (one lot per child run) is gone")
	}
	gateID, field := "", ""
	for _, e := range campaign.Edges {
		if e.To != lotID {
			continue
		}
		if e.Condition == "" || e.Negated || e.Expression != nil {
			t.Fatalf("edge %s -> %s starts a lot on no gate field (condition %q, negated %t, expression %q) — every lot starts through the gate",
				e.From, e.To, e.Condition, e.Negated, e.ExpressionSrc)
		}
		if gateID != "" && (e.From != gateID || e.Condition != field) {
			t.Fatalf("lots start from %s on %q and from %s on %q — one gate, one field, or a start escapes it", gateID, field, e.From, e.Condition)
		}
		gateID, field = e.From, e.Condition
	}
	gate, ok := campaign.Nodes[gateID].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("lots start from %q (%T), not from a compute gate this test can evaluate", gateID, campaign.Nodes[gateID])
	}
	var room *expr.AST
	for _, ce := range gate.Exprs {
		if ce.Key == field {
			room = ce.AST
		}
	}
	if room == nil {
		t.Fatalf("gate %s computes no %q", gateID, field)
	}
	admits := func(elapsed, wall time.Duration) bool {
		t.Helper()
		ok, err := room.EvalBool(&expr.Context{
			Run: func(path []string) any {
				switch strings.Join(path, ".") {
				case "elapsed_seconds":
					return elapsed.Seconds()
				case "max_duration_seconds":
					return wall.Seconds()
				}
				return nil
			},
			Vars: func(path []string) any {
				if len(path) != 1 {
					return nil
				}
				return intVarDefault(t, "campaign", campaign, path[0])
			},
		})
		if err != nil {
			t.Fatalf("evaluating %s.%s at %s of %s: %v", gateID, field, elapsed, wall, err)
		}
		return ok
	}

	lot := longestLot(t)
	declared := literalWall(t, "campaign", campaign.Budget)
	if !admits(0, declared) {
		t.Errorf("%s refuses the first lot under campaign's own %s wall — the campaign hands off having run nothing", gateID, declared)
	}
	if !admits(1000*time.Hour, 0) {
		t.Errorf("%s refuses a lot on a run with no wall (max_duration 0, unbounded) — no pod deadline and no budget threshold stand in its way", gateID)
	}
	for _, wall := range []time.Duration{declared, 30 * time.Hour, 20 * time.Hour, lot} {
		limit := time.Duration(runtime.BudgetHardThreshold * float64(wall))
		for elapsed := time.Duration(0); elapsed <= wall; elapsed += 30 * time.Second {
			if !admits(elapsed, wall) {
				continue
			}
			if end := elapsed + lot; end >= limit {
				t.Errorf("under a %s wall, %s starts a lot at %s: one that runs %s (modernize's wall and the exit grace, then one child) ends at %s, at or past %s (%.0f%% of the wall), from where campaign starts no node — observe is refused, no handoff is written, and the sandbox pod dies at the wall plus 30 min",
					wall, gateID, elapsed, lot, end, limit, runtime.BudgetHardThreshold*100)
				break
			}
		}
	}
}

// TestModernizeLotCostCapFundsTheForwardedPasses holds the child's spend cap
// to the agent passes campaign asks for (lot_max_passes, forwarded as
// modernize's max_passes): the first pass and one repair pass per red gate the
// loop permits, each costing lotPassCostShare, all funded under the loop
// guard's threshold — and under campaign no launch override raises the
// child's cap. A direct launch reads modernize's own max_passes, held the same
// way.
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
		need := float64(agentPasses) * lotPassCostShare
		if funded := runtime.BudgetHardThreshold * got; need >= funded {
			t.Errorf("modernize's cost cap is %.2f $: %d agent passes (the first, then the %d repairs %s = %d permits) of %.2f $ each reach %.2f $, at or past the %.2f $ (%.0f%% of the cap) where the loop guard declines the last repair — the lot exits with a repair it was given never run",
				got, agentPasses, passes.count, passes.who, passes.count, lotPassCostShare, need, funded, runtime.BudgetHardThreshold*100)
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
