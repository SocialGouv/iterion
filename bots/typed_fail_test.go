package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A bot-declared refusal is a VERDICT the operator has to act on, and the
// only place a machine reads it is the run's own `failure_code` / `error`
// (`iterion runs list`, the studio, the merge-gate notice, the alert
// sinks). Routing such a refusal to the bare `fail` target files it under
// FAIL_NODE / "workflow reached fail node" — the same wording every other
// refusal in the fleet produces — and leaves the code on a node output only
// an artifact reader ever sees.
//
// Each entry below is one shipped refusal that carries a code: the edge
// that takes it, the named `fail` node it must land on, and whether
// continuing is the cure.
type codedRefusal struct {
	bot string
	// from/condition identify the guard edge; negated matches `when not X`.
	from      string
	condition string
	negated   bool
	// failNode is the named fail node the edge must target, and code the
	// value it stamps on the run.
	failNode  string
	code      string
	resumable bool
	// messageRefs are references the rendered `message:` must carry, so
	// the operator reads the figure that caused the refusal rather than a
	// restatement of the code.
	messageRefs []string
}

// workspaceProbeRefusals is the same refusal on every campaign bot whose
// mission needs an existing checkout: the deterministic entry probe found
// no repository (or an unreachable base). Non-resumable — nothing about
// the run changes by resuming it; the operator fixes the launch.
func workspaceProbeRefusals() []codedRefusal {
	var out []codedRefusal
	for _, bot := range repoRequiringCampaignBots {
		out = append(out, codedRefusal{
			bot: bot, from: "workspace_probe", condition: "ok", negated: true,
			failNode: "workspace_not_a_repo", code: "WORKSPACE_NOT_A_REPO",
			messageRefs: []string{"{{outputs.workspace_probe.reason}}"},
		})
	}
	return out
}

func codedRefusals() []codedRefusal {
	out := workspaceProbeRefusals()
	return append(out,
		// The plan phase outgrew its share of the run's budget. The cure
		// is "raise the caps and carry on": a terminal failure would make
		// the operator re-pay a plan phase the run already completed,
		// which is the exact cost this guard exists to avoid.
		codedRefusal{
			bot: "branch-improve-loop", from: "plan_budget_gate", condition: "exhausted",
			failNode: "plan_exhausted", code: "PLAN_BUDGET_EXHAUSTED", resumable: true,
			messageRefs: []string{
				"{{outputs.plan_budget_gate.spent_usd}}",
				"{{outputs.plan_budget_gate.cost_share_usd}}",
				"{{outputs.plan_budget_gate.elapsed_seconds}}",
				"{{outputs.plan_budget_gate.duration_share_seconds}}",
			},
		},
		// The fixer read the diff (and the premise of its own mission) and
		// concluded there is nothing attributable to this branch to fix.
		// TERMINAL: resuming re-runs a campaign whose answer was "no work
		// here" — what has to change is the dispatch, not the budget. The
		// typed code is the whole point: it is what lets an unattended lane
		// tell "the fixer declined" from "the fixer died", without either
		// side naming the other.
		codedRefusal{
			bot: "branch-improve-loop", from: "decline_probe", condition: "honoured",
			failNode: "campaign_declined", code: "DECLINED",
			messageRefs: []string{"{{outputs.decline_probe.reason}}"},
		},
		// An explicit only_lot request naming a done / blocked / absent
		// lot. Terminal: the contract has to change before the answer can.
		codedRefusal{
			bot: "modernize", from: "work_gate", condition: "lot_not_actionable",
			failNode: "lot_not_actionable", code: "LOT_NOT_ACTIONABLE",
			messageRefs: []string{"{{outputs.work_gate.lot_status}}"},
		},
		// A gate command that outlived the contract's wall, and a contract
		// the verifier could not read. Both were already typed in prose
		// (`block_reason` / `log_tail` carry the code as a prefix); the run
		// itself said FAIL_NODE.
		codedRefusal{
			bot: "modernize", from: "lot_gate", condition: "timed_out",
			failNode: "gate_timeout", code: "GATE_TIMEOUT",
			messageRefs: []string{"{{outputs.lot_verify.block_reason}}"},
		},
		codedRefusal{
			bot: "modernize", from: "lot_gate", condition: "unreadable",
			failNode: "contract_unreadable", code: "CONTRACT_UNREADABLE",
			messageRefs: []string{"{{outputs.lot_gate.fail_log}}"},
		},
	)
}

// TestUncodedFailEdgesStayBare is the other half of the sweep: an edge
// whose refusal carries NO single code of its own keeps the bare `fail`
// target, and this test says why for each — so the next reader does not
// have to re-derive it, and a future code makes the entry stale rather
// than silently absent.
func TestUncodedFailEdgesStayBare(t *testing.T) {
	bare := []struct {
		bot, from, condition, why string
	}{
		{"modernize", "work_gate", "refused",
			"one bool carries TWO codes — plan_read's refuse() emits CONTRACT_UNREADABLE or LOT_UNEDITABLE — and a fail node stamps one; splitting the bool is a contract change, not an adoption"},
		{"modernize", "lot_gate", "refused",
			"the campaign forged a `done` or rewrote the contract: a refusal to JUDGE with no code of its own on either the node output or the tool log"},
		{"modernize", "mark_done", "refused",
			"the gate could not write its word (contract shape, git); mark_state carries `notice`, never a code"},
		{"feed-watch", "plan", "", "an unconditional edge from the planner: plain failure, nothing typed to carry"},
	}
	for _, b := range bare {
		t.Run(b.bot+"/"+b.from, func(t *testing.T) {
			wf := compilePlanPhaseBot(t, b.bot)
			for _, e := range wf.Edges {
				if e.From == b.from && e.Condition == b.condition && e.To == "fail" {
					return
				}
			}
			t.Errorf("no bare edge %s -> fail when %q — it was typed, or removed; update this entry with the code it now carries (%s)",
				b.from, b.condition, b.why)
		})
	}
}

// TestCodedRefusalsLandOnANamedFailNode pins, per refusal, that the guard
// edge targets the named fail node and that the node carries the code, the
// resumability and a message naming the figures.
func TestCodedRefusalsLandOnANamedFailNode(t *testing.T) {
	for _, r := range codedRefusals() {
		t.Run(r.bot+"/"+r.failNode, func(t *testing.T) {
			wf := compilePlanPhaseBot(t, r.bot)

			var edge *ir.Edge
			for _, e := range wf.Edges {
				if e.From == r.from && e.Condition == r.condition && e.Negated == r.negated {
					edge = e
				}
			}
			if edge == nil {
				t.Fatalf("no edge %s -> … when %s (negated=%v)", r.from, r.condition, r.negated)
			}
			if edge.To == "fail" {
				t.Fatalf("%s -> fail: the refusal reads FAIL_NODE / \"workflow reached fail node\" on the run, "+
					"indistinguishable from every other refusal in the fleet — route it to a named fail node carrying %s",
					r.from, r.code)
			}
			if edge.To != r.failNode {
				t.Fatalf("%s -> %s, want the named fail node %s", r.from, edge.To, r.failNode)
			}

			fn, ok := wf.Nodes[r.failNode].(*ir.FailNode)
			if !ok {
				t.Fatalf("%s is %T, want *ir.FailNode", r.failNode, wf.Nodes[r.failNode])
			}
			if fn.Code != r.code {
				t.Errorf("%s code = %q, want %q", r.failNode, fn.Code, r.code)
			}
			if fn.Resumable != r.resumable {
				t.Errorf("%s resumable = %v, want %v", r.failNode, fn.Resumable, r.resumable)
			}
			if fn.Message == nil || strings.TrimSpace(fn.Message.Raw) == "" {
				t.Fatalf("%s carries no message — the run's `error` falls back to the generic wording", r.failNode)
			}
			for _, ref := range r.messageRefs {
				if !strings.Contains(fn.Message.Raw, ref) {
					t.Errorf("%s message does not carry %s — the operator reads a restatement of the code, "+
						"not the figure that caused the refusal:\n  %s", r.failNode, ref, fn.Message.Raw)
				}
			}
		})
	}
}

// TestPlanBudgetGuardReadsTheRunNamespace pins the guard's shape after the
// `run.*` namespace landed: ONE deterministic compute reading the run's own
// consumption and the caps IN FORCE.
//
// What it replaces is the reason the assertion is worth its lines. The
// guard used to self-measure wall-clock in a tool node (`plan_scope_probe`
// stamped `started_epoch`) and compare against two hand-maintained mirror
// vars — so `iterion run --max-cost-usd 200` re-budgeted the run and the
// guard went on refusing against the literal 75 nobody had updated.
func TestPlanBudgetGuardReadsTheRunNamespace(t *testing.T) {
	wf := compilePlanPhaseBot(t, "branch-improve-loop")

	for _, v := range []string{"budget_max_duration_minutes", "budget_max_cost_usd"} {
		if _, ok := wf.Vars[v]; ok {
			t.Errorf("var %s survives — a mirror of the `budget:` block drifts from it in silence; "+
				"the caps come from run.max_* now", v)
		}
	}

	gate, ok := wf.Nodes["plan_budget_gate"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("plan_budget_gate is %T, want *ir.ComputeNode (one deterministic expression, no shell, no clock)", wf.Nodes["plan_budget_gate"])
	}
	exprs := map[string]string{}
	for _, ex := range gate.Exprs {
		exprs[ex.Key] = ex.Raw
	}
	verdict, hasVerdict := exprs["exhausted"]
	if !hasVerdict {
		t.Fatal("plan_budget_gate has no `exhausted` expression")
	}
	for _, member := range []string{"run.elapsed_seconds", "run.max_duration_seconds", "run.cost_usd", "run.max_cost_usd"} {
		if !strings.Contains(verdict, member) {
			t.Errorf("plan_budget_gate.exhausted = %q does not read %s", verdict, member)
		}
	}
	// A cap of 0 is UNBOUNDED on that axis, never "no allowance left":
	// without the guard-of-the-guard, an unbudgeted run refuses at once.
	for _, cap := range []string{"run.max_duration_seconds > 0", "run.max_cost_usd > 0"} {
		if !strings.Contains(verdict, cap) {
			t.Errorf("plan_budget_gate.exhausted = %q does not check %s — a cap of 0 means unbounded, "+
				"so an unbudgeted run would refuse on its first pass", verdict, cap)
		}
	}

	// The self-measured clock is gone from every schema that carried it.
	for name, sch := range wf.Schemas {
		for _, f := range sch.Fields {
			if f.Name == "started_epoch" {
				t.Errorf("schema %s still declares started_epoch — the wall clock is run.elapsed_seconds now", name)
			}
		}
	}
	if probe, ok := wf.Nodes["plan_scope_probe"].(*ir.ToolNode); ok {
		if strings.Contains(probe.Command, "time.time()") {
			t.Error("plan_scope_probe still self-measures wall-clock (time.time()) — the run's own clock is monotonic and survives a resume")
		}
	}
}
