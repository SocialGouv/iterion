package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStdioServerConfig_GetType(t *testing.T) {
	t.Parallel()

	t.Run("defaults to stdio", func(t *testing.T) {
		t.Parallel()

		cfg := &StdioServerConfig{
			Command: "cat",
		}
		require.Equal(t, ServerTypeStdio, cfg.GetType())
	})

	t.Run("uses explicit type", func(t *testing.T) {
		t.Parallel()

		serverType := ServerTypeSDK
		cfg := &StdioServerConfig{
			Type: &serverType,
		}
		require.Equal(t, ServerTypeSDK, cfg.GetType())
	})
}

func TestServerConfig_GetType(t *testing.T) {
	t.Parallel()

	require.Equal(t, ServerTypeSSE, (&SSEServerConfig{Type: ServerTypeSSE}).GetType())
	require.Equal(t, ServerTypeHTTP, (&HTTPServerConfig{Type: ServerTypeHTTP}).GetType())
	require.Equal(t, ServerTypeSDK, (&SdkServerConfig{Type: ServerTypeSDK}).GetType())
}
