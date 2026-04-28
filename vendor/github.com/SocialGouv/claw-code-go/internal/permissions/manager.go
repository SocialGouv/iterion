package permissions

import "sync"

// Manager enforces permissions for tool execution according to a mode and ruleset.
type Manager struct {
	Mode  PermissionMode
	Rules *Ruleset

	mu       sync.Mutex
	cache    map[string]Decision // keyed by tool name for ScopeAlways grants
	prompter PermissionPrompter  // optional; set via SetPrompter()
	policy   *PermissionPolicy   // optional; when set, Check delegates here
}

// NewManager creates a Manager with the given mode and ruleset.
// If rules is nil an empty ruleset is used.
func NewManager(mode PermissionMode, rules *Ruleset) *Manager {
	if rules == nil {
		rules = &Ruleset{}
	}
	return &Manager{
		Mode:  mode,
		Rules: rules,
		cache: make(map[string]Decision),
	}
}

// SetPolicy sets an optional PermissionPolicy for Rust-compatible permission
// evaluation. When set, Check delegates to the policy; when nil, the existing
// Manager logic is used as fallback.
func (m *Manager) SetPolicy(p *PermissionPolicy) {
	m.mu.Lock()
	m.policy = p
	m.mu.Unlock()
}

// Check returns the Decision for the given tool and input summary.
//
// When a PermissionPolicy is set via SetPolicy, it is used for evaluation
// (Rust-compatible path). Otherwise the legacy Go logic applies:
//
//   - BypassPermissions → always Allow
//   - Plan             → always Deny
//   - Otherwise        → consult session cache, then ruleset, then Ask
func (m *Manager) Check(tool, input string) Decision {
	m.mu.Lock()
	policy := m.policy
	m.mu.Unlock()

	if policy != nil {
		return m.checkViaPolicy(tool, input, policy, nil)
	}

	return m.checkLegacy(tool, input)
}

// CheckWithHookOverride returns the Decision for the given tool and input,
// applying a hook permission override if provided.
//
// When a PermissionPolicy is set, the hook override is translated into a
// PermissionContext and evaluated through the policy engine. Otherwise,
// hook override takes highest precedence in the legacy path.
func (m *Manager) CheckWithHookOverride(tool, input string, override *HookPermissionOverride) Decision {
	m.mu.Lock()
	policy := m.policy
	m.mu.Unlock()

	if policy != nil {
		var ctx *PermissionContext
		if override != nil {
			overrideDecision := hookDecisionToOverride(override.Decision)
			ctx = &PermissionContext{
				OverrideDecision: &overrideDecision,
				OverrideReason:   override.Reason,
			}
		}
		return m.checkViaPolicy(tool, input, policy, ctx)
	}

	// Legacy: hook override takes highest precedence.
	if override != nil {
		return override.Decision
	}
	return m.checkLegacy(tool, input)
}

// SetPrompter sets the permission prompter for interactive decisions.
func (m *Manager) SetPrompter(p PermissionPrompter) {
	m.mu.Lock()
	m.prompter = p
	m.mu.Unlock()
}

// Remember stores a user decision in the session cache for ScopeAlways grants.
// ScopeOnce decisions are not cached.
func (m *Manager) Remember(tool, _ string, decision Decision, scope Scope) {
	if scope != ScopeAlways {
		return
	}
	m.mu.Lock()
	m.cache[tool] = decision
	m.mu.Unlock()
}

// checkViaPolicy evaluates permission through the PermissionPolicy engine.
func (m *Manager) checkViaPolicy(tool, input string, policy *PermissionPolicy, ctx *PermissionContext) Decision {
	m.mu.Lock()
	prompter := m.prompter
	m.mu.Unlock()

	if ctx == nil {
		ctx = &PermissionContext{}
	}

	outcome := policy.AuthorizeWithContext(tool, input, ctx, prompter)
	if outcome.Allowed {
		return DecisionAllow
	}
	return DecisionDeny
}

// checkLegacy is the original Go permission evaluation logic.
func (m *Manager) checkLegacy(tool, input string) Decision {
	switch m.Mode {
	case ModeBypassPermissions:
		return DecisionAllow
	case ModePlan:
		return DecisionDeny
	}

	m.mu.Lock()
	if d, ok := m.cache[tool]; ok {
		m.mu.Unlock()
		return d
	}
	m.mu.Unlock()

	if d, ok := m.Rules.Match(tool, input); ok {
		return d
	}

	// AcceptEdits: auto-allow read-only and edit tools, ask for bash
	if m.Mode == ModeAcceptEdits {
		switch tool {
		case "read_file", "glob", "grep", "write_file":
			return DecisionAllow
		}
	}

	return DecisionAsk
}

// MatchesAskRule returns true if any ask-rule in the manager's ruleset matches
// the given tool name and input. This is used by the conversation loop to
// check whether a hook "allow" decision should be overridden by an ask-rule,
// matching Rust semantics where ask-rules take precedence over hook allow.
func (m *Manager) MatchesAskRule(tool, input string) bool {
	m.mu.Lock()
	policy := m.policy
	m.mu.Unlock()

	if policy != nil {
		return findMatchingRule(policy.askRules, tool, input) != nil
	}

	// Legacy path: check ruleset for ask decisions.
	d, ok := m.Rules.Match(tool, input)
	return ok && d == DecisionAsk
}

// hookDecisionToOverride maps a legacy Decision to a PermissionOverride.
func hookDecisionToOverride(d Decision) PermissionOverride {
	switch d {
	case DecisionAllow:
		return OverrideAllow
	case DecisionDeny:
		return OverrideDeny
	default:
		return OverrideAsk
	}
}
