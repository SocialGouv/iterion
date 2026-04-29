package model

import (
	"context"
	"fmt"
	"regexp"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
)

// NewDefaultLifecycleHooks returns a Runner pre-configured with the
// hooks every iterion run should have on by default:
//
//   - SafetyHook on PreToolUse: blocks dangerous bash commands
//     (rm -rf /, fork bombs) before they run.
//
// Tool-call observability (PostToolUse, PostToolUseFailure) is NOT
// wired here: the executor already emits OnToolCall via applyHooks
// with the correct node ID at every tool outcome (success, error, or
// hook-blocked). Adding a PostToolUse audit hook here would duplicate
// the same event under a placeholder node ID — see AuditHook docs.
//
// Hosts that need to add or override hooks should construct their own
// runner via hooks.NewRunner and pass it via WithLifecycleHooks; this
// helper is a sane default for the common case.
//
// The default patterns are hardcoded and known to compile, so this
// function never returns an error — but it preserves the error
// signature for symmetry with NewLifecycleHooks variants that take
// user-supplied patterns.
func NewDefaultLifecycleHooks(_ EventHooks) *hooks.Runner {
	r := hooks.NewRunner()
	hook, err := SafetyHook(DefaultDangerousCommandPatterns())
	if err != nil {
		// Defaults are tested and must compile; surface as panic to
		// catch regressions in DefaultDangerousCommandPatterns().
		panic(fmt.Sprintf("iterion: built-in safety patterns failed to compile: %v", err))
	}
	r.Register(hooks.PreToolUse, hook)
	return r
}

// DefaultDangerousCommandPatterns returns conservative regexes that
// match obviously destructive shell invocations. The list is small on
// purpose: each entry should have very low false-positive rate
// against typical engineering workflows.
func DefaultDangerousCommandPatterns() []string {
	return []string{
		// `rm -rf /`, `rm -rf ~`, `rm -rf $HOME`, with optional flag ordering.
		`(?i)\brm\s+(-[rRf]+\s+)+(/\s*$|/\s+|\$HOME\b|~\s*$|~\s)`,
		// Classic fork bomb.
		`:\(\)\{\s*:\|:&?\s*\}\s*;:`,
		// dd to a block device.
		`(?i)\bdd\s+.*of=/dev/(sd|nvme|hd|mmcblk)`,
		// mkfs against a real device path.
		`(?i)\bmkfs(\.\w+)?\s+/dev/`,
		// chmod -R 777 of a system path.
		`(?i)\bchmod\s+-R\s+0?7{3}\s+(/|/etc|/usr|/var|/home)\b`,
		// shutdown / reboot at root.
		`(?i)\b(shutdown|reboot|halt|poweroff)\s+(now|-h|-r)`,
	}
}

// SafetyHook blocks PreToolUse events whose top-level "command"
// input matches one of `patterns`. Returns an error listing every
// pattern that fails to compile so operators are not silently
// running without the safety net they configured.
func SafetyHook(patterns []string) (hooks.Handler, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	var compileErrs []string
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			compileErrs = append(compileErrs, fmt.Sprintf("%q: %v", p, err))
			continue
		}
		compiled = append(compiled, re)
	}
	if len(compileErrs) > 0 {
		return nil, fmt.Errorf("safety hook: %d invalid pattern(s): %v", len(compileErrs), compileErrs)
	}
	handler := func(ctx context.Context, hctx hooks.Context) (hooks.Decision, error) {
		if hctx.ToolInput == nil {
			return hooks.Decision{Action: hooks.ActionContinue}, nil
		}
		raw, ok := hctx.ToolInput["command"]
		if !ok {
			return hooks.Decision{Action: hooks.ActionContinue}, nil
		}
		cmd, ok := raw.(string)
		if !ok || cmd == "" {
			return hooks.Decision{Action: hooks.ActionContinue}, nil
		}
		for _, re := range compiled {
			if re.MatchString(cmd) {
				return hooks.Decision{
					Action: hooks.ActionBlock,
					Reason: fmt.Sprintf("safety hook blocked dangerous command (pattern: %s)", re.String()),
				}, nil
			}
		}
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	}
	return handler, nil
}
