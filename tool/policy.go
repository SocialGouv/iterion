package tool

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// ToolChecker — contextual tool policy interface
// ---------------------------------------------------------------------------

// ToolChecker is the interface for contextual tool policy evaluation.
type ToolChecker interface {
	CheckContext(ctx PolicyContext) error
}

// PolicyContext carries evaluation context for dynamic policies.
type PolicyContext struct {
	NodeID   string
	NodeKind string // "agent", "judge", "tool", etc.
	ToolName string
	Input    json.RawMessage        // nil when unavailable
	Vars     map[string]interface{} // workflow vars, read-only
}

// ---------------------------------------------------------------------------
// ToolPolicy — allowlist-based command and tool policy
// ---------------------------------------------------------------------------

// ErrToolDenied is returned when a tool call is rejected by the policy.
var ErrToolDenied = fmt.Errorf("tool: denied by policy")

// Policy controls which tools may be executed during a run.
// It enforces an allowlist of tool name patterns. If the allowlist is nil
// (zero-value), all tools are allowed (open policy). An empty non-nil
// allowlist denies everything.
//
// Pattern syntax:
//   - "*"                → allow all tools
//   - "git_diff"         → exact match on qualified name
//   - "mcp.github.*"     → prefix match: any tool under the mcp.github namespace
//   - "run_command"      → exact match on a built-in
type Policy struct {
	// AllowedTools is the list of tool name patterns.
	// nil = open (everything allowed). Empty slice = deny all.
	AllowedTools []string
}

// OpenPolicy returns a policy that allows all tools.
func OpenPolicy() *Policy {
	return nil // nil policy = open
}

// DenyAllPolicy returns a policy that denies every tool.
func DenyAllPolicy() *Policy {
	return &Policy{AllowedTools: []string{}}
}

// NewPolicy creates a policy with the given allowed tool patterns.
func NewPolicy(patterns ...string) *Policy {
	return &Policy{AllowedTools: patterns}
}

// IsAllowed returns true if the given tool qualified name is permitted
// by this policy.
func (p *Policy) IsAllowed(qualifiedName string) bool {
	// nil policy = open, everything allowed.
	if p == nil {
		return true
	}
	return patternsMatch(p.AllowedTools, qualifiedName)
}

// patternsMatch checks whether toolName matches any pattern in the list.
func patternsMatch(patterns []string, toolName string) bool {
	for _, p := range patterns {
		if matchPattern(p, toolName) {
			return true
		}
	}
	return false
}

// Check returns nil if the tool is allowed, or a descriptive error if denied.
func (p *Policy) Check(qualifiedName string) error {
	if p.IsAllowed(qualifiedName) {
		return nil
	}
	return fmt.Errorf("%w: tool %q is not in the allowlist", ErrToolDenied, qualifiedName)
}

// CheckContext implements ToolChecker for the static Policy.
// It delegates to Check, ignoring all context fields except ToolName.
func (p *Policy) CheckContext(ctx PolicyContext) error {
	return p.Check(ctx.ToolName)
}

// ---------------------------------------------------------------------------
// VarRule — conditional tool patterns based on workflow vars
// ---------------------------------------------------------------------------

// VarRule maps a set of var conditions to tool patterns. When all vars match,
// the listed patterns are allowed.
type VarRule struct {
	VarMatch map[string]interface{} // vars that must match
	Allow    []string               // tool patterns to allow when matched
}

// ---------------------------------------------------------------------------
// BuildChecker — construct a ToolChecker from workflow + node policies
// ---------------------------------------------------------------------------

// BuildChecker constructs a ToolChecker from workflow-level patterns,
// optional per-node overrides, and optional var-conditional rules.
// Returns a simple *Policy when no overrides or rules are needed.
func BuildChecker(workflowPatterns []string, nodeOverrides map[string][]string, varRules []VarRule) ToolChecker {
	hasOverrides := len(nodeOverrides) > 0
	hasVarRules := len(varRules) > 0

	if !hasOverrides && !hasVarRules {
		// Simple static policy.
		if len(workflowPatterns) == 0 {
			return nil // open policy
		}
		return NewPolicy(workflowPatterns...)
	}

	// Build a RulePolicy with rules for overrides and var-rules,
	// falling back to the workflow-level patterns.
	var rules []Rule

	// Per-node overrides become rules that match by NodeID.
	for nodeID, patterns := range nodeOverrides {
		rules = append(rules, Rule{
			NodeIDs: []string{nodeID},
			Allow:   patterns,
		})
	}

	// Var-conditional rules.
	for _, vr := range varRules {
		rules = append(rules, Rule{
			VarMatch: vr.VarMatch,
			Allow:    vr.Allow,
		})
	}

	var fallback *Policy
	if len(workflowPatterns) > 0 {
		fallback = NewPolicy(workflowPatterns...)
	}

	return &RulePolicy{
		Rules:    rules,
		Fallback: fallback,
	}
}

// matchPattern checks whether qualifiedName matches a single pattern.
//
//	"*"            → matches everything
//	"foo.*"        → matches any name starting with "foo."
//	"foo"          → exact match
func matchPattern(pattern, qualifiedName string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		prefix := pattern[:len(pattern)-1] // "mcp.github." from "mcp.github.*"
		return strings.HasPrefix(qualifiedName, prefix)
	}
	return pattern == qualifiedName
}
