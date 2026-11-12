package bots

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// The lot the wall was measured on: three gate commands, each walled at
// gate_timeout_s 14400 by the contract, and lot_verify killed by a 4h workflow
// wall (measuredCutWall) while the third one ran — so the agent's pass and the
// first two commands had returned inside those 4h.
const (
	measuredGateCommands = 3
	measuredGateTimeout  = 14400 * time.Second
	measuredCutWall      = 4 * time.Hour
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

// TestModernizeLotWallHoldsTheMeasuredLot holds the child's wall to the lot it
// was raised for, and to what its budget comment, its 0.7.1 note and
// campaign's lot_room_s say of it. Under campaign a subbot budgets itself
// from its own .bot — no launch override and no parent budget reaches it —
// and lot_verify must end before runtime.BudgetHardThreshold of the wall for
// its verdict to land: from there until the wall the engine refuses to start
// a tool or agent node, so neither mark_done, which writes `done`, nor a
// repair pass starts; and a lot_verify the wall cuts leaves no verdict at all.
// The figures those documents give — the wall, its threshold, what is left
// after the measured lot's commands, the Kubernetes pod's deadline and the
// graced ceiling — are the wall's, read here: a wall that moves without them
// fails until they are rewritten.
//
// The measured lot ends its gate commands by measuredCutWall plus one
// gate_timeout_s: everything before its third command returned inside the
// wall that cut that command, and the third has its own wall. The wall holds
// that, with room left for the suites and the oracle. It does not hold three
// commands each run to its gate_timeout_s — those alone reach the threshold,
// before the agent's pass is counted — and the documents say so: a wall that
// does hold them makes that sentence false, and fails here until it is
// rewritten. A repair pass, a reanchor or extend child and the gate replayed
// after it hold under no literal (gate_timeout_s reaches 86400 s per
// command): the contract sizes its gate to the wall (plan-contract.md). What
// the wall asks of campaign, which starts the lot, is
// TestCampaignStartsALotOnlyWhereOneCanFinish's.
func TestModernizeLotWallHoldsTheMeasuredLot(t *testing.T) {
	t.Parallel()
	modernize := compileBot(t, "modernize")
	if modernize == nil {
		t.Fatal("modernize did not compile")
	}

	lot := literalWall(t, "modernize", modernize.Budget)
	limit := time.Duration(runtime.BudgetHardThreshold * float64(lot))
	if ends := measuredCutWall + measuredGateTimeout; ends >= limit {
		t.Errorf("modernize's wall is %s: the measured lot's gate commands end by %s (all before the third inside the %s wall that cut it, then the third's %s), at or past %s (%.0f%% of the wall), from where neither mark_done nor a repair pass starts — the lot this wall was raised for lands no verdict",
			lot, ends, measuredCutWall, measuredGateTimeout, limit, runtime.BudgetHardThreshold*100)
	}
	if walled := measuredGateCommands * measuredGateTimeout; walled < limit {
		t.Errorf("modernize's wall is %s: %d gate commands run to their %s walls (%s) now end before %s (%.0f%% of it), where the budget comment, the 0.7.1 manifest note and plan-contract.md say this wall does not hold them — rewrite those figures, then this bound",
			lot, measuredGateCommands, measuredGateTimeout, walled, limit, runtime.BudgetHardThreshold*100)
	}

	// The pod's deadline is the wall plus the 30 min the Kubernetes driver
	// adds (activeDeadlineFor).
	left := limit - (measuredCutWall + measuredGateTimeout)
	pod, ceiling := lot+30*time.Minute, graced(lot)
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(botsDir(t), rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, doc := range []struct {
		rel   string
		needs []string
	}{
		{"modernize/main.bot", []string{`max_duration: "` + hours(lot) + `"`, "(" + hours(limit) + ")", hours(left) + " remain", "(" + hours(pod) + ")", "(" + hours(ceiling) + ")"}},
		{"modernize/manifest.yaml", []string{"is " + hours(lot), "under " + hours(limit), ", " + hours(pod) + ")", "(" + hours(ceiling) + ")"}},
		{"campaign/main.bot", []string{"modernize's " + hours(lot) + " wall", "(" + hours(ceiling) + ")"}},
		{"campaign/README.md", []string{"modernize's " + hours(lot) + " wall", "(" + hours(ceiling) + ")"}},
	} {
		text := read(doc.rel)
		for _, n := range doc.needs {
			if !strings.Contains(text, n) {
				t.Errorf("%s does not say %q: with a %s wall the threshold is %s, %s remain after the measured lot's commands, the Kubernetes pod dies at %s and the exit grace reaches %s — a document giving other figures sizes contracts and campaigns on a wall that is not the bot's",
					doc.rel, n, hours(lot), hours(limit), hours(left), hours(pod), hours(ceiling))
			}
		}
	}
}

// hours writes a duration the way the documents give one: in hours, with no
// trailing zero ("12h", "10.8h").
func hours(d time.Duration) string {
	return strconv.FormatFloat(d.Hours(), 'f', -1, 64) + "h"
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
// longer judge it — from runtime.BudgetHardThreshold of the wall the engine
// refuses to start a tool node, so observe is refused and no handoff is
// written —
// and on Kubernetes the sandbox pod, whose deadline is the wall plus 30 min,
// is killed under it.
//
// Every edge into the lot comes from one compute gate, on one bool field of
// it; that field's expression, evaluated as the engine evaluates it, admits a
// start only where the longest lot, then observe and the steward outside its
// acts, at their commands' walls (afterLot), still end before the threshold —
// for campaign's own wall and for the shorter ones a launch may set — admits
// the first lot under campaign's wall, and never refuses a run with no wall.
// What the steward's acts and finalize's commands take is theirs to fit: an
// act starts only where it ends the steward's reserve before the threshold
// (TestCampaignStewardDefersAnActTheWallCannotHold), the reserve holds what
// the act does after (TestCampaignStewardReserveHoldsWhatAnActLeaves), and
// finalize writes its handoff before the wall, cutting what would not
// (TestCampaignFinalizeWritesItsHandoffBeforeTheWall). The child judges the
// same predicate where it starts, which a resumed start reaches without this
// gate (TestModernizeJudgesTheCampaignsRoomAsLotRoomDoes).
func TestCampaignStartsALotOnlyWhereOneCanFinish(t *testing.T) {
	t.Parallel()
	g := campaignLotGate(t)
	admits := func(elapsed, wall time.Duration) bool {
		t.Helper()
		v := g.eval(t, g.field, elapsed, wall, 0)
		ok, isBool := v.(bool)
		if !isBool {
			t.Fatalf("%s.%s is %v (%T), not a bool", g.id, g.field, v, v)
		}
		return ok
	}

	lot := longestLot(t)
	after := afterLot(t)
	declared := literalWall(t, "campaign", g.campaign.Budget)
	if !admits(0, declared) {
		t.Errorf("%s refuses the first lot under campaign's own %s wall — the campaign hands off having run nothing", g.id, declared)
	}
	if !admits(1000*time.Hour, 0) {
		t.Errorf("%s refuses a lot on a run with no wall (max_duration 0, unbounded) — no pod deadline and no budget threshold stand in its way", g.id)
	}
	for _, wall := range []time.Duration{declared, 30 * time.Hour, 20 * time.Hour, lot} {
		limit := time.Duration(runtime.BudgetHardThreshold * float64(wall))
		for elapsed := time.Duration(0); elapsed <= wall; elapsed += 30 * time.Second {
			if !admits(elapsed, wall) {
				continue
			}
			if end := elapsed + lot; end+after >= limit {
				t.Errorf("under a %s wall, %s starts a lot at %s: one that runs %s (modernize's wall and the exit grace, then one child) ends at %s, and observe and the steward outside its acts (%s at their walls) take the campaign to %s, at or past %s (%.0f%% of the wall), from where the engine refuses to start a tool node — finalize is refused, no handoff is written, and the sandbox pod dies at the wall plus 30 min",
					wall, g.id, elapsed, lot, end, after, end+after, limit, runtime.BudgetHardThreshold*100)
				break
			}
		}
	}
}

// stewardReserve reads RESERVE_S off campaign's steward script: the time an
// act's start keeps, after its record and its counter-test, for the act's own
// git work, the journal commit, and loop_gate, lot_room and finalize to start
// — before 90% of the wall, from which the engine refuses to start a tool node.
func stewardReserve(t *testing.T) time.Duration {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*RESERVE_S = (\d+)\s*$`).FindStringSubmatch(toolScript(t, "campaign/main.bot", "steward"))
	if m == nil {
		t.Fatal("campaign's steward declares no `RESERVE_S = <seconds>` — the time it keeps for the handoff to start is gone, and nothing bounds what follows a lot")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("steward RESERVE_S %q: %v", m[1], err)
	}
	return time.Duration(n) * time.Second
}

// lotGate is the compute gate every modernize lot of campaign starts through.
type lotGate struct {
	campaign  *ir.Workflow
	id, field string
	node      *ir.ComputeNode
	// set holds the campaign vars a launch sets (`--var name=value`); every
	// other var reads its default.
	set map[string]int64
}

// campaignLotGate finds the gate: every edge into the modernize subbot comes
// from one compute node, on one bool field of it.
func campaignLotGate(t *testing.T) lotGate {
	t.Helper()
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
	return lotGate{campaign: campaign, id: gateID, field: field, node: gate}
}

// eval evaluates one field of the gate as the engine does, at elapsed of a
// wall (0: no wall), after `crossings` crossings of the campaign's loop, with
// g.set over campaign's var defaults.
func (g lotGate) eval(t *testing.T, field string, elapsed, wall time.Duration, crossings int64) any {
	t.Helper()
	var ast *expr.AST
	for _, ce := range g.node.Exprs {
		if ce.Key == field {
			ast = ce.AST
		}
	}
	if ast == nil {
		t.Fatalf("gate %s computes no %q", g.id, field)
	}
	v, err := ast.Eval(&expr.Context{
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
			if v, ok := g.set[path[0]]; ok {
				return v
			}
			return intVarDefault(t, "campaign", g.campaign, path[0])
		},
		Loop: func(path []string) any {
			if len(path) == 2 && path[1] == "iteration" {
				return crossings
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("evaluating %s.%s at %s of %s: %v", g.id, field, elapsed, wall, err)
	}
	return v
}

// TestCampaignSaysWhyItStartsNoLot holds what the campaign tells an operator
// whose wall leaves no room for a lot — a short --max-duration, a deployment
// ceiling under campaign's 48h. The gate's `reason` agrees with its verdict
// at every point the start test probes, and a refusal names whether any lot
// ran, the figures it was judged on and the two levers; the refusal's edge
// carries it to finalize as `ended_by`, and every edge into finalize says why
// the campaign ended — the handoff and its notice open on it
// (TestCampaignFinalizeSaysWhyTheCampaignEnded): a run with no room for its
// first lot hands off a programme of zero lots, and says why.
func TestCampaignSaysWhyItStartsNoLot(t *testing.T) {
	t.Parallel()
	g := campaignLotGate(t)
	room := intVarDefault(t, "campaign", g.campaign, "lot_room_s")
	reason := func(elapsed, wall time.Duration, crossings int64) string {
		t.Helper()
		v := g.eval(t, "reason", elapsed, wall, crossings)
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s.reason is %v (%T), not a string", g.id, v, v)
		}
		return s
	}
	for _, wall := range []time.Duration{0, 48 * time.Hour, 20 * time.Hour, 30 * time.Minute} {
		for elapsed := time.Duration(0); elapsed <= max(wall, time.Hour); elapsed += 10 * time.Minute {
			admitted, _ := g.eval(t, g.field, elapsed, wall, 1).(bool)
			if got := reason(elapsed, wall, 1); (got == "room for a lot") != admitted {
				t.Errorf("at %s of a %s wall, %s.%s is %t and its reason reads %q — the reason restates the verdict, and the two disagree", elapsed, wall, g.id, g.field, admitted, got)
			}
		}
	}

	first := reason(0, 30*time.Minute, 0)
	for _, want := range []string{"NO LOT STARTED", fmt.Sprintf("lot_room_s (%d s", room), "(1620 s of 1800 s)", "`--var lot_room_s=<seconds>`", "`--max-duration`"} {
		if !strings.Contains(first, want) {
			t.Errorf("under a 30m wall the first lot is refused with %q, which does not say %q — the operator gets a handoff of zero lots and no cause (a lever outside a code span loses its <seconds> to a markdown renderer)", first, want)
		}
	}
	if later := reason(40*time.Hour, 48*time.Hour, 3); !strings.Contains(later, "no lot after the 3 this campaign ran") {
		t.Errorf("a refusal after three lots reads %q — it must not claim no lot started", later)
	}

	refusals, intoFinalize := 0, 0
	for _, e := range g.campaign.Edges {
		if e.To != "finalize" {
			continue
		}
		intoFinalize++
		var ended *ir.DataMapping
		for _, m := range e.With {
			if m.Key == "ended_by" {
				ended = m
			}
		}
		if ended == nil || strings.TrimSpace(ended.Raw) == "" {
			t.Errorf("edge %s -> finalize maps no ended_by — the handoff of a campaign ending there does not say why", e.From)
			continue
		}
		if e.From == g.id && e.Condition == g.field && e.Negated {
			refusals++
			if want := "{{outputs." + g.id + ".reason}}"; ended.Raw != want {
				t.Errorf("the refused-lot edge %s -> finalize maps ended_by %q, want %q — the figures and the levers stay in the gate's output", e.From, ended.Raw, want)
			}
		}
	}
	if refusals != 1 || intoFinalize < 4 {
		t.Fatalf("found %d refused-lot edge(s) from %s and %d edge(s) into finalize — want 1 and at least 4: the walk no longer sees the campaign's exits", refusals, g.id, intoFinalize)
	}
}

// forwardedMaxPasses is the max_passes campaign's modernize subbot hands its
// child, read off the node's `with:` mapping: a literal, or one campaign var
// at its default (lot_max_passes). Any other shape is one this bound cannot
// price.
func forwardedMaxPasses(t *testing.T, campaign *ir.Workflow) int64 {
	t.Helper()
	var lot *ir.SubbotNode
	for _, n := range campaign.Nodes {
		if sb, ok := n.(*ir.SubbotNode); ok && filepath.Base(filepath.Dir(sb.Source)) == "modernize" {
			lot = sb
		}
	}
	if lot == nil {
		t.Fatal("campaign runs no modernize subbot — the passes it forwards have no node to be read from")
	}
	for _, m := range lot.With {
		if m.Key != "max_passes" {
			continue
		}
		raw := strings.TrimSpace(m.Raw)
		if v := regexp.MustCompile(`^\{\{\s*vars\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`).FindStringSubmatch(raw); v != nil {
			return intVarDefault(t, "campaign", campaign, v[1])
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("campaign's %s forwards max_passes %q, neither a literal nor one var — the passes a lot is given cannot be priced", lot.ID, m.Raw)
		}
		return n
	}
	t.Fatalf("campaign's %s forwards no max_passes — its lots take modernize's own default, which this bound must then read instead", lot.ID)
	return 0
}

// TestModernizeLotCostCapFundsTheForwardedPasses holds the child's spend cap
// to the agent passes campaign asks for (read off run_lot's `with:` mapping:
// lot_max_passes, forwarded as modernize's max_passes): the first pass and
// one repair pass per red gate the loop permits, each costing
// lotPassCostShare, all funded under the loop guard's threshold — and under
// campaign no launch override raises the child's cap. A direct launch reads
// modernize's own max_passes, held the same way.
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
		{"campaign's run_lot", forwardedMaxPasses(t, campaign)},
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
