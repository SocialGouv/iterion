package permissions

import "fmt"

// PermissionPolicy evaluates permission mode requirements plus allow/deny/ask
// rules. This matches Rust's PermissionPolicy struct and authorize_with_context
// flow: deny rules → hook override → ask rules → allow rules/mode check →
// escalation prompt → deny.
type PermissionPolicy struct {
	activeMode       PermissionMode
	toolRequirements map[string]PermissionMode
	allowRules       []permissionRule
	denyRules        []permissionRule
	askRules         []permissionRule
}

// NewPermissionPolicy creates a policy with the given active mode.
func NewPermissionPolicy(activeMode PermissionMode) *PermissionPolicy {
	return &PermissionPolicy{
		activeMode:       activeMode,
		toolRequirements: make(map[string]PermissionMode),
	}
}

// WithToolRequirement adds a per-tool permission mode requirement.
func (p *PermissionPolicy) WithToolRequirement(toolName string, requiredMode PermissionMode) *PermissionPolicy {
	p.toolRequirements[toolName] = requiredMode
	return p
}

// WithPermissionRules loads allow/deny/ask rules from string lists.
// Each string is parsed via parsePermissionRule (supporting "Bash(git:*)" syntax).
func (p *PermissionPolicy) WithPermissionRules(allow, deny, ask []string) *PermissionPolicy {
	p.allowRules = make([]permissionRule, 0, len(allow))
	for _, raw := range allow {
		p.allowRules = append(p.allowRules, parsePermissionRule(raw))
	}
	p.denyRules = make([]permissionRule, 0, len(deny))
	for _, raw := range deny {
		p.denyRules = append(p.denyRules, parsePermissionRule(raw))
	}
	p.askRules = make([]permissionRule, 0, len(ask))
	for _, raw := range ask {
		p.askRules = append(p.askRules, parsePermissionRule(raw))
	}
	return p
}

// ActiveMode returns the policy's current active permission mode.
func (p *PermissionPolicy) ActiveMode() PermissionMode {
	return p.activeMode
}

// RequiredModeFor returns the required permission mode for a specific tool.
// Defaults to DangerFullAccess if no specific requirement is configured.
func (p *PermissionPolicy) RequiredModeFor(toolName string) PermissionMode {
	if mode, ok := p.toolRequirements[toolName]; ok {
		return mode
	}
	return ModeDangerFullAccess
}

// Authorize evaluates permission for a tool invocation without hook context.
// This is a convenience wrapper around AuthorizeWithContext with a default context.
func (p *PermissionPolicy) Authorize(toolName, input string, prompter PermissionPrompter) PermissionOutcome {
	return p.AuthorizeWithContext(toolName, input, &PermissionContext{}, prompter)
}

// AuthorizeWithContext evaluates full permission for a tool invocation with
// optional hook-provided context. The evaluation order matches Rust:
//
//  1. Deny rules (short-circuit)
//  2. Hook override (Deny → deny, Ask → prompt, Allow → check ask rules then allow)
//  3. Ask rules (force prompt even if mode allows)
//  4. Allow rules or mode check
//  5. Escalation prompt (WorkspaceWrite→DangerFullAccess, or Prompt mode)
//  6. Default deny
func (p *PermissionPolicy) AuthorizeWithContext(toolName, input string, context *PermissionContext, prompter PermissionPrompter) PermissionOutcome {
	// 1. Deny rules always short-circuit.
	if rule := findMatchingRule(p.denyRules, toolName, input); rule != nil {
		return PermissionOutcome{
			Allowed: false,
			Reason:  fmt.Sprintf("Permission to use %s has been denied by rule '%s'", toolName, rule.raw),
		}
	}

	currentMode := p.ActiveMode()
	requiredMode := p.RequiredModeFor(toolName)
	askRule := findMatchingRule(p.askRules, toolName, input)
	allowRule := findMatchingRule(p.allowRules, toolName, input)

	// 2. Hook override.
	if context != nil && context.OverrideDecision != nil {
		switch *context.OverrideDecision {
		case OverrideDeny:
			reason := fmt.Sprintf("tool '%s' denied by hook", toolName)
			if context.OverrideReason != "" {
				reason = context.OverrideReason
			}
			return PermissionOutcome{Allowed: false, Reason: reason}

		case OverrideAsk:
			reason := fmt.Sprintf("tool '%s' requires approval due to hook guidance", toolName)
			if context.OverrideReason != "" {
				reason = context.OverrideReason
			}
			return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)

		case OverrideAllow:
			// Hook says allow, but ask rules still take precedence.
			if askRule != nil {
				reason := fmt.Sprintf("tool '%s' requires approval due to ask rule '%s'", toolName, askRule.raw)
				return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)
			}
			if allowRule != nil || currentMode == ModeAllow || currentMode >= requiredMode {
				return PermissionOutcome{Allowed: true}
			}
		}
	}

	// 3. Ask rules force prompt even when mode would allow.
	if askRule != nil {
		reason := fmt.Sprintf("tool '%s' requires approval due to ask rule '%s'", toolName, askRule.raw)
		return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)
	}

	// 4. Allow rules or sufficient mode.
	if allowRule != nil || currentMode == ModeAllow || currentMode >= requiredMode {
		return PermissionOutcome{Allowed: true}
	}

	// 5. Escalation: Prompt mode or WorkspaceWrite→DangerFullAccess.
	if currentMode == ModePrompt ||
		(currentMode == ModeWorkspaceWrite && requiredMode == ModeDangerFullAccess) {
		reason := fmt.Sprintf("tool '%s' requires approval to escalate from %s to %s",
			toolName, currentMode.AsStr(), requiredMode.AsStr())
		return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)
	}

	// 6. Default deny.
	return PermissionOutcome{
		Allowed: false,
		Reason: fmt.Sprintf("tool '%s' requires %s permission; current mode is %s",
			toolName, requiredMode.AsStr(), currentMode.AsStr()),
	}
}

// promptOrDeny asks the prompter for a decision, or denies if no prompter.
func promptOrDeny(toolName, input string, currentMode, requiredMode PermissionMode, reason string, prompter PermissionPrompter) PermissionOutcome {
	request := &PermissionRequest{
		ToolName:     toolName,
		Input:        input,
		CurrentMode:  currentMode,
		RequiredMode: requiredMode,
		Reason:       reason,
	}

	if prompter == nil {
		if reason == "" {
			reason = fmt.Sprintf("tool '%s' requires approval to run while mode is %s", toolName, currentMode.AsStr())
		}
		return PermissionOutcome{Allowed: false, Reason: reason}
	}

	decision := prompter.Decide(request)
	if decision.Allowed {
		return PermissionOutcome{Allowed: true}
	}
	return PermissionOutcome{Allowed: false, Reason: decision.Reason}
}

// findMatchingRule returns the first rule that matches the given tool name and input.
func findMatchingRule(rules []permissionRule, toolName, input string) *permissionRule {
	for i := range rules {
		if rules[i].matches(toolName, input) {
			return &rules[i]
		}
	}
	return nil
}
