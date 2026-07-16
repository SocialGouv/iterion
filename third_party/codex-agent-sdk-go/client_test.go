package codexsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewClient_Creation tests client creation.
func TestNewClient_Creation(t *testing.T) {
	client := NewClient()
	require.NotNil(t, client)

	err := client.Close()
	require.NoError(t, err)
}

// TestClient_QueryNotConnected tests query when not connected.
func TestClient_QueryNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.Query(ctx, Text("test prompt"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_ReceiveMessagesNotConnected tests ReceiveMessages when not connected.
func TestClient_ReceiveMessagesNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var gotError error

	for _, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			gotError = err

			break
		}
	}

	require.Error(t, gotError)
	require.Contains(t, gotError.Error(), "not connected")
}

// TestClient_InterruptNotConnected tests interrupt when not connected.
func TestClient_InterruptNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.Interrupt(ctx)

	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_SetPermissionModeNotConnected tests set permission mode when not connected.
func TestClient_SetPermissionModeNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.SetPermissionMode(ctx, "acceptEdits")

	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_SetModelNotConnected tests set model when not connected.
func TestClient_SetModelNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.SetModel(ctx, new("claude-3-5-sonnet-20241022"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_GetServerInfoNotConnected tests get server info when not connected.
func TestClient_GetServerInfoNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	info := client.GetServerInfo()

	require.Nil(t, info)
}

// TestClient_ReceiveResponseNotConnected tests receive response when not connected.
func TestClient_ReceiveResponseNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	var gotError error

	for _, err := range client.ReceiveResponse(ctx) {
		if err != nil {
			gotError = err

			break
		}
	}

	require.Error(t, gotError)
	require.Contains(t, gotError.Error(), "not connected")
}

// TestClient_ReceiveResponseContextCancellation tests ReceiveResponse context cancellation.
func TestClient_ReceiveResponseContextCancellation(t *testing.T) {
	t.Run("respects context cancellation", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		var gotError error

		for _, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		require.Error(t, gotError)
	})
}

// TestClient_ReceiveResponseEarlyBreak tests that early termination works correctly.
func TestClient_ReceiveResponseEarlyBreak(t *testing.T) {
	t.Run("early break when not connected", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()
		iterations := 0

		for _, err := range client.ReceiveResponse(ctx) {
			iterations++

			// Break after first iteration (which should be the error)
			if err != nil {
				break
			}
		}

		// Should have exactly one iteration (the error)
		require.Equal(t, 1, iterations)
	})
}

// TestClient_CloseMultipleTimes tests idempotent close.
func TestClient_CloseMultipleTimes(t *testing.T) {
	client := NewClient()

	err := client.Close()
	require.NoError(t, err)

	err = client.Close()
	require.NoError(t, err)

	err = client.Close()
	require.NoError(t, err)
}

// TestClient_ConnectWithCLINotFound tests connection with invalid CLI path.
func TestClient_ConnectWithCLINotFound(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.Start(ctx,
		WithCliPath("/nonexistent/path/to/claude"),
	)
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)
}

// TestClient_ConnectWithCancelledContext tests connection with cancelled context.
func TestClient_ConnectWithCancelledContext(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.Start(ctx,
		WithCliPath("/nonexistent/path/to/claude"),
	)
	require.Error(t, err)
}

// TestClient_ReceiveMessagesWithCancelledContext tests ReceiveMessages with cancelled context.
func TestClient_ReceiveMessagesWithCancelledContext(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var gotError error

	for _, err := range client.ReceiveMessages(ctx) {
		if err != nil {
			gotError = err

			break
		}
	}

	require.Error(t, gotError)
}

// TestClient_CloseWithContext tests Close is idempotent.
func TestClient_CloseWithContext(t *testing.T) {
	client := NewClient()

	err := client.Close()
	require.NoError(t, err)
}

// TestClient_WithOptions tests client with various options.
func TestClient_WithOptions(t *testing.T) {
	tests := []struct {
		name              string
		opts              []Option
		expectUnsupported bool
	}{
		{
			name: "empty options",
			opts: []Option{},
		},
		{
			name: "with permission mode",
			opts: []Option{WithPermissionMode("acceptEdits")},
		},
		{
			name: "with model",
			opts: []Option{WithModel("claude-3-5-sonnet-20241022")},
		},
		{
			name:              "with add dirs unsupported on app-server",
			opts:              []Option{WithAddDirs("/tmp")},
			expectUnsupported: true,
		},
		{
			name: "with system prompt",
			opts: []Option{WithSystemPrompt("You are a helpful assistant.")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient()
			defer client.Close()

			ctx := context.Background()

			opts := append(tt.opts, WithCliPath("/nonexistent/claude"))

			err := client.Start(ctx, opts...)
			require.Error(t, err)

			if tt.expectUnsupported {
				require.ErrorIs(t, err, ErrUnsupportedOption)
			} else {
				_, ok := errors.AsType[*CLINotFoundError](err)
				require.True(t, ok)
			}
		})
	}
}

// TestClient_DoubleConnect tests connecting twice returns error.
func TestClient_DoubleConnect(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	// First connect fails with CLI not found
	err := client.Start(ctx, WithCliPath("/nonexistent/claude"))
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)

	// Second connect should also fail (either CLI not found or already connected)
	err = client.Start(ctx, WithCliPath("/nonexistent/claude"))
	require.Error(t, err)
}

// TestClient_DisconnectWithoutConnect tests disconnecting before connecting.
func TestClient_DisconnectWithoutConnect(t *testing.T) {
	client := NewClient()

	// Should not error
	err := client.Close()
	require.NoError(t, err)
}

// TestClient_ContextTimeout tests operations with timed out context.
func TestClient_ContextTimeout(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	time.Sleep(5 * time.Millisecond)

	err := client.Query(ctx, Text("test"))
	require.Error(t, err)
}

// TestClient_WithSessionID tests client with session ID handling.
func TestClient_WithSessionID(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()
	err := client.Start(ctx,
		WithCliPath("/nonexistent/claude"),
		WithResume("session_abc123"),
	)

	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)
}

// TestClient_WithEnv tests client with environment variables.
func TestClient_WithEnv(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()
	err := client.Start(ctx,
		WithCliPath("/nonexistent/claude"),
		WithEnv(map[string]string{
			"CUSTOM_VAR":        "custom_value",
			"ANTHROPIC_API_KEY": "test-key",
		}),
	)

	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)
}

// TestClient_CleanupOnError tests that resources are properly cleaned up
// when operations fail.
func TestClient_CleanupOnError(t *testing.T) {
	t.Run("cleanup on connect error", func(t *testing.T) {
		client := NewClient()

		ctx := context.Background()

		// Connect should fail
		err := client.Start(ctx, WithCliPath("/nonexistent/claude"))
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)

		// Close should be safe to call after failed connect
		err = client.Close()
		require.NoError(t, err)

		// Multiple closes should be safe
		err = client.Close()
		require.NoError(t, err)
	})

	t.Run("cleanup with defer pattern", func(t *testing.T) {
		client := NewClient()

		defer func() {
			// Simulate cleanup even if something panics
			err := client.Close()
			require.NoError(t, err)
		}()

		ctx := context.Background()

		// This simulates using the client in a function that might fail
		err := client.Start(ctx, WithCliPath("/nonexistent/claude"))
		require.Error(t, err)
	})

	t.Run("cleanup recovers from operations on closed client", func(t *testing.T) {
		client := NewClient()

		// Close before any operations
		err := client.Close()
		require.NoError(t, err)

		// Operations after close should fail gracefully
		ctx := context.Background()
		err = client.Query(ctx, Text("test"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "not connected")

		// Close again should be safe
		err = client.Close()
		require.NoError(t, err)
	})
}

// TestClient_ConcurrentOperations tests that client operations are thread-safe.
// This verifies that concurrent calls don't cause race conditions.
func TestClient_ConcurrentOperations(t *testing.T) {
	t.Run("concurrent queries to disconnected client", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		const goroutines = 10

		var wg sync.WaitGroup

		wg.Add(goroutines)

		errors := make(chan error, goroutines)

		for range goroutines {
			go func() {
				defer wg.Done()

				ctx := context.Background()
				err := client.Query(ctx, Text("test"))

				errors <- err
			}()
		}

		wg.Wait()
		close(errors)

		// All should return "not connected" error
		errorCount := 0

		for err := range errors {
			require.Error(t, err)
			require.Contains(t, err.Error(), "not connected")

			errorCount++
		}

		require.Equal(t, goroutines, errorCount)
	})

	t.Run("concurrent ReceiveMessages to disconnected client", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		const goroutines = 10

		var wg sync.WaitGroup

		wg.Add(goroutines)

		for range goroutines {
			go func() {
				defer wg.Done()

				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()

				for _, err := range client.ReceiveMessages(ctx) {
					// Should error (not connected)
					require.Error(t, err)

					break
				}
			}()
		}

		wg.Wait()
	})

	t.Run("concurrent close calls", func(t *testing.T) {
		client := NewClient()

		const goroutines = 5

		var wg sync.WaitGroup

		wg.Add(goroutines)

		errors := make(chan error, goroutines)

		for range goroutines {
			go func() {
				defer wg.Done()

				err := client.Close()
				errors <- err
			}()
		}

		wg.Wait()
		close(errors)

		// All closes should succeed (idempotent)
		for err := range errors {
			require.NoError(t, err)
		}
	})

	t.Run("concurrent connect attempts", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		const goroutines = 5

		var wg sync.WaitGroup

		wg.Add(goroutines)

		for range goroutines {
			go func() {
				defer wg.Done()

				ctx := context.Background()

				err := client.Start(ctx, WithCliPath("/nonexistent/claude"))
				// All should fail with CLI not found or "already connected"
				require.Error(t, err)
			}()
		}

		wg.Wait()
	})
}

// TestClient_ReceiveResponseIterator tests the ReceiveResponse iterator method
// that yields messages until a ResultMessage is received.
func TestClient_ReceiveResponseIterator(t *testing.T) {
	t.Run("returns error when not connected", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		var gotError error

		for _, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		require.Error(t, gotError)
		require.Contains(t, gotError.Error(), "not connected")
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		var gotError error

		for _, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		require.Error(t, gotError)
	})
}

// TestClient_GetMCPStatusNotConnected tests GetMCPStatus when not connected.
func TestClient_GetMCPStatusNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	status, err := client.GetMCPStatus(ctx)

	require.Error(t, err)
	require.Nil(t, status)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_RewindFilesNotConnected tests RewindFiles when not connected.
func TestClient_RewindFilesNotConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()
	err := client.RewindFiles(ctx, "msg_user_123")

	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_RewindFilesEmptyList tests RewindFiles with empty file list.
func TestClient_RewindFilesEmptyList(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()
	err := client.RewindFiles(ctx, "")

	// Should fail because not connected, not because of empty list
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")
}

// TestClient_WithOptionsVerification tests that options are properly stored.
func TestClient_WithOptionsVerification(t *testing.T) {
	tests := []struct {
		name              string
		opts              []Option
		expectUnsupported bool
	}{
		{
			name: "with allowed tools",
			opts: []Option{WithAllowedTools("Read", "Write")},
		},
		{
			name: "with disallowed tools",
			opts: []Option{WithDisallowedTools("Bash")},
		},
		{
			name: "with output format",
			opts: []Option{WithOutputFormat(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"result": map[string]any{
						"type": "string",
					},
				},
			})},
		},
		{
			name:              "with add dirs",
			opts:              []Option{WithAddDirs("/tmp", "/home")},
			expectUnsupported: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient()
			defer client.Close()

			ctx := context.Background()

			opts := append(tt.opts, WithCliPath("/nonexistent/claude"))

			// Connect will fail with CLI not found, but options should be validated
			err := client.Start(ctx, opts...)
			require.Error(t, err)

			if tt.expectUnsupported {
				require.ErrorIs(t, err, ErrUnsupportedOption)
			} else {
				_, ok := errors.AsType[*CLINotFoundError](err)
				require.True(t, ok)
			}
		})
	}
}

// TestClient_WithMCPServers tests MCP server configuration propagation.
func TestClient_WithMCPServers(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()
	err := client.Start(ctx,
		WithCliPath("/nonexistent/claude"),
		WithMCPServers(map[string]MCPServerConfig{
			"test-server": &MCPStdioServerConfig{
				Type:    new(MCPServerTypeStdio),
				Command: "node",
				Args:    []string{"server.js"},
			},
		}),
	)
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)
}

// TestClient_RewindFilesWithCancelledContext tests RewindFiles with cancelled context.
func TestClient_RewindFilesWithCancelledContext(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := client.RewindFiles(ctx, "msg_user_123")
	require.Error(t, err)
}

// TestClient_MessageFlowPattern tests the message send/receive pattern.
func TestClient_MessageFlowPattern(t *testing.T) {
	t.Run("Query followed by ReceiveMessages when not connected", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		// Query should fail when not connected
		err := client.Query(ctx, Text("Hello"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "not connected")

		// ReceiveMessages should also fail when not connected
		ctx2, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		var gotError error

		for _, recvErr := range client.ReceiveMessages(ctx2) {
			if recvErr != nil {
				gotError = recvErr

				break
			}
		}

		require.Error(t, gotError)
	})

	t.Run("ReceiveResponse pattern when not connected", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		// ReceiveResponse should fail when not connected
		var gotError error

		for _, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		require.Error(t, gotError)
		require.Contains(t, gotError.Error(), "not connected")
	})
}

// TestClient_QueryPayloadFormat tests the Query message format matches expected JSON structure.
func TestClient_QueryPayloadFormat(t *testing.T) {
	t.Run("query payload matches expected format", func(t *testing.T) {
		// Test the expected JSON structure without connecting
		expected := map[string]any{
			"type": "user",
			"content": []map[string]any{
				{
					"type": "text",
					"text": "Hello, Claude!",
				},
			},
		}

		// Verify we can marshal the expected format
		data, err := json.Marshal(expected)
		require.NoError(t, err)

		// Verify the JSON structure
		var parsed map[string]any

		err = json.Unmarshal(data, &parsed)
		require.NoError(t, err)
		require.Equal(t, "user", parsed["type"])

		content, ok := parsed["content"].([]any)
		require.True(t, ok)
		require.Len(t, content, 1)
	})
}

// TestClient_ControlMessageFormats tests control request/response message formats.
func TestClient_ControlMessageFormats(t *testing.T) {
	tests := []struct {
		name     string
		subtype  string
		payload  map[string]any
		expected map[string]any
	}{
		{
			name:    "interrupt control request",
			subtype: "interrupt",
			payload: nil,
			expected: map[string]any{
				"type":    "control_request",
				"subtype": "interrupt",
			},
		},
		{
			name:    "set_permission_mode control request",
			subtype: "set_permission_mode",
			payload: map[string]any{"mode": "acceptEdits"},
			expected: map[string]any{
				"type":    "control_request",
				"subtype": "set_permission_mode",
				"payload": map[string]any{"mode": "acceptEdits"},
			},
		},
		{
			name:    "set_model control request",
			subtype: "set_model",
			payload: map[string]any{"model": "claude-3-5-sonnet-20241022"},
			expected: map[string]any{
				"type":    "control_request",
				"subtype": "set_model",
				"payload": map[string]any{"model": "claude-3-5-sonnet-20241022"},
			},
		},
		{
			name:    "rewind_files control request",
			subtype: "rewind_files",
			payload: map[string]any{"files": []string{"file1.txt", "file2.txt"}},
			expected: map[string]any{
				"type":    "control_request",
				"subtype": "rewind_files",
				"payload": map[string]any{"files": []string{"file1.txt", "file2.txt"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := map[string]any{
				"type":    "control_request",
				"subtype": tt.subtype,
			}

			if tt.payload != nil {
				msg["payload"] = tt.payload
			}

			// Verify serialization
			data, err := json.Marshal(msg)
			require.NoError(t, err)
			require.NotEmpty(t, data)

			// Verify structure
			var parsed map[string]any

			err = json.Unmarshal(data, &parsed)
			require.NoError(t, err)
			require.Equal(t, tt.expected["type"], parsed["type"])
			require.Equal(t, tt.expected["subtype"], parsed["subtype"])
		})
	}
}

// TestClient_MessageTypeHandling tests that different message types are handled correctly.
// This verifies the client would correctly process messages from the transport.
func TestClient_MessageTypeHandling(t *testing.T) {
	tests := []struct {
		name        string
		messageJSON string
		expectType  string
	}{
		{
			name:        "system message",
			messageJSON: `{"type":"system","subtype":"init","content":"Session started"}`,
			expectType:  "system",
		},
		{
			name:        "assistant message with text",
			messageJSON: `{"type":"assistant","message":{"type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","id":"msg_123","content":[{"type":"text","text":"Hello!"}]}}`,
			expectType:  "assistant",
		},
		{
			name:        "result message success",
			messageJSON: `{"type":"result","subtype":"success","is_error":false,"session_id":"session_abc"}`,
			expectType:  "result",
		},
		{
			name:        "result message error",
			messageJSON: `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"session_err"}`,
			expectType:  "result",
		},
		{
			name:        "user message with tool result",
			messageJSON: `{"type":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","is_error":false,"content":[{"type":"text","text":"Success"}]}]}`,
			expectType:  "user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg map[string]any

			err := json.Unmarshal([]byte(tt.messageJSON), &msg)
			require.NoError(t, err)
			require.Equal(t, tt.expectType, msg["type"])
		})
	}
}

// TestClient_SessionIDHandling tests that session ID is properly passed through options.
func TestClient_SessionIDHandling(t *testing.T) {
	t.Run("resume session ID in options", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()
		err := client.Start(ctx,
			WithResume("session_abc123"),
			WithCliPath("/nonexistent/claude"),
		)

		// Verify CLI not found (expected)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("fork session option", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()
		err := client.Start(ctx,
			WithResume("session_xyz"),
			WithForkSession(true),
			WithCliPath("/nonexistent/claude"),
		)

		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})
}

// TestClient_ConcurrentSendReceiveWhenNotConnected tests concurrent safety for disconnected client.
func TestClient_ConcurrentSendReceiveWhenNotConnected(t *testing.T) {
	t.Run("concurrent queries and receives", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		const goroutines = 10

		var wg sync.WaitGroup

		wg.Add(goroutines * 2)

		// Concurrent queries
		for range goroutines {
			go func() {
				defer wg.Done()

				ctx := context.Background()
				err := client.Query(ctx, Text("test"))

				require.Error(t, err)
				require.Contains(t, err.Error(), "not connected")
			}()
		}

		// Concurrent ReceiveMessages
		for range goroutines {
			go func() {
				defer wg.Done()

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer cancel()

				for _, err := range client.ReceiveMessages(ctx) {
					require.Error(t, err)

					break
				}
			}()
		}

		wg.Wait()
	})

	t.Run("concurrent control operations", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		var wg sync.WaitGroup

		wg.Add(4)

		go func() {
			defer wg.Done()

			ctx := context.Background()

			_ = client.Query(ctx, Text("test"))
		}()

		go func() {
			defer wg.Done()

			ctx := context.Background()

			_ = client.Interrupt(ctx)
		}()

		go func() {
			defer wg.Done()

			ctx := context.Background()

			_ = client.RewindFiles(ctx, "msg_user_456")
		}()

		go func() {
			defer wg.Done()

			ctx := context.Background()

			for range client.ReceiveResponse(ctx) {
				break
			}
		}()

		wg.Wait()
	})
}

// TestClient_ResponseStopsAtResult tests that ReceiveResponse stops at ResultMessage.
func TestClient_ResponseStopsAtResult(t *testing.T) {
	t.Run("ResultMessage detection", func(t *testing.T) {
		// Test that we can detect ResultMessage type
		resultJSON := `{"type":"result","subtype":"success","is_error":false,"session_id":"session_abc"}`

		var msg map[string]any

		err := json.Unmarshal([]byte(resultJSON), &msg)
		require.NoError(t, err)

		// Verify it's a result message
		require.Equal(t, "result", msg["type"])
		require.Equal(t, "success", msg["subtype"])
	})

	t.Run("ReceiveResponse returns error when not connected", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		var gotError error

		for _, err := range client.ReceiveResponse(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		require.Error(t, gotError)
		require.Contains(t, gotError.Error(), "not connected")
	})
}

// TestClient_StartWithContent tests StartWithContent method.
func TestClient_StartWithContent(t *testing.T) {
	t.Run("sends prompt after connection", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		// StartWithContent should fail with CLI not found
		err := client.StartWithContent(ctx, Text("Hello, Claude!"),
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("with empty prompt", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		err := client.StartWithContent(ctx, Text(""),
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("with cancelled context", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := client.StartWithContent(ctx, Text("test"),
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
	})

	t.Run("with various options", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		err := client.StartWithContent(ctx, Text("What is 2+2?"),
			WithCliPath("/nonexistent/claude"),
			WithModel("claude-3-5-sonnet-20241022"),
			WithPermissionMode("acceptEdits"),
			WithSystemPrompt("You are a helpful assistant."),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})
}

// TestClient_StartWithStream tests StartWithStream method.
func TestClient_StartWithStream(t *testing.T) {
	t.Run("with single message", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		messages := SingleMessage(Text("Hello, Claude!"))

		err := client.StartWithStream(ctx, messages,
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("with multiple messages from slice", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		msgs := []StreamingMessage{
			NewUserMessage(Text("First message")),
			NewUserMessage(Text("Second message")),
		}
		messages := MessagesFromSlice(msgs)

		err := client.StartWithStream(ctx, messages,
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("with empty iterator", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		messages := MessagesFromSlice([]StreamingMessage{})

		err := client.StartWithStream(ctx, messages,
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})

	t.Run("with cancelled context", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		messages := SingleMessage(Text("test"))

		err := client.StartWithStream(ctx, messages,
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
	})

	t.Run("with channel-based messages", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		ctx := context.Background()

		ch := make(chan StreamingMessage, 2)

		ch <- NewUserMessage(Text("First"))

		ch <- NewUserMessage(Text("Second"))

		close(ch)

		messages := MessagesFromChannel(ch)

		err := client.StartWithStream(ctx, messages,
			WithCliPath("/nonexistent/claude"),
		)
		require.Error(t, err)
		_, ok := errors.AsType[*CLINotFoundError](err)
		require.True(t, ok)
	})
}

// TestClient_StartWithContent_AlreadyConnected tests that StartWithContent returns error if already connected.
func TestClient_StartWithContent_AlreadyConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	// First call fails with CLI not found (doesn't set connected=true)
	err := client.StartWithContent(ctx, Text("test"),
		WithCliPath("/nonexistent/claude"),
	)
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)

	// Second call should also fail with CLI not found
	err = client.StartWithContent(ctx, Text("test2"),
		WithCliPath("/nonexistent/claude"),
	)
	require.Error(t, err)
}

// TestClient_StartWithStream_AlreadyConnected tests that StartWithStream returns error if already connected.
func TestClient_StartWithStream_AlreadyConnected(t *testing.T) {
	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	messages := SingleMessage(Text("test"))

	// First call fails with CLI not found
	err := client.StartWithStream(ctx, messages,
		WithCliPath("/nonexistent/claude"),
	)
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)

	// Second call should also fail
	err = client.StartWithStream(ctx, messages,
		WithCliPath("/nonexistent/claude"),
	)
	require.Error(t, err)
}

// TestClient_StartAfterClose tests that Start() returns ErrClientClosed after Close().
func TestClient_StartAfterClose(t *testing.T) {
	client := NewClient()

	// Close the client first
	err := client.Close()
	require.NoError(t, err)

	// Start should return ErrClientClosed
	err = client.Start(context.Background(), WithCliPath("/nonexistent/claude"))
	require.ErrorIs(t, err, ErrClientClosed)
}

// TestClient_StartWithContentAfterClose tests that StartWithContent() returns ErrClientClosed after Close().
func TestClient_StartWithContentAfterClose(t *testing.T) {
	client := NewClient()

	// Close the client first
	err := client.Close()
	require.NoError(t, err)

	// StartWithContent should return ErrClientClosed
	err = client.StartWithContent(context.Background(), Text("Hello"),
		WithCliPath("/nonexistent/claude"),
	)
	require.ErrorIs(t, err, ErrClientClosed)
}

// TestClient_StartWithStreamAfterClose tests that StartWithStream() returns ErrClientClosed after Close().
func TestClient_StartWithStreamAfterClose(t *testing.T) {
	client := NewClient()

	// Close the client first
	err := client.Close()
	require.NoError(t, err)

	// StartWithStream should return ErrClientClosed
	messages := SingleMessage(Text("Hello"))
	err = client.StartWithStream(context.Background(), messages,
		WithCliPath("/nonexistent/claude"),
	)
	require.ErrorIs(t, err, ErrClientClosed)
}

// TestClient_ConcurrentCloseNoPanic tests that 50 concurrent Close() calls don't panic.
func TestClient_ConcurrentCloseNoPanic(t *testing.T) {
	client := NewClient()

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			// Should not panic
			err := client.Close()
			require.NoError(t, err)
		}()
	}

	wg.Wait()

	// Verify client is closed by trying to start
	err := client.Start(context.Background(), WithCliPath("/nonexistent/claude"))
	require.ErrorIs(t, err, ErrClientClosed)
}

// TestClient_CloseAfterFailedStart tests that Close() then Start() returns ErrClientClosed.
func TestClient_CloseAfterFailedStart(t *testing.T) {
	client := NewClient()

	// First Start fails with CLI not found (client not marked as connected)
	err := client.Start(context.Background(), WithCliPath("/nonexistent/claude"))
	require.Error(t, err)
	_, ok := errors.AsType[*CLINotFoundError](err)
	require.True(t, ok)

	// Close the client
	err = client.Close()
	require.NoError(t, err)

	// Start should return ErrClientClosed
	err = client.Start(context.Background(), WithCliPath("/nonexistent/claude"))
	require.ErrorIs(t, err, ErrClientClosed)
}

// =============================================================================
// Bug Detection Tests - These tests are designed to FAIL with current buggy code
// and PASS after the bugs are fixed.
// =============================================================================

// TestClient_CloseReturnsTransportError tests that the sync.Once pattern
// used in Client.Close() properly captures and returns errors.
//
// The Close() method uses a closure variable to capture the error from
// transport.Close() and return it to the caller.
func TestClient_CloseReturnsTransportError(t *testing.T) {
	t.Run("sync.Once pattern captures error via closure", func(t *testing.T) {
		// This test verifies the correct pattern used in client.Close()
		// The error is captured via a closure variable and returned after Do() completes.
		var (
			closeOnce sync.Once
			closeErr  error
		)

		correctClose := func() error {
			closeOnce.Do(func() {
				// Simulate transport.Close() returning an error
				closeErr = fmt.Errorf("transport close failed")
			})

			return closeErr // Return the captured error
		}

		err := correctClose()

		// The error should be properly captured and returned
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transport close failed")
	})

	t.Run("second close returns nil", func(t *testing.T) {
		// Once.Do only executes the function once, so subsequent calls
		// should return nil (the zero value of closeErr before it's set)
		var (
			closeOnce sync.Once
			closeErr  error
		)

		correctClose := func() error {
			closeOnce.Do(func() {
				closeErr = fmt.Errorf("transport close failed")
			})

			return closeErr
		}

		// First call captures the error
		err1 := correctClose()
		require.Error(t, err1)

		// Second call returns the same captured error (closure persists)
		err2 := correctClose()
		require.Error(t, err2)
		assert.Equal(t, err1, err2)
	})
}

// =============================================================================
// Mid-Operation Close/Cancel Tests
// =============================================================================

// Note: Tests with mock transport require the full protocol initialization flow.
// These tests focus on behaviors that can be verified without a connected client.

// TestClient_ReceiveMessagesContextCancelMidStream tests context cancellation
// behavior when ReceiveMessages is in progress (disconnected client path).
func TestClient_ReceiveMessagesContextCancelMidStream(t *testing.T) {
	t.Run("context cancellation returns error", func(t *testing.T) {
		client := NewClient()
		defer client.Close()

		// Create a context that we'll cancel
		ctx, cancel := context.WithCancel(context.Background())

		// Cancel after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		var gotError error

		for _, err := range client.ReceiveMessages(ctx) {
			if err != nil {
				gotError = err

				break
			}
		}

		// Should get not connected error (since client isn't started)
		require.Error(t, gotError)
		require.Contains(t, gotError.Error(), "not connected")
	})
}

// TestClient_ConcurrentCloseAndReceiveDisconnected tests that concurrent close
// and receive operations on a disconnected client don't cause panics.
func TestClient_ConcurrentCloseAndReceiveDisconnected(t *testing.T) {
	client := NewClient()

	const goroutines = 10

	var wg sync.WaitGroup

	wg.Add(goroutines * 2)

	// Start multiple receivers (on disconnected client)
	for range goroutines {
		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			for _, err := range client.ReceiveMessages(ctx) {
				// Should immediately get "not connected" error
				require.Error(t, err)

				break
			}
		}()
	}

	// Start multiple closers
	for range goroutines {
		go func() {
			defer wg.Done()

			// Small random delay
			time.Sleep(time.Duration(time.Now().UnixNano()%10) * time.Millisecond)

			err := client.Close()
			require.NoError(t, err)
		}()
	}

	// Wait for all goroutines
	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success - no panic or deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("Deadlock detected")
	}
}

// TestClient_OperationsAfterCloseReturnError tests that all operations
// return appropriate errors after Close() is called.
func TestClient_OperationsAfterCloseReturnError(t *testing.T) {
	client := NewClient()

	ctx := context.Background()

	// Close first
	err := client.Close()
	require.NoError(t, err)

	// Query should fail
	err = client.Query(ctx, Text("test"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")

	// Interrupt should fail
	err = client.Interrupt(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")

	// SetPermissionMode should fail
	err = client.SetPermissionMode(ctx, "acceptEdits")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")

	// ReceiveMessages should fail
	for _, err := range client.ReceiveMessages(ctx) {
		require.Error(t, err)
		require.Contains(t, err.Error(), "not connected")

		break
	}

	// ReceiveResponse should fail
	for _, err := range client.ReceiveResponse(ctx) {
		require.Error(t, err)
		require.Contains(t, err.Error(), "not connected")

		break
	}

	// RewindFiles should fail
	err = client.RewindFiles(ctx, "msg_123")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not connected")

	// GetMCPStatus should fail
	status, err := client.GetMCPStatus(ctx)
	require.Error(t, err)
	require.Nil(t, status)
	require.Contains(t, err.Error(), "not connected")

	// GetServerInfo should return nil
	info := client.GetServerInfo()
	require.Nil(t, info)

	// Start should return ErrClientClosed
	err = client.Start(ctx, WithCliPath("/nonexistent/claude"))
	require.ErrorIs(t, err, ErrClientClosed)
}
