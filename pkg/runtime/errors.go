package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// ErrorCode categorizes runtime errors for programmatic handling.
type ErrorCode string

const (
	ErrCodeNodeNotFound     ErrorCode = "NODE_NOT_FOUND"
	ErrCodeNoOutgoingEdge   ErrorCode = "NO_OUTGOING_EDGE"
	ErrCodeLoopExhausted    ErrorCode = "LOOP_EXHAUSTED"
	ErrCodeBudgetExceeded   ErrorCode = "BUDGET_EXCEEDED"
	ErrCodeExecutionFailed  ErrorCode = "EXECUTION_FAILED"
	ErrCodeWorkspaceSafety  ErrorCode = "WORKSPACE_SAFETY"
	ErrCodeTimeout          ErrorCode = "TIMEOUT"
	ErrCodeCancelled        ErrorCode = "CANCELLED"
	ErrCodeJoinFailed       ErrorCode = "JOIN_FAILED"
	ErrCodeResumeInvalid    ErrorCode = "RESUME_INVALID"
	ErrCodeSchemaValidation ErrorCode = "SCHEMA_VALIDATION"
	ErrCodeRateLimited      ErrorCode = "RATE_LIMITED"
	// ErrCodeUsageLimitBlocked: the provider's subscription/quota WINDOW
	// is exhausted (Anthropic forfait 5h / session / weekly cap) —
	// distinct from ErrCodeRateLimited because retrying inside the
	// window can never succeed: the only cure is waiting for the reset.
	// In-node recovery fails terminal immediately; the run lands
	// failed_resumable and the run-level auto-resume loop waits with a
	// reset-aware delay (see pkg/cli/auto_resume.go).
	ErrCodeUsageLimitBlocked     ErrorCode = "USAGE_LIMIT_BLOCKED"
	ErrCodeContextLengthExceeded ErrorCode = "CONTEXT_LENGTH_EXCEEDED"
	ErrCodeToolFailedTransient   ErrorCode = "TOOL_FAILED_TRANSIENT"
	ErrCodeToolFailedPermanent   ErrorCode = "TOOL_FAILED_PERMANENT"
	// ErrCodeNetworkTransient: occasional ISP / DNS / TCP / TLS hiccup
	// reaching the upstream model API. Distinct from ErrCodeExecutionFailed
	// so the recovery dispatcher can apply a longer exponential-backoff
	// budget — a 2-second single retry is plenty for "stale token" or
	// "race on the tool subprocess", but useless against a 30-second
	// captive-portal handoff or a multi-minute datacenter routing blip.
	// Surfaced via Classify when the error string matches a known
	// network-failure phrase (FailedToOpenSocket, "Unable to connect to
	// API", "no such host", "connection refused", "i/o timeout", etc.).
	ErrCodeNetworkTransient ErrorCode = "NETWORK_TRANSIENT"
	// ErrCodeAuthFailed: the upstream model provider rejected the
	// request for credential reasons (HTTP 401/403, "authentication
	// token is expired", "invalid api key", …). NOT transient — retrying
	// the same call can never succeed until a human re-authenticates
	// (e.g. `codex login` for the ChatGPT-forfait OAuth token, or
	// rotating an API key). The recovery dispatcher pauses for human
	// instead of burning the retry budget; the run is resumable once the
	// credential is refreshed.
	ErrCodeAuthFailed ErrorCode = "AUTH_FAILED"
)

// RuntimeError is a structured error carrying a machine-readable code,
// the node where the error occurred, and a human-friendly hint for
// resolution. It implements the error interface and can wrap an
// underlying cause.
type RuntimeError struct {
	Code    ErrorCode // machine-readable error category
	Message string    // human-readable description
	NodeID  string    // node where the error originated (may be empty)
	Hint    string    // suggested resolution for the user
	Cause   error     // underlying error (may be nil)
}

func (e *RuntimeError) Error() string {
	s := fmt.Sprintf("[%s] %s", e.Code, e.Message)
	if e.NodeID != "" {
		s += fmt.Sprintf(" (node: %s)", e.NodeID)
	}
	if e.Cause != nil {
		s += fmt.Sprintf(": %v", e.Cause)
	}
	return s
}

func (e *RuntimeError) Unwrap() error { return e.Cause }

// ---------------------------------------------------------------------------
// Recovery dispatch surface
// ---------------------------------------------------------------------------

// RecoveryActionKind enumerates how the engine should handle a node
// failure.
type RecoveryActionKind int

const (
	RecoveryRetrySameNode RecoveryActionKind = iota
	// RecoveryCompactAndRetry: the engine asks the executor to drop
	// older conversation turns first; falls back to a plain retry when
	// the executor does not implement Compactor.
	RecoveryCompactAndRetry
	// RecoveryPauseForHuman writes a synthetic interaction so the run
	// is resumable via `iterion resume --answers-file`.
	RecoveryPauseForHuman
	// RecoveryFailTerminal still produces a checkpoint (failRunWithCheckpoint),
	// just no further retries.
	RecoveryFailTerminal
)

// RecoveryAction is the engine-facing decision returned by a
// RecoveryDispatch. The zero value (RecoveryRetrySameNode with no
// delay, no attempts left) is safe to apply.
type RecoveryAction struct {
	Kind         RecoveryActionKind
	Delay        time.Duration
	AttemptsLeft int
	Reason       string
}

// RecoveryDispatch is the callback consulted by the engine when a node
// execution returns an error. The engine passes a `priorAttempts`
// resolver so the dispatcher can classify the error first and only
// then look up the per-class attempt count — avoiding a redundant
// double-call. Implementations classify, look up the recipe, and
// return the action together with the matched ErrorCode (so the
// engine can bucket attempt counts on runState).
//
// Implementations live in runtime/recovery so they don't cycle back
// into runtime; this signature is the only contract the engine cares
// about.
type RecoveryDispatch func(ctx context.Context, err error, priorAttempts func(ErrorCode) int) (RecoveryAction, ErrorCode)

// Compactor is an optional executor capability surfaced for
// RecoveryCompactAndRetry. Backends that can drop older conversation
// turns (e.g. claw's ConversationLoop.Compact) implement it
// structurally; the engine falls back to a plain retry when the
// underlying executor does not. Compact may return
// model.ErrCompactionUnsupported to signal an architectural no-op
// without alarming the operator.
type Compactor interface {
	Compact(ctx context.Context, nodeID string) error
}

// ErrCompactionUnsupported is re-exported from the model package so
// runtime callers can match on it without importing model directly.
// This is a const alias — the canonical sentinel lives in model/.
var ErrCompactionUnsupported = model.ErrCompactionUnsupported
