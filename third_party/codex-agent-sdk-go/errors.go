package codexsdk

import "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"

// CLINotFoundError indicates the Codex CLI binary was not found.
type CLINotFoundError = errors.CLINotFoundError

// CLIConnectionError indicates failure to connect to the CLI.
type CLIConnectionError = errors.CLIConnectionError

// ProcessError indicates the CLI process failed.
type ProcessError = errors.ProcessError

// MessageParseError indicates message parsing failed.
type MessageParseError = errors.MessageParseError

// CLIJSONDecodeError indicates JSON parsing failed for CLI output.
type CLIJSONDecodeError = errors.CLIJSONDecodeError

// CodexSDKError is the base interface for all SDK errors.
type CodexSDKError = errors.CodexSDKError

// Sentinel errors for commonly checked conditions.
var (
	// ErrClientNotConnected indicates the client is not connected.
	ErrClientNotConnected = errors.ErrClientNotConnected

	// ErrClientAlreadyConnected indicates the client is already connected.
	ErrClientAlreadyConnected = errors.ErrClientAlreadyConnected

	// ErrClientClosed indicates the client has been closed and cannot be reused.
	ErrClientClosed = errors.ErrClientClosed

	// ErrTransportNotConnected indicates the transport is not connected.
	ErrTransportNotConnected = errors.ErrTransportNotConnected

	// ErrRequestTimeout indicates a request timed out.
	ErrRequestTimeout = errors.ErrRequestTimeout

	// ErrUnsupportedOption indicates an option is not supported by the selected backend.
	ErrUnsupportedOption = errors.ErrUnsupportedOption

	// ErrUnsupportedControlRequest indicates a control request subtype is not supported.
	ErrUnsupportedControlRequest = errors.ErrUnsupportedControlRequest

	// ErrSessionNotFound indicates the requested session was not found.
	ErrSessionNotFound = errors.ErrSessionNotFound
)
