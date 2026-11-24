package hooks

// HookEvent represents the lifecycle stage of a hook invocation.
type HookEvent string

const (
	PreToolUse         HookEvent = "PreToolUse"
	PostToolUse        HookEvent = "PostToolUse"
	PostToolUseFailure HookEvent = "PostToolUseFailure"
)

// PermissionDecision is the permission override from a hook response.
type PermissionDecision string

const (
	PermissionAllow PermissionDecision = "allow"
	PermissionDeny  PermissionDecision = "deny"
	PermissionAsk   PermissionDecision = "ask"
)

// HookRunResult is the aggregated result of running all hooks for an event.
type HookRunResult struct {
	Denied             bool
	Failed             bool
	Cancelled          bool
	Messages           []string
	PermissionOverride *PermissionDecision // nil if no override
	PermissionReason   string
	UpdatedInput       string // empty if no update; JSON string of modified input
}

// Allow creates a result indicating all hooks passed.
func Allow(messages []string) HookRunResult {
	if messages == nil {
		messages = []string{}
	}
	return HookRunResult{
		Messages: messages,
	}
}

// IsDenied returns true if the hook result represents a denial.
func (r *HookRunResult) IsDenied() bool {
	return r.Denied
}

// IsFailed returns true if the hook result represents a failure.
func (r *HookRunResult) IsFailed() bool {
	return r.Failed
}

// IsCancelled returns true if the hook result represents a cancellation.
func (r *HookRunResult) IsCancelled() bool {
	return r.Cancelled
}
