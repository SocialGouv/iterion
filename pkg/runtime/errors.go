package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrorCode categorizes runtime errors for programmatic handling.
//
// It IS the persisted store.FailureCode vocabulary (a type alias, so
// values are interchangeable across the package boundary) — the same
// code the engine classifies with in-process is what lands on
// Run.FailureCode at the store, instead of dying into free text
// (ADR-095). The constants below re-export the store's values under
// the historical runtime names; codes the engine does not emit itself
// (INTERRUPTED, FAIL_NODE, PROCESS_ORPHANED, QUEUE_SCHEMA_MISMATCH)
// live only under their store.Failure* names.
type ErrorCode = store.FailureCode

const (
	ErrCodeNodeNotFound          = store.FailureNodeNotFound
	ErrCodeNoOutgoingEdge        = store.FailureNoOutgoingEdge
	ErrCodeLoopExhausted         = store.FailureLoopExhausted
	ErrCodeBudgetExceeded        = store.FailureBudgetExceeded
	ErrCodeExecutionFailed       = store.FailureExecutionFailed
	ErrCodeWorkspaceSafety       = store.FailureWorkspaceSafety
	ErrCodeTimeout               = store.FailureTimeout
	ErrCodeCancelled             = store.FailureCancelled
	ErrCodeJoinFailed            = store.FailureJoinFailed
	ErrCodeResumeInvalid         = store.FailureResumeInvalid
	ErrCodeSchemaValidation      = store.FailureSchemaValidation
	ErrCodeRateLimited           = store.FailureRateLimited
	ErrCodeUsageLimitBlocked     = store.FailureUsageLimitBlocked
	ErrCodeContextLengthExceeded = store.FailureContextLengthExceeded
	ErrCodeToolFailedTransient   = store.FailureToolFailedTransient
	ErrCodeToolFailedPermanent   = store.FailureToolFailedPermanent
	ErrCodeNetworkTransient      = store.FailureNetworkTransient
	ErrCodeAuthFailed            = store.FailureAuthFailed
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
