package permissions

import (
	"context"
	"sync"
)

// ClassifierLogger is the minimal logging surface Manager uses to surface
// classifier errors and panics without coupling the permissions package to
// a concrete logger. Set via SetClassifierLogger; nil means silent.
type ClassifierLogger interface {
	Warn(format string, args ...any)
}

// Manager enforces permissions for tool execution according to a mode and ruleset.
type Manager struct {
	Mode  PermissionMode
	Rules *Ruleset

	mu               sync.Mutex
	cache            map[string]Decision // keyed by tool name for ScopeAlways grants
	prompter         PermissionPrompter  // optional; set via SetPrompter()
	policy           *PermissionPolicy   // optional; when set, Check delegates here
	classifier       Classifier          // optional; consulted between policy and legacy
	classifierLogger ClassifierLogger    // optional; logs classifier errors/panics
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

// SetClassifier registers an optional Classifier consulted between the
// policy path and the legacy path. When set and the classifier returns
// DecisionAllow or DecisionDeny, that decision wins. DecisionAsk (or any
// classifier error) falls through to the legacy logic. The policy path
// takes precedence over the classifier.
//
// Pass nil to clear a previously installed classifier.
func (m *Manager) SetClassifier(c Classifier) {
	m.mu.Lock()
	m.classifier = c
	m.mu.Unlock()
}

// SetClassifierLogger installs an optional logger for classifier errors
// and recovered panics. Nil clears the logger (silent). Logging is
// best-effort — classifier errors still fall through to the legacy
// path, the logger only surfaces them for debugging.
func (m *Manager) SetClassifierLogger(l ClassifierLogger) {
	m.mu.Lock()
	m.classifierLogger = l
	m.mu.Unlock()
}

// Check returns the Decision for the given tool and input summary.
//
// Resolution order:
//
//  1. A PermissionPolicy set via SetPolicy (Rust-compatible path).
//  2. A Classifier set via SetClassifier (Allow/Deny short-circuit;
//     Ask falls through).
//  3. The legacy Go logic:
//     - BypassPermissions → always Allow
//     - Plan             → always Deny
//     - Otherwise        → consult session cache, then ruleset, then Ask.
//
// Check is preserved for backward compatibility and forwards to CheckCtx
// with context.Background. New callers should prefer CheckCtx so the
// classifier can honour cancellation and deadlines.
func (m *Manager) Check(tool, input string) Decision {
	return m.CheckCtx(context.Background(), tool, input)
}

// CheckCtx is the context-aware variant of Check. The provided context
// is forwarded to the classifier so LLM-backed implementations can be
// cancelled or honour deadlines.
func (m *Manager) CheckCtx(ctx context.Context, tool, input string) Decision {
	m.mu.Lock()
	policy := m.policy
	classifier := m.classifier
	m.mu.Unlock()

	if policy != nil {
		return m.checkViaPolicy(tool, input, policy, nil)
	}

	if d, ok := m.checkViaClassifier(ctx, classifier, tool, input); ok {
		return d
	}

	return m.checkLegacy(tool, input)
}

// CheckWithHookOverride returns the Decision for the given tool and input,
// applying a hook permission override if provided.
//
// When a PermissionPolicy is set, the hook override is translated into a
// PermissionContext and evaluated through the policy engine. Otherwise,
// hook override takes highest precedence; a registered Classifier is
// consulted next, with the legacy path as final fallback.
//
// Preserved for backward compatibility — see CheckWithHookOverrideCtx
// for the context-aware variant.
func (m *Manager) CheckWithHookOverride(tool, input string, override *HookPermissionOverride) Decision {
	return m.CheckWithHookOverrideCtx(context.Background(), tool, input, override)
}

// CheckWithHookOverrideCtx is the context-aware variant of
// CheckWithHookOverride. The provided context is forwarded to the
// classifier (when consulted).
func (m *Manager) CheckWithHookOverrideCtx(ctx context.Context, tool, input string, override *HookPermissionOverride) Decision {
	m.mu.Lock()
	policy := m.policy
	classifier := m.classifier
	m.mu.Unlock()

	if policy != nil {
		var pctx *PermissionContext
		if override != nil {
			overrideDecision := hookDecisionToOverride(override.Decision)
			pctx = &PermissionContext{
				OverrideDecision: &overrideDecision,
				OverrideReason:   override.Reason,
			}
		}
		return m.checkViaPolicy(tool, input, policy, pctx)
	}

	// Legacy: hook override takes highest precedence.
	if override != nil {
		return override.Decision
	}

	if d, ok := m.checkViaClassifier(ctx, classifier, tool, input); ok {
		return d
	}

	return m.checkLegacy(tool, input)
}

// checkViaClassifier consults the classifier (if any) and returns the
// decision plus a "decided" flag. A nil classifier or DecisionAsk
// (including classifier errors) returns ok=false so the caller can fall
// through to the next resolution stage.
//
// The provided ctx is forwarded to the classifier so LLM-backed
// implementations can honour cancellation and deadlines. A panic from a
// third-party classifier is recovered (logged when a classifier logger
// is configured) and treated as DecisionAsk so a misbehaving classifier
// cannot crash the manager.
func (m *Manager) checkViaClassifier(ctx context.Context, c Classifier, tool, input string) (decision Decision, decided bool) {
	if c == nil {
		return DecisionAsk, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	args := parseToolArgs(input)

	defer func() {
		if r := recover(); r != nil {
			m.logClassifierIssue("classifier panic for tool %q: %v", tool, r)
			decision = DecisionAsk
			decided = false
		}
	}()

	d, err := c.Classify(ctx, tool, args)
	if err != nil {
		m.logClassifierIssue("classifier error for tool %q: %v", tool, err)
		return DecisionAsk, false
	}
	if d == DecisionAsk {
		return DecisionAsk, false
	}
	return d, true
}

// logClassifierIssue is a best-effort warn helper that no-ops when no
// logger has been configured.
func (m *Manager) logClassifierIssue(format string, args ...any) {
	m.mu.Lock()
	logger := m.classifierLogger
	m.mu.Unlock()
	if logger == nil {
		return
	}
	logger.Warn(format, args...)
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
