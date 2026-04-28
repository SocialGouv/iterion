package permissions

// PermissionOverride represents a hook-provided override applied before standard
// permission evaluation. Matches Rust's PermissionOverride enum.
type PermissionOverride int

const (
	OverrideAllow PermissionOverride = iota
	OverrideDeny
	OverrideAsk
)

// PermissionContext provides additional permission context supplied by hooks
// or higher-level orchestration. Matches Rust's PermissionContext struct.
type PermissionContext struct {
	OverrideDecision *PermissionOverride
	OverrideReason   string
}

// NewPermissionContext creates a context with optional override.
func NewPermissionContext(decision *PermissionOverride, reason string) PermissionContext {
	return PermissionContext{
		OverrideDecision: decision,
		OverrideReason:   reason,
	}
}

// PermissionRequest describes a tool requesting permission.
// Both CurrentMode and RequiredMode use PermissionMode (matching Rust's
// PermissionRequest where both fields are PermissionMode).
type PermissionRequest struct {
	ToolName     string
	Input        string
	CurrentMode  PermissionMode
	RequiredMode PermissionMode
	Reason       string
}

// PermissionPromptDecision is the outcome of prompting the user.
type PermissionPromptDecision struct {
	Allowed bool
	Reason  string // populated when denied
}

// PermissionOutcome is the final authorization result after evaluating static
// rules and prompts. Matches Rust's PermissionOutcome enum.
type PermissionOutcome struct {
	Allowed bool
	Reason  string // populated when denied
}

// PermissionPrompter is the interface for interactively asking the user
// for permission decisions. Implementations should be synchronous;
// callers wrap in goroutines if async behavior is needed.
// Matches Rust's PermissionPrompter trait.
type PermissionPrompter interface {
	// Decide presents a permission request to the user and returns their decision.
	Decide(req *PermissionRequest) PermissionPromptDecision
}

// HookPermissionOverride represents a permission decision made by a hook,
// which can override the normal permission flow.
type HookPermissionOverride struct {
	Decision Decision // Allow, Deny, or Ask
	Reason   string
}
