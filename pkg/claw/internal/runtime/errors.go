package runtime

import "fmt"

// SessionErrorKind classifies the category of a session error.
type SessionErrorKind int

const (
	// SessionErrIO indicates a filesystem I/O error.
	SessionErrIO SessionErrorKind = iota
	// SessionErrJSON indicates a JSON serialization/deserialization error.
	SessionErrJSON
	// SessionErrInvalidFormat indicates the session data has an invalid or unexpected format.
	SessionErrInvalidFormat
)

func (k SessionErrorKind) String() string {
	switch k {
	case SessionErrIO:
		return "io"
	case SessionErrJSON:
		return "json"
	case SessionErrInvalidFormat:
		return "invalid_format"
	default:
		return "unknown"
	}
}

// SessionError is a structured error for session operations.
type SessionError struct {
	Kind    SessionErrorKind
	Op      string
	Path    string
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *SessionError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("session %s [%s]: %s", e.Op, e.Path, e.Message)
	}
	return fmt.Sprintf("session %s: %s", e.Op, e.Message)
}

// Unwrap returns the underlying cause, enabling errors.Is and errors.As.
func (e *SessionError) Unwrap() error {
	return e.Cause
}

// NewSessionIOError creates a SessionError for I/O failures.
func NewSessionIOError(op, path string, cause error) *SessionError {
	msg := "i/o error"
	if cause != nil {
		msg = cause.Error()
	}
	return &SessionError{
		Kind:    SessionErrIO,
		Op:      op,
		Path:    path,
		Message: msg,
		Cause:   cause,
	}
}

// NewSessionJSONError creates a SessionError for JSON failures.
func NewSessionJSONError(op string, cause error) *SessionError {
	msg := "json error"
	if cause != nil {
		msg = cause.Error()
	}
	return &SessionError{
		Kind:    SessionErrJSON,
		Op:      op,
		Message: msg,
		Cause:   cause,
	}
}

// NewSessionFormatError creates a SessionError for invalid format.
func NewSessionFormatError(msg string) *SessionError {
	return &SessionError{
		Kind:    SessionErrInvalidFormat,
		Op:      "parse",
		Message: msg,
	}
}
