package runtime

import "fmt"

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
