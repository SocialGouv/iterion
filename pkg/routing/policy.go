// Package routing evaluates a run's launch-frozen RoutingPolicy
// against its terminal state. It is a pure decision function — it
// performs no action, maintains no state, and is shared by every
// consumer (API surfaces today, the outcome reactor next) so there is
// exactly one reading of a contract.
package routing

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Decision is what the policy says about a terminal run. It is a
// statement, not an action — the consumer still applies its own
// preconditions (banked branch integrity, continuation ownership,
// relaunch budget) before acting.
type Decision string

const (
	// DecisionMerge: SuccessWhen held, no blocker held, and "merge" is
	// an allowed action.
	DecisionMerge Decision = "merge"
	// DecisionEscalate: a blocker held, an expression could not be
	// read strictly, the outcome is a failure the policy does not
	// permit acting on, or the policy allows nothing — a human (or a
	// richer consumer) owns the next step. Escalation is the DEFAULT:
	// every uncertain path lands here, never on merge.
	DecisionEscalate Decision = "escalate"
	// DecisionRelaunch: the run failed in a way the policy permits
	// retrying fresh ("relaunch" allowed). The consumer enforces
	// MaxRelaunches — the decision only states permission.
	DecisionRelaunch Decision = "relaunch"
)

// Verdict carries the decision and its evidence: which expression
// produced it and what it evaluated to. A consumer records the verdict
// (with the policy hash) so an audit can replay WHY.
type Verdict struct {
	Decision Decision
	// Reason is a one-line, human-readable cause ("success_when held",
	// "block_when[0] held", "success_when: path absent").
	Reason string
	// PolicyHash echoes the contract that decided.
	PolicyHash string
}

// Evaluate reads the policy against the run's terminal state. Strict
// by construction:
//   - nil policy → escalate ("no contract, no automatic action");
//   - any blocker that is true OR unreadable → escalate;
//   - SuccessWhen unreadable (absent path, non-bool value, parse
//     error) → escalate — a missing field is a contract violation,
//     not a false;
//   - SuccessWhen true → merge if allowed, else escalate;
//   - SuccessWhen false → relaunch if allowed, else escalate.
func Evaluate(r *store.Run) Verdict {
	p := r.RoutingPolicy
	if p == nil {
		return Verdict{Decision: DecisionEscalate, Reason: "no routing policy on the run"}
	}
	v := Verdict{PolicyHash: p.Hash}

	ctx := exprContext(r)

	for i, b := range p.BlockWhen {
		held, err := evalStrictBool(b, ctx)
		if err != nil {
			v.Decision = DecisionEscalate
			v.Reason = fmt.Sprintf("block_when[%d] unreadable: %v", i, err)
			return v
		}
		if held {
			v.Decision = DecisionEscalate
			v.Reason = fmt.Sprintf("block_when[%d] held: %s", i, b)
			return v
		}
	}

	success, err := evalStrictBool(p.SuccessWhen, ctx)
	if err != nil {
		v.Decision = DecisionEscalate
		v.Reason = fmt.Sprintf("success_when unreadable: %v", err)
		return v
	}
	if success {
		if allows(p, "merge") {
			v.Decision = DecisionMerge
			v.Reason = "success_when held: " + p.SuccessWhen
			return v
		}
		v.Decision = DecisionEscalate
		v.Reason = "success_when held but merge is not an allowed action"
		return v
	}
	if allows(p, "relaunch") {
		v.Decision = DecisionRelaunch
		v.Reason = "success_when did not hold: " + p.SuccessWhen
		return v
	}
	v.Decision = DecisionEscalate
	v.Reason = "success_when did not hold and relaunch is not an allowed action"
	return v
}

// Validate parses every expression and normalises the policy; called at
// launch so a malformed contract is refused BEFORE any work happens,
// never discovered at the terminal.
func Validate(p *store.RoutingPolicy) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.SuccessWhen) == "" {
		return fmt.Errorf("routing policy: success_when is required")
	}
	if _, err := expr.Parse(p.SuccessWhen); err != nil {
		return fmt.Errorf("routing policy: success_when: %w", err)
	}
	for i, b := range p.BlockWhen {
		if _, err := expr.Parse(b); err != nil {
			return fmt.Errorf("routing policy: block_when[%d]: %w", i, err)
		}
	}
	for _, a := range p.AllowedActions {
		switch a {
		case "merge", "relaunch", "resume":
		default:
			return fmt.Errorf("routing policy: unknown action %q (allowed: merge, relaunch, resume)", a)
		}
	}
	if p.MaxRelaunches < 0 {
		return fmt.Errorf("routing policy: max_relaunches must be >= 0")
	}
	return nil
}

// allows reports whether action is in the policy's allowed set. An
// empty set allows NOTHING — the fail-closed default.
func allows(p *store.RoutingPolicy, action string) bool {
	for _, a := range p.AllowedActions {
		if a == action {
			return true
		}
	}
	return false
}

// evalStrictBool evaluates src and requires a literal bool result. An
// absent path (nil) or any other type is an error — the strict rule
// that keeps "field missing" from reading as "false" and silently
// green-lighting a merge.
func evalStrictBool(src string, ctx *expr.Context) (bool, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return false, fmt.Errorf("parse: %w", err)
	}
	val, err := ast.Eval(ctx)
	if err != nil {
		return false, err
	}
	b, ok := val.(bool)
	if !ok {
		if val == nil {
			return false, fmt.Errorf("path absent (evaluated to nil)")
		}
		return false, fmt.Errorf("not a bool (got %T)", val)
	}
	return b, nil
}

// exprContext resolves the DSL namespaces against the run's persisted
// terminal state: outputs.<node>.<key>… reads the checkpoint's output
// map (the gates the bot published), run.<field> reads a curated set
// of run document fields.
func exprContext(r *store.Run) *expr.Context {
	return &expr.Context{
		Outputs: func(path []string) any {
			if r.Checkpoint == nil || len(path) == 0 {
				return nil
			}
			node, ok := r.Checkpoint.Outputs[path[0]]
			if !ok {
				return nil
			}
			var cur any = node
			for _, seg := range path[1:] {
				m, ok := cur.(map[string]any)
				if !ok {
					return nil
				}
				cur, ok = m[seg]
				if !ok {
					return nil
				}
			}
			return cur
		},
		Run: func(path []string) any {
			if len(path) != 1 {
				return nil
			}
			switch path[0] {
			case "status":
				return string(r.Status)
			case "terminal_code":
				return string(r.FailureCode)
			case "final_branch":
				return r.FinalBranch
			case "final_commit":
				return r.FinalCommit
			default:
				return nil
			}
		},
	}
}
