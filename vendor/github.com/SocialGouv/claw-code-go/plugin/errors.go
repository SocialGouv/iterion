package plugin

import "fmt"

// ErrorKind classifies plugin errors.
type ErrorKind string

const (
	ErrIO                 ErrorKind = "io"
	ErrJSON               ErrorKind = "json"
	ErrManifestValidation ErrorKind = "manifest_validation"
	ErrLoadFailures       ErrorKind = "load_failures"
	ErrInvalidManifest    ErrorKind = "invalid_manifest"
	ErrNotFound           ErrorKind = "not_found"
	ErrCommandFailed      ErrorKind = "command_failed"
)

// PluginError is a structured plugin error.
type PluginError struct {
	Kind    ErrorKind
	Message string
	Cause   error
}

func (e *PluginError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Kind, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Kind, e.Message)
}

func (e *PluginError) Unwrap() error {
	return e.Cause
}

// ValidationError represents a single manifest validation issue.
type ValidationError struct {
	Code    string // e.g., "empty_field", "duplicate_permission", "unsupported_contract"
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// LoadFailure records a failed plugin load attempt.
type LoadFailure struct {
	PluginRoot string
	Kind       PluginKind
	Source     string
	Err        error
}

func (f *LoadFailure) Error() string {
	return fmt.Sprintf("failed to load %s plugin from %s (%s): %v", f.Kind, f.PluginRoot, f.Source, f.Err)
}
