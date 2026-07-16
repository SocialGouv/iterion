package errors

import (
	stderrors "errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSentinelErrors_AreDistinctAndStable(t *testing.T) {
	t.Parallel()

	require.EqualError(t, ErrClientNotConnected, "client not connected")
	require.EqualError(t, ErrUnsupportedOption, "unsupported option")
	require.EqualError(t, ErrUnsupportedControlRequest, "unsupported control request")

	require.NotEqual(t, ErrClientNotConnected, ErrClientAlreadyConnected)
	require.NotEqual(t, ErrUnsupportedOption, ErrUnsupportedControlRequest)
}

func TestCLINotFoundError_ImplementsSDKError(t *testing.T) {
	t.Parallel()

	err := &CLINotFoundError{SearchedPaths: []string{"/usr/bin", "/opt/bin"}}

	var sdkErr CodexSDKError = err
	require.NotNil(t, sdkErr)
	require.True(t, err.IsCodexSDKError())
	require.Contains(t, err.Error(), "codex CLI not found")
	require.Contains(t, err.Error(), "/usr/bin")
}

func TestCLIConnectionError_WrapsCause(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("dial failed")
	err := &CLIConnectionError{Err: cause}

	require.True(t, err.IsCodexSDKError())
	require.Contains(t, err.Error(), "failed to connect to CLI")
	require.True(t, stderrors.Is(err, cause))
	require.Equal(t, cause, err.Unwrap())
}

func TestProcessError_ErrorBranchesAndUnwrap(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("exit status 2")
	errWithCause := &ProcessError{ExitCode: 2, Err: cause}
	errWithStderr := &ProcessError{ExitCode: 3, Stderr: "permission denied"}

	require.True(t, errWithCause.IsCodexSDKError())
	require.Contains(t, errWithCause.Error(), "exit 2")
	require.True(t, stderrors.Is(errWithCause, cause))
	require.Equal(t, cause, errWithCause.Unwrap())

	require.True(t, errWithStderr.IsCodexSDKError())
	require.Equal(t, "CLI process failed (exit 3): permission denied", errWithStderr.Error())
	require.Nil(t, errWithStderr.Unwrap())
}

func TestMessageParseError_WrapsCause(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("invalid payload")
	err := &MessageParseError{
		Message: "message",
		Err:     cause,
		Data:    map[string]any{"type": "unknown"},
	}

	require.True(t, err.IsCodexSDKError())
	require.Contains(t, err.Error(), "failed to parse message")
	require.True(t, stderrors.Is(err, cause))
	require.Equal(t, cause, err.Unwrap())
}

func TestCLIJSONDecodeError_WrapsCause(t *testing.T) {
	t.Parallel()

	cause := stderrors.New("unexpected token")
	err := &CLIJSONDecodeError{
		RawData: "{invalid}",
		Err:     cause,
	}

	require.True(t, err.IsCodexSDKError())
	require.Contains(t, err.Error(), "failed to decode JSON from CLI")
	require.True(t, stderrors.Is(err, cause))
	require.Equal(t, cause, err.Unwrap())
}
