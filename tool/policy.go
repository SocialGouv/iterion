package tool

import (
	"fmt"
	"strings"
)

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

	for _, pattern := range p.AllowedTools {
		if matchPattern(pattern, qualifiedName) {
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
