package runtime

import (
	"errors"
	"fmt"
)

// LoopErrorKind classifies the category of a loop adapter error.
type LoopErrorKind int

const (
	// LoopErrSessionNotFound indicates the requested session does not exist.
	LoopErrSessionNotFound LoopErrorKind = iota
	// LoopErrNotConnected indicates a required subsystem (e.g. LSP) is not connected.
	LoopErrNotConnected
	// LoopErrSubsystemUnavailable indicates a subsystem is nil or not initialized.
	LoopErrSubsystemUnavailable
	// LoopErrInvalidArgs indicates the command received invalid arguments.
	LoopErrInvalidArgs
	// LoopErrTurnActive indicates a mutating command was rejected because a turn is active.
	LoopErrTurnActive
)

func (k LoopErrorKind) String() string {
	switch k {
	case LoopErrSessionNotFound:
		return "session_not_found"
	case LoopErrNotConnected:
		return "not_connected"
	case LoopErrSubsystemUnavailable:
		return "subsystem_unavailable"
	case LoopErrInvalidArgs:
		return "invalid_args"
	case LoopErrTurnActive:
		return "turn_active"
	default:
		return "unknown"
	}
}

// LoopError is a structured error for loop adapter operations.
type LoopError struct {
	Kind      LoopErrorKind
	Subsystem string // e.g. "session", "lsp", "usage", "config"
	Message   string
	Cause     error
}

// Error implements the error interface.
func (e *LoopError) Error() string {
	if e.Subsystem != "" {
		return fmt.Sprintf("loop %s [%s]: %s", e.Subsystem, e.Kind, e.Message)
	}
	return fmt.Sprintf("loop [%s]: %s", e.Kind, e.Message)
}

// Unwrap returns the underlying cause, enabling errors.Is and errors.As.
func (e *LoopError) Unwrap() error {
	return e.Cause
}

// Sentinel errors for use with errors.Is.
var (
	// ErrSessionNotFound is a sentinel for session lookup failures.
	ErrSessionNotFound = &LoopError{Kind: LoopErrSessionNotFound, Message: "session not found"}
	// ErrNotConnected is a sentinel for unconnected subsystems (e.g. LSP).
	ErrNotConnected = &LoopError{Kind: LoopErrNotConnected, Message: "not connected"}
	// ErrSubsystemUnavailable is a sentinel for nil/uninitialized subsystems.
	ErrSubsystemUnavailable = &LoopError{Kind: LoopErrSubsystemUnavailable, Message: "subsystem unavailable"}
	// ErrTurnActive is a sentinel for rejecting mutating commands during active turns.
	ErrTurnActive = &LoopError{Kind: LoopErrTurnActive, Message: "cannot mutate session while a turn is active"}
)

// NewLoopError creates a LoopError with the given kind, subsystem, and message.
func NewLoopError(kind LoopErrorKind, subsystem, msg string) *LoopError {
	return &LoopError{Kind: kind, Subsystem: subsystem, Message: msg}
}

// WrapLoopError creates a LoopError wrapping an underlying cause.
func WrapLoopError(kind LoopErrorKind, subsystem, msg string, cause error) *LoopError {
	return &LoopError{Kind: kind, Subsystem: subsystem, Message: msg, Cause: cause}
}

// LoopErrorKindOf extracts the LoopErrorKind from err if it is a *LoopError.
// Returns (kind, true) if found, or (0, false) otherwise.
func LoopErrorKindOf(err error) (LoopErrorKind, bool) {
	var le *LoopError
	if errors.As(err, &le) {
		return le.Kind, true
	}
	return 0, false
}
