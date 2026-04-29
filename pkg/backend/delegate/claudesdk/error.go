package claudesdk

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for use with [errors.Is].
var (
	// ErrCLINotFound indicates the claude CLI binary was not found.
	ErrCLINotFound = errors.New("claude: CLI not found")

	// ErrSessionClosed indicates an operation on a closed session.
	ErrSessionClosed = errors.New("claude: session closed")

	// ErrEmptyPrompt indicates an empty prompt was provided.
	ErrEmptyPrompt = errors.New("claude: empty prompt")
)

// ProcessError wraps an error from the CLI subprocess.
type ProcessError struct {
	ExitCode int
	Stderr   string
	Err      error
}

func (e *ProcessError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("claude: process exited with code %d: %s", e.ExitCode, e.Stderr)
	}
	return fmt.Sprintf("claude: process exited with code %d", e.ExitCode)
}

func (e *ProcessError) Unwrap() error { return e.Err }

// ParseError wraps a JSON parsing error with the raw bytes that failed.
type ParseError struct {
	Raw []byte
	Err error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("claude: parse error: %v (raw: %s)", e.Err, e.Raw)
}

func (e *ParseError) Unwrap() error { return e.Err }

// PermissionDeniedError indicates a tool use was denied.
type PermissionDeniedError struct {
	ToolName string
	Reason   string
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("claude: permission denied for tool %q: %s", e.ToolName, e.Reason)
}

// VersionError indicates a CLI version mismatch.
type VersionError struct {
	Got  string
	Want string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("claude: version mismatch: got %s, want %s", e.Got, e.Want)
}

// cliNotFoundError contains the paths that were searched.
type cliNotFoundError struct {
	searched []string
}

func (e *cliNotFoundError) Error() string {
	return "claude: CLI not found (searched: " + strings.Join(e.searched, ", ") + ")"
}
