package permissions

import (
	"context"
	"encoding/json"
	"fmt"
)

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
	classifier       Classifier
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

// WithClassifier registers a Classifier consulted when the active mode is
// ModeAuto. Passing nil clears any previously registered classifier (in
// which case the documented default RuleClassifier is used).
func (p *PermissionPolicy) WithClassifier(c Classifier) *PermissionPolicy {
	p.classifier = c
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
func (p *PermissionPolicy) AuthorizeWithContext(toolName, input string, permCtx *PermissionContext, prompter PermissionPrompter) PermissionOutcome {
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

	// ModeDontAsk: strict allow-list. After deny rules have run, the only
	// paths to Allow are an explicit allow rule match or an explicit
	// per-tool requirement registered via WithToolRequirement. Anything
	// else is denied without prompting.
	if currentMode == ModeDontAsk {
		if allowRule != nil {
			return PermissionOutcome{Allowed: true}
		}
		if _, listed := p.toolRequirements[toolName]; listed {
			return PermissionOutcome{Allowed: true}
		}
		return PermissionOutcome{
			Allowed: false,
			Reason: fmt.Sprintf("tool '%s' is not in the dont-ask allow-list", toolName),
		}
	}

	// ModeAuto: consult the registered classifier (or the default
	// RuleClassifier if none) and route Allow/Deny/Ask outcomes.
	if currentMode == ModeAuto {
		classifier := p.classifier
		if classifier == nil {
			classifier = NewRuleClassifier()
		}
		args := parseToolArgs(input)
		decision, err := classifier.Classify(context.Background(), toolName, args)
		if err != nil {
			decision = DecisionAsk
		}
		switch decision {
		case DecisionAllow:
			// Ask rules still take precedence over a classifier Allow.
			if askRule != nil {
				reason := fmt.Sprintf("tool '%s' requires approval due to ask rule '%s'", toolName, askRule.raw)
				return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)
			}
			return PermissionOutcome{Allowed: true}
		case DecisionDeny:
			return PermissionOutcome{
				Allowed: false,
				Reason: fmt.Sprintf("tool '%s' denied by auto-mode classifier", toolName),
			}
		default:
			reason := fmt.Sprintf("tool '%s' requires approval (auto-mode classifier deferred)", toolName)
			return promptOrDeny(toolName, input, currentMode, requiredMode, reason, prompter)
		}
	}

	// 2. Hook override.
	if permCtx != nil && permCtx.OverrideDecision != nil {
		switch *permCtx.OverrideDecision {
		case OverrideDeny:
			reason := fmt.Sprintf("tool '%s' denied by hook", toolName)
			if permCtx.OverrideReason != "" {
				reason = permCtx.OverrideReason
			}
			return PermissionOutcome{Allowed: false, Reason: reason}

		case OverrideAsk:
			reason := fmt.Sprintf("tool '%s' requires approval due to hook guidance", toolName)
			if permCtx.OverrideReason != "" {
				reason = permCtx.OverrideReason
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

// parseToolArgs decodes a tool input string as a JSON object so the result can
// be handed to a Classifier. If the input is not valid JSON, an empty map is
// returned (the classifier still receives the toolName).
func parseToolArgs(input string) map[string]any {
	args := map[string]any{}
	if input == "" {
		return args
	}
	_ = json.Unmarshal([]byte(input), &args)
	return args
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
