// Package hookstesting provides test helpers for the hooks package.
// This package is intended only for use in tests (_test.go files).
// It lives under internal/ to enforce import restrictions — only packages
// within the module can import it, and its test-only intent is clear.
package hookstesting

import "github.com/SocialGouv/iterion/pkg/claw/hooks"

// NewHookRunnerWithOverride creates a HookRunner for testing that will return
// the given permission override on the first PreToolUse call. This is used
// to test that the permission wiring in the conversation loop correctly
// handles hook permission decisions.
//
// This works by configuring a PreToolUse hook command that echoes the
// appropriate JSON on stdout.
func NewHookRunnerWithOverride(decision *hooks.PermissionDecision, reason string) *hooks.HookRunner {
	if decision == nil {
		return hooks.NewHookRunner(hooks.HookConfig{})
	}

	// Build a command that echoes the JSON permission override to stdout.
	// The hook output parser expects permissionDecision nested under hookSpecificOutput.
	cmd := `echo '{"hookSpecificOutput":{"permissionDecision":"` + string(*decision) + `","permissionDecisionReason":"` + reason + `"}}'`

	return hooks.NewHookRunner(hooks.HookConfig{
		PreToolUse: []string{cmd},
	})
}
