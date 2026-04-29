package tool

import (
	"fmt"
	"math"
)

// ---------------------------------------------------------------------------
// RulePolicy — ordered conditional rules, first-match-wins
// ---------------------------------------------------------------------------

// Rule is a single conditional rule in a RulePolicy.
type Rule struct {
	NodeIDs  []string               // nil = any node
	NodeKind string                 // "" = any kind
	VarMatch map[string]interface{} // nil = any vars
	Allow    []string               // patterns to allow if this rule matches
	Deny     bool                   // if true, deny matched tools instead
}

// RulePolicy evaluates an ordered list of conditional rules using
// first-match-wins semantics. If no rule matches, the Fallback policy
// is consulted. A nil Fallback means open (allow all).
type RulePolicy struct {
	Rules    []Rule
	Fallback *Policy // nil = open (allow all if no rule matched)
}

// CheckContext implements ToolChecker.
func (rp *RulePolicy) CheckContext(ctx PolicyContext) error {
	for _, rule := range rp.Rules {
		if !ruleMatches(rule, ctx) {
			continue
		}
		// Rule matched — evaluate tool patterns.
		toolHit := patternsMatch(rule.Allow, ctx.ToolName)
		if rule.Deny {
			// Deny rule: if the tool matches patterns, deny it.
			if toolHit {
				return fmt.Errorf("%w: tool %q denied by rule", ErrToolDenied, ctx.ToolName)
			}
			// Tool didn't match deny patterns — rule matched but doesn't apply
			// to this tool. First-match-wins means we stop here.
			return nil
		}
		// Allow rule: tool must match patterns.
		if toolHit {
			return nil
		}
		return fmt.Errorf("%w: tool %q is not in the allowlist", ErrToolDenied, ctx.ToolName)
	}

	// No rule matched — use fallback.
	if rp.Fallback != nil {
		return rp.Fallback.CheckContext(ctx)
	}
	return nil // nil fallback = open
}

// ruleMatches checks whether all conditions on a rule match the context.
func ruleMatches(r Rule, ctx PolicyContext) bool {
	if r.NodeIDs != nil {
		found := false
		for _, id := range r.NodeIDs {
			if id == ctx.NodeID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if r.NodeKind != "" && r.NodeKind != ctx.NodeKind {
		return false
	}
	if r.VarMatch != nil {
		for k, expected := range r.VarMatch {
			actual, ok := ctx.Vars[k]
			if !ok {
				return false
			}
			if !valuesEqual(expected, actual) {
				return false
			}
		}
	}
	return true
}

// valuesEqual compares two values for policy matching. It handles
// numeric type mismatches (int vs float64 from JSON unmarshalling)
// and falls back to string comparison for other types.
func valuesEqual(a, b interface{}) bool {
	if a == b {
		return true
	}
	aF, aOK := toFloat64(a)
	bF, bOK := toFloat64(b)
	if aOK && bOK {
		return aF == bF
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// toFloat64 converts numeric types to float64 for comparison.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		if math.IsNaN(n) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
