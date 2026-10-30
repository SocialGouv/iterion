package ir

import "strings"

// Verified Action diagnostics (ADR-044). Deterministic ACTION nodes
// (tool) may carry a goal + postcondition + policy + recovery quad. The
// postcondition is what makes the recovery rungs SAFE — an agent cannot
// fake success past a deterministic property check. These diagnostics
// encode the anti-Goodhart firewall: recovery is for actions, never gates.
const (
	DiagInvalidPolicy        DiagCode = "C103" // policy value not in {required, recover, best_effort} (error)
	DiagRecoveryNoPostcond   DiagCode = "C104" // recovery configured without a postcondition (error)
	DiagRecoveryOnGate       DiagCode = "C105" // recovery rungs attached to a gate (recipe == postcondition) (error)
	DiagRecoveryWithoutRecov DiagCode = "C106" // recovery bounds present but policy != recover (warning, dead config)
)

// validateVerifiedActions enforces the Verified Action contract on tool
// nodes. The rules are the load-bearing safety invariants from ADR-044:
//
//   - C103 — the policy must be a known value.
//   - C104 — recovery (policy: recover, or any recovery: bound) requires a
//     postcondition; without the truth oracle, adaptive recovery would
//     reintroduce the façade risk the gate model exists to prevent.
//   - C105 — a GATE is the degenerate quad (recipe == postcondition); it
//     must never declare recovery. "Never attach LLM recovery to a gate."
//   - C106 — recovery bounds/model only run under policy: recover; declaring
//     them under any other policy is dead config.
//   - C179 — the genre half of that inertness: under policy: recover, a node
//     whose ONLY budgeted rung is max_repair_attempts and whose recipe is a
//     bare registered tool name reaches no rung (rung 3 corrects a command
//     the recipe does not carry, rung 4 is off) — the recovery is inert.
func (c *compiler) validateVerifiedActions(w *Workflow) {
	for _, node := range w.Nodes {
		tn, ok := node.(*ToolNode)
		if !ok {
			continue
		}

		policy := strings.TrimSpace(tn.Policy)
		if policy != "" {
			switch policy {
			case PolicyRequired, PolicyRecover, PolicyBestEffort:
				// known
			default:
				c.errorfAtProperty(DiagInvalidPolicy, "tool", tn.ID, "policy",
					"tool %q has invalid policy %q; valid values are required, recover, best_effort",
					tn.ID, tn.Policy)
			}
		}

		// Does this node ask for recovery rungs at all?
		hasRecoveryBounds := tn.Recovery != nil &&
			(tn.Recovery.MaxRepairAttempts > 0 || tn.Recovery.MaxAgentAttempts > 0 ||
				tn.Recovery.Model != "" || len(tn.Recovery.AgentTools) > 0)
		wantsRecovery := policy == PolicyRecover || hasRecoveryBounds

		// C104 — recovery needs a truth oracle.
		if wantsRecovery && tn.Postcondition == "" {
			c.errorfAt(DiagRecoveryNoPostcond, tn.ID, "",
				"tool %q configures recovery (policy: recover / recovery:) but declares no postcondition — the postcondition is the single source of truth that makes recovery safe; without it an agent could fake success",
				tn.ID)
		}

		// C105 — never attach recovery to a gate (recipe == postcondition).
		recipe := tn.Command
		if recipe == "" {
			recipe = tn.Script
		}
		if wantsRecovery && tn.Postcondition != "" &&
			strings.TrimSpace(recipe) == strings.TrimSpace(tn.Postcondition) {
			c.errorfAt(DiagRecoveryOnGate, tn.ID, "",
				"tool %q is a gate (recipe == postcondition) but declares recovery — gates must stay deterministic (the Goodhart firewall); never attach LLM recovery to a gate",
				tn.ID)
		}

		// C106 — recovery bounds only matter under policy: recover.
		if hasRecoveryBounds && policy != PolicyRecover {
			c.warnfAt(DiagRecoveryWithoutRecov, tn.ID, "",
				"tool %q declares recovery bounds but policy is %q — recovery rungs only run under policy: recover, so this config is inert",
				tn.ID, displayPolicy(policy))
		}

		// C179 — the genre half of the same inertness. max_repair_attempts
		// budgets rung 3, the self-repair generation, which proposes a
		// corrected COMMAND; since #2783 dispatch gates that rung on the
		// recipe's genre (ir.ToolRecoverySelfRepairRung reads
		// ir.ToolRecipeSelfRepairable), and a registry recipe — a bare
		// registered tool name — carries no command to correct, so the
		// rung is skipped. With no max_agent_attempts rung 4 is off too:
		// no recovery rung is reachable and the author hears nothing. The
		// predicate is asked the way dispatch asks it — through
		// ToolRecipeSelfRepairable — so the warning and the runtime cannot
		// disagree; an action recipe is excluded (C262 already refuses
		// recovery there), and a recipe-LESS node is excluded too (C004
		// already refuses it — the warning would stack on the error with
		// an empty tool name).
		if policy == PolicyRecover && tn.Recovery != nil &&
			tn.Recovery.MaxRepairAttempts > 0 && tn.Recovery.MaxAgentAttempts <= 0 &&
			ToolRecipeKindOf(tn) == ToolRecipeRegistry && tn.Command != "" {
			c.warnfAt(DiagRepairRungUnreachable, tn.ID, "",
				"tool %q budgets only max_repair_attempts under policy: recover, but rung 3 proposes a corrected command and a registered tool name (%q) has none — dispatch skips the rung, and rung 4 is off without max_agent_attempts, so NO recovery rung is reachable",
				tn.ID, tn.Command)
		}
	}
}

func displayPolicy(p string) string {
	if p == "" {
		return "required (default)"
	}
	return p
}
