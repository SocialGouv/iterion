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

// CurrentPolicyVersion is the newest contract schema this reader
// understands. A newer contract carries fields this code cannot see —
// executing it anyway would honour half a contract.
const CurrentPolicyVersion = 1

// Evaluate reads the policy against the run's terminal state. Strict
// by construction:
//   - nil policy → escalate ("no contract, no automatic action");
//   - a contract newer than this reader → escalate;
//   - no terminal outputs on the run → escalate;
//   - any blocker that is true OR unreadable → escalate;
//   - SuccessWhen unreadable (absent path, non-bool value, parse
//     error) → escalate — a missing field is a contract violation,
//     not a false. Strictness holds through composition: every REF of
//     the expression is resolved and type-checked individually before
//     evaluation, because "!"/"&&"/"||" coerce their operands (an
//     absent field under "!" would otherwise read as true);
//   - SuccessWhen true → merge if allowed, else escalate;
//   - SuccessWhen false → relaunch if allowed, else escalate.
func Evaluate(r *store.Run) Verdict {
	p := r.RoutingPolicy
	if p == nil {
		return Verdict{Decision: DecisionEscalate, Reason: "no routing policy on the run"}
	}
	v := Verdict{PolicyHash: p.Hash}
	if p.Version > CurrentPolicyVersion {
		v.Decision = DecisionEscalate
		v.Reason = fmt.Sprintf("contract version %d is newer than this reader (max %d)", p.Version, CurrentPolicyVersion)
		return v
	}
	if r.Checkpoint == nil || len(r.Checkpoint.Outputs) == 0 {
		// Defence in depth, independent of the per-ref checks below:
		// with no terminal outputs there is nothing to read a verdict
		// from, whatever shape the expressions take.
		v.Decision = DecisionEscalate
		v.Reason = "no terminal outputs on the run — nothing to evaluate the contract against"
		return v
	}

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
	if p.Version < 1 || p.Version > CurrentPolicyVersion {
		return fmt.Errorf("routing policy: version must be 1..%d, got %d", CurrentPolicyVersion, p.Version)
	}
	if strings.TrimSpace(p.SuccessWhen) == "" {
		return fmt.Errorf("routing policy: success_when is required")
	}
	if err := validateExpr("success_when", p.SuccessWhen); err != nil {
		return err
	}
	for i, b := range p.BlockWhen {
		if err := validateExpr(fmt.Sprintf("block_when[%d]", i), b); err != nil {
			return err
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
	switch p.MergeStrategy {
	case "", store.MergeStrategySquash, store.MergeStrategyMerge:
	default:
		return fmt.Errorf("routing policy: unknown merge_strategy %q (allowed: squash, merge)", p.MergeStrategy)
	}
	if err := validateBranchName(p.MergeInto); err != nil {
		return fmt.Errorf("routing policy: merge_into: %w", err)
	}
	return nil
}

// validateExpr enforces the v1 contract grammar: a boolean combination
// of outputs.<node>.<key>… refs, nothing else. A namespace the routing
// context cannot resolve (vars, input, artifacts, loop) — or a
// comparison, literal or combinator whose truthy coercion would defeat
// strict evaluation — is refused at LAUNCH, not discovered at the
// terminal.
func validateExpr(field, src string) error {
	ast, err := expr.Parse(src)
	if err != nil {
		return fmt.Errorf("routing policy: %s: %w", field, err)
	}
	if !ast.IsBoolAlgebraOverRefs() {
		return fmt.Errorf("routing policy: %s: the contract grammar is output refs combined with !, && and || only", field)
	}
	for _, ref := range ast.Refs() {
		if ref.Namespace != "outputs" || len(ref.Path) < 2 {
			return fmt.Errorf("routing policy: %s: ref %s.%s is outside the contract's vocabulary (outputs.<node>.<key> only)", field, ref.Namespace, strings.Join(ref.Path, "."))
		}
	}
	return nil
}

// validateBranchName refuses target names git would misread or a shell
// wrapper could trip on — the value comes from an HTTP body and feeds a
// git operation.
func validateBranchName(name string) error {
	if name == "" {
		return nil
	}
	if strings.HasPrefix(name, "-") || strings.Contains(name, "..") ||
		strings.ContainsAny(name, " \t\n~^:?*[\\") {
		return fmt.Errorf("invalid branch name %q", name)
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

// evalStrictBool evaluates src strictly. Three layers, each needed:
//  1. the expression must be pure boolean algebra over refs (the
//     grammar Validate enforces — re-checked here so a contract that
//     somehow bypassed launch validation still cannot coerce);
//  2. EVERY ref must resolve to a non-nil bool BEFORE evaluation —
//     "!"/"&&"/"||" coerce their operands via truthiness, so the
//     final-result type check alone would let "!outputs.gone.flag"
//     (absent field, typo'd node, renamed key) read as true;
//  3. the result must be a bool (redundant given 1+2, kept as belt).
func evalStrictBool(src string, ctx *expr.Context) (bool, error) {
	ast, err := expr.Parse(src)
	if err != nil {
		return false, fmt.Errorf("parse: %w", err)
	}
	if !ast.IsBoolAlgebraOverRefs() {
		return false, fmt.Errorf("not a boolean combination of output refs — the strict contract grammar is refs, !, &&, || only")
	}
	for _, ref := range ast.Refs() {
		val := resolveRef(ctx, ref)
		if val == nil {
			return false, fmt.Errorf("%s.%s: path absent", ref.Namespace, strings.Join(ref.Path, "."))
		}
		if _, ok := val.(bool); !ok {
			return false, fmt.Errorf("%s.%s: not a bool (got %T)", ref.Namespace, strings.Join(ref.Path, "."), val)
		}
	}
	val, err := ast.Eval(ctx)
	if err != nil {
		return false, err
	}
	b, ok := val.(bool)
	if !ok {
		return false, fmt.Errorf("not a bool (got %T)", val)
	}
	return b, nil
}

// resolveRef reads one ref through the same resolvers evaluation uses.
func resolveRef(ctx *expr.Context, ref expr.Ref) any {
	switch ref.Namespace {
	case "outputs":
		if ctx.Outputs == nil {
			return nil
		}
		return ctx.Outputs(ref.Path)
	default:
		return nil
	}
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
	}
}
