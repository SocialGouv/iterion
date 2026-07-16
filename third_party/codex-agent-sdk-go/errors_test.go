package codexsdk

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCLINotFoundError_Creation tests CLINotFoundError creation and formatting.
func TestCLINotFoundError_Creation(t *testing.T) {
	searchedPaths := []string{
		"$PATH",
		"/usr/local/bin/claude",
		"/usr/bin/claude",
	}
	err := &CLINotFoundError{
		SearchedPaths: searchedPaths,
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "codex CLI not found")
	require.Contains(t, err.Error(), "$PATH")
	require.Contains(t, err.Error(), "/usr/local/bin/claude")
}

// TestCLIConnectionError_Creation tests CLIConnectionError creation and formatting.
func TestCLIConnectionError_Creation(t *testing.T) {
	innerErr := fmt.Errorf("connection refused")
	err := &CLIConnectionError{
		Err: innerErr,
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to connect to CLI")
	require.Contains(t, err.Error(), "connection refused")
}

// TestProcessError_WithExitCodeAndStderr tests ProcessError with exit code and stderr.
func TestProcessError_WithExitCodeAndStderr(t *testing.T) {
	err := &ProcessError{
		ExitCode: 1,
		Stderr:   "Error: authentication failed",
		Err:      nil,
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "CLI process failed")
	require.Contains(t, err.Error(), "exit 1")
	require.Contains(t, err.Error(), "authentication failed")
}

// TestMessageParseError_Creation tests MessageParseError creation and formatting.
func TestMessageParseError_Creation(t *testing.T) {
	innerErr := fmt.Errorf("invalid JSON")
	err := &MessageParseError{
		Message: `{"incomplete": `,
		Err:     innerErr,
		Data: map[string]any{
			"incomplete": true,
		},
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse message")
	require.Contains(t, err.Error(), "invalid JSON")
}

// TestMessageParseError_PreservesMessage tests that MessageParseError preserves the original message.
func TestMessageParseError_PreservesMessage(t *testing.T) {
	err := &MessageParseError{
		Message: `{"type": "unknown", "data": 123}`,
		Err:     fmt.Errorf("unknown type"),
		Data: map[string]any{
			"type": "unknown",
			"data": 123,
		},
	}

	require.Equal(t, `{"type": "unknown", "data": 123}`, err.Message)
	require.Equal(t, "unknown", err.Data["type"])
	require.Equal(t, 123, err.Data["data"])
}

// TestCLIJSONDecodeError_Creation tests CLIJSONDecodeError creation and formatting.
func TestCLIJSONDecodeError_Creation(t *testing.T) {
	innerErr := fmt.Errorf("unexpected end of JSON input")
	err := &CLIJSONDecodeError{
		RawData: `{"incomplete": `,
		Err:     innerErr,
	}

	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decode JSON from CLI")
	require.Contains(t, err.Error(), "unexpected end of JSON input")
}

// TestCLIJSONDecodeError_PreservesRawData tests that raw data is preserved.
func TestCLIJSONDecodeError_PreservesRawData(t *testing.T) {
	rawData := `{"type": "user", invalid}`
	err := &CLIJSONDecodeError{
		RawData: rawData,
		Err:     fmt.Errorf("invalid character"),
	}

	require.Equal(t, rawData, err.RawData)
	require.Contains(t, err.Error(), "invalid character")
}

// TestCLIJSONDecodeError_Unwrap tests that the underlying error can be unwrapped.
func TestCLIJSONDecodeError_Unwrap(t *testing.T) {
	innerErr := fmt.Errorf("syntax error")
	err := &CLIJSONDecodeError{
		RawData: `{bad}`,
		Err:     innerErr,
	}

	require.ErrorIs(t, err, innerErr)
}
