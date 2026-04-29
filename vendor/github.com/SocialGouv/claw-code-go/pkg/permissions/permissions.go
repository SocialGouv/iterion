// Package permissions re-exports the internal permissions package via type
// aliases so external consumers (e.g. iterion) get identity-compatible types
// without duplicating 1600+ lines of policy engine code.
package permissions

import "github.com/SocialGouv/claw-code-go/internal/permissions"

// PermissionMode represents the security level of a session or tool requirement.
type PermissionMode = permissions.PermissionMode

// Decision is the outcome of a permission check.
type Decision = permissions.Decision

// Scope controls how long a user-granted decision is remembered.
type Scope = permissions.Scope

// Rule is a single permission entry matching a tool and optionally an input pattern.
type Rule = permissions.Rule

// Ruleset is an ordered list of rules; the first match wins.
type Ruleset = permissions.Ruleset

// Manager enforces permissions for tool execution.
type Manager = permissions.Manager

// PermissionPolicy evaluates permission mode requirements plus allow/deny/ask rules.
type PermissionPolicy = permissions.PermissionPolicy

// PermissionOverride represents a hook-provided override.
type PermissionOverride = permissions.PermissionOverride

// PermissionContext provides additional permission context supplied by hooks.
type PermissionContext = permissions.PermissionContext

// PermissionRequest describes a tool requesting permission.
type PermissionRequest = permissions.PermissionRequest

// PermissionPromptDecision is the outcome of prompting the user.
type PermissionPromptDecision = permissions.PermissionPromptDecision

// PermissionOutcome is the final authorization result.
type PermissionOutcome = permissions.PermissionOutcome

// PermissionPrompter is the interface for interactive permission decisions.
type PermissionPrompter = permissions.PermissionPrompter

// HookPermissionOverride represents a hook's permission decision.
type HookPermissionOverride = permissions.HookPermissionOverride

// Classifier evaluates a tool invocation under ModeAuto and returns a Decision.
type Classifier = permissions.Classifier

// RuleClassifier is the default Classifier with a conservative read-only safe-list.
type RuleClassifier = permissions.RuleClassifier

// LLMClassifier delegates Allow/Ask/Deny decisions to a small fast model,
// short-circuiting via a Fallback Classifier (typically RuleClassifier) for
// well-known cases.
type LLMClassifier = permissions.LLMClassifier

// ClassifierCache is the in-memory TTL+FIFO cache used by LLMClassifier.
type ClassifierCache = permissions.ClassifierCache

// ClassifierLogger is the minimal logging surface Manager uses to surface
// classifier errors and panics. Set via Manager.SetClassifierLogger.
type ClassifierLogger = permissions.ClassifierLogger

const (
	ModeReadOnly         = permissions.ModeReadOnly
	ModeWorkspaceWrite   = permissions.ModeWorkspaceWrite
	ModeDangerFullAccess = permissions.ModeDangerFullAccess
	ModePrompt           = permissions.ModePrompt
	ModeAllow            = permissions.ModeAllow

	// CLI-facing aliases.
	ModeDefault           = permissions.ModeDefault
	ModeAcceptEdits       = permissions.ModeAcceptEdits
	ModeBypassPermissions = permissions.ModeBypassPermissions
	ModePlan              = permissions.ModePlan
)

const (
	DecisionAsk   = permissions.DecisionAsk
	DecisionAllow = permissions.DecisionAllow
	DecisionDeny  = permissions.DecisionDeny
)

const (
	ScopeOnce   = permissions.ScopeOnce
	ScopeAlways = permissions.ScopeAlways
)

const (
	OverrideAllow = permissions.OverrideAllow
	OverrideDeny  = permissions.OverrideDeny
	OverrideAsk   = permissions.OverrideAsk
)

// NewManager creates a Manager with the given mode and ruleset.
var NewManager = permissions.NewManager

// ParsePermissionMode converts a CLI string to a PermissionMode.
var ParsePermissionMode = permissions.ParsePermissionMode

// LoadRuleset reads .claude/settings.json and returns a Ruleset.
var LoadRuleset = permissions.LoadRuleset

// RulesetFromLists builds a Ruleset from plain allow/deny tool name slices.
var RulesetFromLists = permissions.RulesetFromLists

// NewPermissionPolicy creates a policy with the given active mode.
var NewPermissionPolicy = permissions.NewPermissionPolicy

// NewPermissionContext creates a context with optional override.
var NewPermissionContext = permissions.NewPermissionContext

// NewRuleClassifier returns a RuleClassifier with the documented default safe-list.
var NewRuleClassifier = permissions.NewRuleClassifier

// NewClassifierCache builds a TTL+FIFO classifier cache.
var NewClassifierCache = permissions.NewClassifierCache
