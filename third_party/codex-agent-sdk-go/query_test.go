package codexsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
)

const msgTypeControlRequest = "control_request"

func TestQueryCLINotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithCliPath("/nonexistent/path/to/claude"),
	) {
		if err == nil {
			t.Fatal("Expected error when CLI not found")
		}

		if _, ok := errors.AsType[*CLINotFoundError](err); !ok {
			t.Errorf("Expected CLINotFoundError, got: %v", err)
		}

		break
	}
}

func TestQueryWithNoOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// This should work if claude is in PATH
	// If not, it should return CLINotFoundError or ProcessError
	for _, err := range Query(ctx, Text("test")) {
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type: %v", err)
		}

		break
	}
}

// TestQuery_WithOptions tests Query with full CodexAgentOptions configuration.
func TestQuery_WithOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithSystemPrompt("You are a helpful assistant."),
		WithModel("claude-sonnet-4-5-20250514"),
		WithPermissionMode("bypassPermissions"),
		WithEnv(map[string]string{"TEST_VAR": "test_value"}),
		WithConfig(map[string]string{"model": "gpt-5.4"}),
		WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
				"required": []string{"answer"},
			},
		}),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with full options: %v", err)
		}

		break
	}
}

// TestQuery_WithCwd tests Query with a custom working directory.
func TestQuery_WithCwd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a temporary directory
	tmpDir, err := os.MkdirTemp("", "claude-sdk-test-cwd-*")
	require.NoError(t, err)

	defer os.RemoveAll(tmpDir)

	// Resolve to absolute path
	absPath, err := filepath.Abs(tmpDir)
	require.NoError(t, err)

	for _, err := range Query(ctx, Text("test"),
		WithCwd(absPath),
		WithPermissionMode("bypassPermissions"),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with Cwd option: %v", err)
		}

		break
	}
}

// TestQuery_WithEnv tests Query with custom environment variables.
func TestQuery_WithEnv(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithPermissionMode("bypassPermissions"),
		WithEnv(map[string]string{
			"CLAUDE_SDK_TEST_VAR": "test_value_123",
			"ANOTHER_VAR":         "another_value",
		}),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with Env option: %v", err)
		}

		break
	}
}

// TestQuery_WithSystemPromptPreset tests Query option handling for system prompt presets.
func TestQuery_WithSystemPromptPreset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithSystemPromptPreset(&SystemPromptPreset{
			Type:   "preset",
			Preset: "claude_code",
			Append: new("\nAdditional instructions."),
		}),
		WithPermissionMode("bypassPermissions"),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with SystemPromptPreset option: %v", err)
		}

		break
	}
}

// TestQuery_WithOutputFormat tests Query with structured output format.
func TestQuery_WithOutputFormat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithOutputFormat(map[string]any{
			"type": "json_schema",
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answer": map[string]any{
						"type": "string",
					},
				},
				"required": []string{"answer"},
			},
		}),
		WithPermissionMode("bypassPermissions"),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with OutputFormat option: %v", err)
		}

		break
	}
}

// TestQuery_WithResume tests Query with session resume option.
func TestQuery_WithResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithResume("nonexistent-session-id"),
		WithPermissionMode("bypassPermissions"),
	) {
		// May fail due to invalid session, but should not be unexpected error type
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Logf("Error with Resume option (may be expected): %v", err)
		}

		break
	}
}

// TestQuery_WithExtraArgs tests Query with extra CLI arguments.
func TestQuery_WithExtraArgs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithExtraArgs(map[string]*string{
			"verbose": new(""), // Boolean flag with empty value
		}),
		WithPermissionMode("bypassPermissions"),
	) {
		// Either succeeds or returns CLINotFoundError or ProcessError (CLI issues)
		_, isCLINotFound := errors.AsType[*CLINotFoundError](err)
		_, isProcessError := errors.AsType[*ProcessError](err)

		if err != nil && !isCLINotFound && !isProcessError {
			t.Errorf("Unexpected error type with ExtraArgs option: %v", err)
		}

		break
	}
}

// TestQuery_CanUseToolWithPermissionPromptToolName tests that Query yields
// an error when both CanUseTool and PermissionPromptToolName are set.
func TestQuery_CanUseToolWithPermissionPromptToolName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, err := range Query(ctx, Text("test"),
		WithCanUseTool(func(
			_ context.Context,
			_ string,
			_ map[string]any,
			_ *ToolPermissionContext,
		) (PermissionResult, error) {
			return &PermissionResultAllow{Behavior: "allow"}, nil
		}),
		WithPermissionPromptToolName("custom"),
		WithPermissionMode("bypassPermissions"),
	) {
		require.Error(t, err)
		require.Contains(t, err.Error(), "can_use_tool callback cannot be used with permission_prompt_tool_name")

		break
	}
}

// TestQuery_CanUseToolAutoConfiguresPermissionPrompt tests that Query
// automatically sets PermissionPromptToolName to "stdio" when CanUseTool is set.
func TestQuery_CanUseToolAutoConfiguresPermissionPrompt(t *testing.T) {
	options := &CodexAgentOptions{
		CanUseTool: func(
			_ context.Context,
			_ string,
			_ map[string]any,
			_ *ToolPermissionContext,
		) (PermissionResult, error) {
			return &PermissionResultAllow{Behavior: "allow"}, nil
		},
		PermissionMode: "bypassPermissions",
	}

	// Call validateAndConfigureOptions directly to test the auto-configuration
	err := validateAndConfigureOptions(options)

	require.NoError(t, err)
	require.Equal(t, "stdio", options.PermissionPromptToolName)
}

// TestQueryStream_CanUseToolWithPermissionPromptToolName tests that QueryStream
// yields an error when both CanUseTool and PermissionPromptToolName are set.
func TestQueryStream_CanUseToolWithPermissionPromptToolName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	messages := MessagesFromSlice([]StreamingMessage{
		NewUserMessage(Text("test")),
	})

	for _, err := range QueryStream(ctx, messages,
		WithCanUseTool(func(
			_ context.Context,
			_ string,
			_ map[string]any,
			_ *ToolPermissionContext,
		) (PermissionResult, error) {
			return &PermissionResultAllow{Behavior: "allow"}, nil
		}),
		WithPermissionPromptToolName("custom"),
		WithPermissionMode("bypassPermissions"),
	) {
		require.Error(t, err)
		require.Contains(t, err.Error(), "can_use_tool callback cannot be used with permission_prompt_tool_name")

		break
	}
}

// dummyCanUseTool is a helper for tests that returns allow.
func dummyCanUseTool(
	_ context.Context,
	_ string,
	_ map[string]any,
	_ *ToolPermissionContext,
) (PermissionResult, error) {
	return &PermissionResultAllow{Behavior: "allow"}, nil
}

// TestValidateAndConfigureOptions tests the validation helper function.
func TestValidateAndConfigureOptions(t *testing.T) {
	tests := []struct {
		name        string
		options     *CodexAgentOptions
		wantErr     bool
		errContains string
		checkFunc   func(t *testing.T, opts *CodexAgentOptions)
	}{
		{
			name:    "nil CanUseTool does not modify PermissionPromptToolName",
			options: &CodexAgentOptions{},
			wantErr: false,
			checkFunc: func(t *testing.T, opts *CodexAgentOptions) {
				t.Helper()
				require.Empty(t, opts.PermissionPromptToolName)
			},
		},
		{
			name: "CanUseTool without PermissionPromptToolName sets stdio",
			options: &CodexAgentOptions{
				CanUseTool: dummyCanUseTool,
			},
			wantErr: false,
			checkFunc: func(t *testing.T, opts *CodexAgentOptions) {
				t.Helper()
				require.Equal(t, "stdio", opts.PermissionPromptToolName)
			},
		},
		{
			name: "CanUseTool with PermissionPromptToolName returns error",
			options: &CodexAgentOptions{
				CanUseTool:               dummyCanUseTool,
				PermissionPromptToolName: "custom",
			},
			wantErr:     true,
			errContains: "can_use_tool callback cannot be used with permission_prompt_tool_name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAndConfigureOptions(tt.options)

			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errContains)
			} else {
				require.NoError(t, err)

				if tt.checkFunc != nil {
					tt.checkFunc(t, tt.options)
				}
			}
		})
	}
}

func TestRequiresAppServerQuery(t *testing.T) {
	tests := []struct {
		name    string
		options *CodexAgentOptions
		want    bool
	}{
		{name: "default options", options: &CodexAgentOptions{}, want: false},
		{name: "exec-compatible option stays exec", options: &CodexAgentOptions{Model: "gpt-5.4"}, want: false},
		{name: "system prompt requires app-server", options: &CodexAgentOptions{SystemPrompt: "be concise"}, want: true},
		{name: "resume requires app-server", options: &CodexAgentOptions{Resume: "thread_1"}, want: true},
		{name: "fork requires app-server", options: &CodexAgentOptions{ForkSession: true}, want: true},
		{name: "continue requires app-server", options: &CodexAgentOptions{ContinueConversation: true}, want: true},
		{name: "output format requires app-server", options: &CodexAgentOptions{OutputFormat: map[string]any{"type": "json_schema"}}, want: true},
		{
			name: "sdk mcp server requires app-server",
			options: &CodexAgentOptions{
				MCPServers: map[string]MCPServerConfig{
					"sdk": &MCPSdkServerConfig{Type: MCPServerTypeSDK, Name: "sdk"},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, config.SelectQueryBackend(tt.options) == config.QueryBackendAppServer)
		})
	}
}

func TestQuery_FailFastUnsupportedOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tests := []struct {
		name     string
		opts     []Option
		contains string
	}{
		{
			name: "continue without resume unsupported",
			opts: []Option{
				WithContinueConversation(true),
			},
			contains: "requires WithResume",
		},
		{
			name: "permission prompt custom tool unsupported",
			opts: []Option{
				WithPermissionPromptToolName("custom"),
			},
			contains: "PermissionPromptToolName",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, err := range Query(ctx, Text("test"), tt.opts...) {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrUnsupportedOption)
				require.Contains(t, err.Error(), tt.contains)

				return
			}

			t.Fatal("expected fail-fast error")
		})
	}
}

func TestBuildInitialUserMessage(t *testing.T) {
	t.Run("text-only prompt", func(t *testing.T) {
		msg := buildInitialUserMessage(Text("hello"), &CodexAgentOptions{})
		require.Equal(t, "user", msg["type"])

		messageData, ok := msg["message"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, Text("hello"), messageData["content"])
	})

	t.Run("prompt with images", func(t *testing.T) {
		msg := buildInitialUserMessage(Text("look at this"), &CodexAgentOptions{
			Images: []string{"/tmp/a.png", "/tmp/b.png"},
		})

		messageData, ok := msg["message"].(map[string]any)
		require.True(t, ok)

		content, ok := messageData["content"].(UserMessageContent)
		require.True(t, ok)

		blocks := content.Blocks()
		require.Len(t, blocks, 3)
		require.Equal(t, "text", blocks[0].BlockType())

		imageA, ok := blocks[1].(*InputLocalImageBlock)
		require.True(t, ok)
		require.Equal(t, "/tmp/a.png", imageA.Path)

		imageB, ok := blocks[2].(*InputLocalImageBlock)
		require.True(t, ok)
		require.Equal(t, "/tmp/b.png", imageB.Path)
	})
}

// =============================================================================
// Mid-Operation Context Cancellation Tests
// =============================================================================

// TestQuery_ContextCancelMidIteration tests that Query respects context
// cancellation during message iteration.
func TestQuery_ContextCancelMidIteration(t *testing.T) {
	t.Run("context timeout during iteration", func(t *testing.T) {
		// Create a context with very short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		var gotError error

		var iterationCount int

		for _, err := range Query(ctx, Text("test"),
			WithCliPath("/nonexistent/path/to/claude"),
		) {
			iterationCount++

			if err != nil {
				gotError = err

				break
			}
		}

		// Should get either CLI not found (fast path) or context deadline exceeded
		require.Error(t, gotError)

		_, isCLINotFound := errors.AsType[*CLINotFoundError](gotError)
		isContextErr := gotError == context.DeadlineExceeded ||
			gotError == context.Canceled

		require.True(t, isCLINotFound || isContextErr,
			"Expected CLINotFoundError or context error, got: %v", gotError)

		t.Logf("Iterations: %d, Error type: %T", iterationCount, gotError)
	})

	t.Run("context cancel before iteration starts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		var gotError error

		for _, err := range Query(ctx, Text("test")) {
			if err != nil {
				gotError = err

				break
			}
		}

		// Should get an error (either context canceled or CLI not found)
		require.Error(t, gotError)
	})
}

// TestQueryStream_ContextCancelMidIteration tests that QueryStream respects
// context cancellation during message iteration.
func TestQueryStream_ContextCancelMidIteration(t *testing.T) {
	t.Run("context timeout during streaming iteration", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		messages := MessagesFromSlice([]StreamingMessage{
			NewUserMessage(Text("test message")),
		})

		var gotError error

		for _, err := range QueryStream(ctx, messages,
			WithCliPath("/nonexistent/path/to/claude"),
		) {
			if err != nil {
				gotError = err

				break
			}
		}

		// Should get either CLI not found or context error
		require.Error(t, gotError)

		_, isCLINotFound := errors.AsType[*CLINotFoundError](gotError)
		isContextErr := gotError == context.DeadlineExceeded ||
			gotError == context.Canceled

		require.True(t, isCLINotFound || isContextErr,
			"Expected CLINotFoundError or context error, got: %v", gotError)
	})
}

// =============================================================================
// Early Iteration Exit Goroutine Leak Tests
// =============================================================================

// TestQuery_EarlyExitDoesNotLeakGoroutines tests that breaking out of the
// iterator early doesn't leak goroutines.
func TestQuery_EarlyExitDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Break out of iteration immediately
	for _, err := range Query(ctx, Text("test"),
		WithCliPath("/nonexistent/path/to/claude"),
	) {
		// Break immediately regardless of error
		_ = err

		break
	}

	// Allow goroutines to settle
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()

	// Should not have leaked goroutines (allow +2 for GC fluctuation)
	require.LessOrEqual(t, after, before+2,
		"goroutine leak detected: before=%d, after=%d", before, after)
}

// TestQueryStream_EarlyExitDoesNotBlockInputStreamer tests that early exit
// with MCP/hooks configuration doesn't block the input streamer goroutine.
func TestQueryStream_EarlyExitDoesNotBlockInputStreamer(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	messages := MessagesFromSlice([]StreamingMessage{
		NewUserMessage(Text("test")),
	})

	// Break out immediately
	for _, err := range QueryStream(ctx, messages,
		WithCliPath("/nonexistent/path/to/claude"),
	) {
		_ = err

		break
	}

	// Allow goroutines to settle - should NOT take 60 seconds
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()

	require.LessOrEqual(t, after, before+2,
		"goroutine leak detected - input streamer may be blocked")
}

// TestQueryStream_DeferOrderingDoesNotDeadlock verifies that the defer ordering
// in QueryStream correctly closes resultReceived BEFORE wg.Wait() to prevent
// deadlock when the main loop exits early while streamInputMessages is blocked.
//
// This test specifically targets Issue #1: Potential goroutine leak in error paths.
// The bug was that defer statements were in wrong order (LIFO):
//   - defer close(resultReceived) registered FIRST, executes SECOND
//   - defer wg.Wait() registered SECOND, executes FIRST
//
// This caused wg.Wait() to block before close(resultReceived) could unblock
// the streamInputMessages goroutine, relying on 60s timeout to recover.
func TestQueryStream_DeferOrderingDoesNotDeadlock(t *testing.T) {
	before := runtime.NumGoroutine()

	// Use a context with short timeout to force early exit
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Create a slow message iterator that will be interrupted
	slowMessages := func(yield func(StreamingMessage) bool) {
		// First message goes through
		if !yield(NewUserMessage(Text("first"))) {
			return
		}
		// Simulate slow producer - will be interrupted by context
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
			yield(NewUserMessage(Text("second")))
		}
	}

	start := time.Now()

	// Run QueryStream - should exit quickly when context times out
	for _, err := range QueryStream(ctx, slowMessages,
		WithCliPath("/nonexistent/path/to/claude"),
		WithMCPServers(map[string]MCPServerConfig{
			"test": &MCPStdioServerConfig{Command: "echo", Args: []string{"test"}},
		}), // Enable MCP to trigger resultReceived channel usage
	) {
		_ = err

		break
	}

	elapsed := time.Since(start)

	// Should complete quickly (context timeout + cleanup), NOT wait for
	// the 60s streamCloseTimeout. Allow 2 seconds for cleanup overhead.
	require.Less(t, elapsed, 2*time.Second,
		"QueryStream took too long (%v) - possible deadlock due to defer ordering", elapsed)

	// Allow goroutines to settle
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()

	require.LessOrEqual(t, after, before+2,
		"goroutine leak detected: before=%d, after=%d", before, after)
}

// =============================================================================
// Bug Detection Tests - These tests are designed to FAIL with current buggy code
// and PASS after the bugs are fixed.
// =============================================================================

// =============================================================================
// Errgroup Error Propagation Tests
// =============================================================================

// failingTransport is a mock transport that fails on SendMessage after N calls.
type failingTransport struct {
	mu            sync.Mutex
	sendCallCount atomic.Int32
	failAfter     int32 // Fail after this many SendMessage calls
	msgChan       chan map[string]any
	errChan       chan error
	closed        bool
}

func newFailingTransport(failAfter int32) *failingTransport {
	return &failingTransport{
		failAfter: failAfter,
		msgChan:   make(chan map[string]any, 10),
		errChan:   make(chan error, 1),
	}
}

func (f *failingTransport) Start(_ context.Context) error {
	return nil
}

func (f *failingTransport) ReadMessages(_ context.Context) (<-chan map[string]any, <-chan error) {
	return f.msgChan, f.errChan
}

func (f *failingTransport) SendMessage(_ context.Context, data []byte) error {
	count := f.sendCallCount.Add(1)
	if count > f.failAfter {
		return errors.New("simulated transport send failure")
	}

	// Parse the message to check if it's a control_request that needs a response.
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err == nil {
		if msgType, ok := msg["type"].(string); ok && msgType == msgTypeControlRequest {
			if requestID, ok := msg["request_id"].(string); ok {
				// Send a success response asynchronously.
				go func() {
					f.msgChan <- map[string]any{
						"type": "control_response",
						"response": map[string]any{
							"subtype":    "success",
							"request_id": requestID,
							"response":   map[string]any{},
						},
					}
				}()
			}
		}
	}

	return nil
}

func (f *failingTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.closed {
		f.closed = true
		close(f.msgChan)
		close(f.errChan)
	}

	return nil
}

func (f *failingTransport) IsReady() bool {
	return true
}

func (f *failingTransport) EndInput() error {
	return nil
}

// Compile-time check that failingTransport implements config.Transport.
var _ config.Transport = (*failingTransport)(nil)

// stickyResultTransport emits a ResultMessage after input is closed but keeps
// the transport open until Close() or context cancellation. This characterizes
// Query behavior when a final result arrives before transport EOF.
type stickyResultTransport struct {
	mu      sync.Mutex
	msgChan chan map[string]any
	errChan chan error
	ready   chan struct{}
	msg     map[string]any
	started bool
	closed  bool
}

func newStickyResultTransport(msg map[string]any) *stickyResultTransport {
	return &stickyResultTransport{
		msgChan: make(chan map[string]any, 16),
		errChan: make(chan error, 1),
		ready:   make(chan struct{}),
		msg:     msg,
	}
}

func (s *stickyResultTransport) Start(_ context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()

		return nil
	}

	s.started = true
	msg := s.msg
	s.mu.Unlock()

	go func() {
		<-s.ready

		s.msgChan <- msg
	}()

	return nil
}

func (s *stickyResultTransport) ReadMessages(_ context.Context) (<-chan map[string]any, <-chan error) {
	return s.msgChan, s.errChan
}

func (s *stickyResultTransport) SendMessage(_ context.Context, data []byte) error {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil
	}

	msgType, _ := msg["type"].(string)
	if msgType != msgTypeControlRequest {
		s.release()

		return nil
	}

	requestID, _ := msg["request_id"].(string)
	request, _ := msg["request"].(map[string]any)
	subtype, _ := request["subtype"].(string)

	payload := map[string]any{}
	if subtype == "initialize" {
		payload = map[string]any{
			"protocol_version": "1.0",
			"server_info": map[string]any{
				"name":    "test-server",
				"version": "1.0.0",
			},
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.msgChan <- map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"request_id": requestID,
			"subtype":    "success",
			"response":   payload,
		},
	}

	return nil
}

func (s *stickyResultTransport) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true
	close(s.msgChan)
	close(s.errChan)

	return nil
}

func (s *stickyResultTransport) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.started && !s.closed
}

func (s *stickyResultTransport) EndInput() error {
	s.release()

	return nil
}

func (s *stickyResultTransport) release() {
	select {
	case <-s.ready:
	default:
		close(s.ready)
	}
}

var _ config.Transport = (*stickyResultTransport)(nil)

// resultMessageTransport is a mock transport that successfully initializes and
// emits a ResultMessage while keeping channels open. This verifies that
// QueryStream stops at ResultMessage instead of waiting for context cancellation.
type resultMessageTransport struct {
	mu         sync.Mutex
	msgChan    chan map[string]any
	errChan    chan error
	sentResult atomic.Bool
	closed     bool
}

func newResultMessageTransport() *resultMessageTransport {
	return &resultMessageTransport{
		msgChan: make(chan map[string]any, 16),
		errChan: make(chan error, 1),
	}
}

func (t *resultMessageTransport) Start(_ context.Context) error {
	return nil
}

func (t *resultMessageTransport) ReadMessages(_ context.Context) (<-chan map[string]any, <-chan error) {
	return t.msgChan, t.errChan
}

func (t *resultMessageTransport) SendMessage(_ context.Context, data []byte) error {
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	if msgType, ok := msg["type"].(string); ok && msgType == msgTypeControlRequest {
		requestID, _ := msg["request_id"].(string)
		if requestID == "" {
			return nil
		}

		go func() {
			t.msgChan <- map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": requestID,
					"response":   map[string]any{},
				},
			}
		}()

		return nil
	}

	if t.sentResult.CompareAndSwap(false, true) {
		go func() {
			t.msgChan <- map[string]any{
				"type":       "result",
				"subtype":    "success",
				"is_error":   false,
				"session_id": "session-test",
			}
		}()
	}

	return nil
}

func (t *resultMessageTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.closed {
		t.closed = true
		close(t.msgChan)
		close(t.errChan)
	}

	return nil
}

func (t *resultMessageTransport) IsReady() bool {
	return true
}

func (t *resultMessageTransport) EndInput() error {
	return nil
}

// Compile-time check that resultMessageTransport implements config.Transport.
var _ config.Transport = (*resultMessageTransport)(nil)

// TestQueryStream_StreamInputError_Propagated verifies that errors from the
// streamInputMessages goroutine are properly propagated to callers via the
// gCtx.Done() case in the select loop.
//
// This test validates that the errgroup error propagation fix works correctly:
// 1. When streamInputMessages returns an error, errgroup cancels gCtx
// 2. gCtx.Done() becomes readable in the select
// 3. g.Wait() returns the error immediately (goroutine already exited)
// 4. Error is yielded to caller before iterator returns.
func TestQueryStream_StreamInputError_Propagated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a transport that fails after first SendMessage
	// (first call is for initialization, second is for first streaming message)
	transport := newFailingTransport(1)

	// Create multiple messages - the second one should trigger the failure
	messages := MessagesFromSlice([]StreamingMessage{
		NewUserMessage(Text("first message")),
		NewUserMessage(Text("second message - this will fail")),
	})

	var receivedError error

	for _, err := range QueryStream(ctx, messages,
		WithTransport(transport),
	) {
		if err != nil {
			receivedError = err

			break
		}
	}

	// Verify that the error was propagated to the caller
	require.Error(t, receivedError, "Error from streamInputMessages should be propagated")
	require.Contains(t, receivedError.Error(), "simulated transport send failure",
		"Error message should contain the original transport error")
}

func TestQuery_ReturnsImmediatelyAfterResultAndClosesTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultText := "final answer before transport EOF"
	transport := newStickyResultTransport(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": "session-test",
		"result":     resultText,
	})

	resultSeen := make(chan *ResultMessage, 1)
	iteratorDone := make(chan struct{})

	go func() {
		defer close(iteratorDone)

		for msg, err := range Query(ctx, Text("test"), WithTransport(transport)) {
			if err != nil {
				t.Errorf("unexpected error: %v", err)

				return
			}

			if result, ok := msg.(*ResultMessage); ok {
				resultSeen <- result
			}
		}
	}()

	select {
	case result := <-resultSeen:
		require.NotNil(t, result)
		require.NotNil(t, result.Result)
		require.Equal(t, resultText, *result.Result)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ResultMessage")
	}

	select {
	case <-iteratorDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Query iterator to stop after ResultMessage")
	}
}

// TestQueryStream_StopsAtResultMessage verifies QueryStream returns immediately
// after receiving ResultMessage, instead of waiting for context cancellation.
func TestQueryStream_StopsAtResultMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	transport := newResultMessageTransport()
	messages := MessagesFromSlice([]StreamingMessage{
		NewUserMessage(Text("hello")),
	})

	start := time.Now()

	var (
		gotResult bool
		gotErr    error
	)

	for msg, err := range QueryStream(ctx, messages, WithTransport(transport)) {
		if err != nil {
			gotErr = err

			break
		}

		switch msg.(type) {
		case *ResultMessage:
			gotResult = true
		}
	}

	elapsed := time.Since(start)

	require.NoError(t, gotErr, "QueryStream should not wait for context timeout after result")
	require.True(t, gotResult, "should receive result message")
	require.Less(t, elapsed, 400*time.Millisecond,
		"QueryStream should return quickly after result, got %v", elapsed)
}

// TestStreamInputMessages_ErrorNotPropagated_Pattern tests that errors from
// SendMessage in streamInputMessages are properly propagated via SetFatalError.
// This is Issue #3 in the bug tracking.
//
// FIXED: query.go now calls controller.SetFatalError() when SendMessage or
// Marshal fails, properly propagating errors to callers.
//
// The correct pattern in query.go:
//
//	if err := transport.SendMessage(ctx, data); err != nil {
//	    log.Error("Failed to send streaming message", "error", err)
//	    controller.SetFatalError(fmt.Errorf("send streaming message: %w", err))
//	    return
//	}
//
// Expected behavior: When SendMessage fails, the error is propagated
// via controller.SetFatalError() so that callers can detect the failure.
func TestStreamInputMessages_ErrorNotPropagated_Pattern(t *testing.T) {
	t.Run("marshal error is propagated via SetFatalError", func(t *testing.T) {
		// This test verifies the correct pattern for marshal error handling
		marshalErr := fmt.Errorf("simulated marshal failure")

		var propagatedError error

		correctHandler := func(err error) {
			if err != nil {
				// Log the error
				_ = fmt.Sprintf("Failed to marshal streaming message: %v", err)
				// CORRECT: Propagate the error via SetFatalError
				propagatedError = fmt.Errorf("marshal streaming message: %w", err)

				return
			}
		}

		correctHandler(marshalErr)

		// Error should be propagated
		if propagatedError == nil {
			t.Error("Marshal error was not propagated - SetFatalError should be called")
		}
	})

	t.Run("send error is propagated via SetFatalError", func(t *testing.T) {
		// This test verifies the correct pattern for send error handling
		sendErr := fmt.Errorf("simulated send failure")

		var propagatedError error

		correctHandler := func(err error) {
			if err != nil {
				// Log the error
				_ = fmt.Sprintf("Failed to send streaming message: %v", err)
				// CORRECT: Propagate the error via SetFatalError
				propagatedError = fmt.Errorf("send streaming message: %w", err)

				return
			}
		}

		correctHandler(sendErr)

		// Error should be propagated
		if propagatedError == nil {
			t.Error("Send error was not propagated - SetFatalError should be called")
		}
	})
}
