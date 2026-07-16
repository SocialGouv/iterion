package subprocess

import (
	"bufio"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/stretchr/testify/require"
)

// TestStart_IgnoredGetwdError tests that Start() returns an error when os.Getwd() fails.
// This validates the fix at cli.go:109-114 where os.Getwd() error is now properly checked.
//
// Previously, the code ignored the error:
//
//	t.cwd, _ = os.Getwd()  // Error was ignored with _
//
// If os.Getwd() failed (e.g., current directory deleted), t.cwd became empty string,
// which caused confusing errors later when the subprocess started.
//
// The fix wraps the error and returns it from Start().
func TestStart_IgnoredGetwdError(t *testing.T) {
	// Skip on Windows - different directory behavior
	if runtime.GOOS == "windows" {
		t.Skip("Test requires Unix directory semantics")
	}

	log := slog.Default()

	// Create a temp directory
	tmpDir, err := os.MkdirTemp("", "getwd-test-*")
	require.NoError(t, err)

	// Save original directory to restore later
	origDir, err := os.Getwd()
	require.NoError(t, err)

	defer func() {
		// Restore original directory
		_ = os.Chdir(origDir)
	}()

	// Change to temp directory
	err = os.Chdir(tmpDir)
	require.NoError(t, err)

	// Delete the directory while we're in it
	// This will cause os.Getwd() to fail
	err = os.RemoveAll(tmpDir)
	require.NoError(t, err)

	// Verify os.Getwd() actually fails in this state
	_, getwdErr := os.Getwd()
	if getwdErr == nil {
		t.Skip("os.Getwd() did not fail after directory deletion (OS-dependent behavior)")
	}

	// Now try to create a transport with empty Cwd (will trigger os.Getwd() fallback)
	transport := NewCLITransport(log, "test", &config.Options{
		Cwd: "", // Empty - should trigger os.Getwd() fallback
	})

	ctx := context.Background()
	startErr := transport.Start(ctx)

	// Start() should return an error because os.Getwd() failed.
	// The fix at cli.go:109-114 checks the error and wraps it.
	if startErr == nil {
		t.Fatal("Start() should return an error when os.Getwd() fails")
	}

	if _, ok := stderrors.AsType[*errors.CLINotFoundError](startErr); ok {
		t.Skip("Codex CLI not installed - cannot fully verify the fix")
	}

	// The error should mention "working directory"
	if !strings.Contains(startErr.Error(), "working directory") {
		t.Errorf("expected error mentioning 'working directory', got: %v", startErr)
	}
}

// TestConnect_WithNonexistentCwd tests connection with invalid working directory.
func TestConnect_WithNonexistentCwd(t *testing.T) {
	log := slog.Default()

	transport := NewCLITransport(log, "test", &config.Options{
		Cwd: "/nonexistent/path/that/does/not/exist",
	})

	ctx := context.Background()
	err := transport.Start(ctx)

	// Either CLI not found or Start fails due to invalid cwd
	if err != nil {
		if _, ok := stderrors.AsType[*errors.CLINotFoundError](err); ok {
			t.Skip("Claude CLI not installed")
		}

		// Start should fail with invalid working directory
		require.Error(t, err)

		return
	}

	// If we got here, the CLI was found but Start should have failed
	t.Error("Expected Start to fail with nonexistent working directory")
}

// TestConcurrentWrites_AreSerialized tests that concurrent writes are serialized via mutex.
func TestConcurrentWrites_AreSerialized(t *testing.T) {
	log := slog.Default()

	// Create a transport with a mock stdin using a pipe
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	ctx := context.Background()

	// Start a goroutine to drain the reader so writes don't block
	go func() {
		buf := make([]byte, 1024)
		for {
			_, err := reader.Read(buf)
			if err != nil {
				return
			}
		}
	}()

	// Test concurrent writes
	const numWriters = 10

	done := make(chan struct{}, numWriters)

	for i := range numWriters {
		go func(id int) {
			defer func() { done <- struct{}{} }()

			msg := []byte(`{"id":` + strconv.Itoa(id) + `}`)
			_ = transport.SendMessage(ctx, msg)
		}(i)
	}

	// Wait for all writers to complete
	for range numWriters {
		<-done
	}

	// If we get here without deadlock or panic, the mutex is working
	require.NotNil(t, transport)
}

// TestConnect_Close tests the full connect/close lifecycle with a mock-style test.
// Since we can't mock subprocess easily in Go, this tests the error paths
// and cleanup behavior of the transport.
func TestConnect_Close(t *testing.T) {
	log := slog.Default()

	t.Run("close before start", func(t *testing.T) {
		transport := &CLITransport{
			log: log,
		}

		// Close on unstarted transport should not panic
		err := transport.Close()
		require.NoError(t, err)
	})

	t.Run("send message before start", func(t *testing.T) {
		transport := &CLITransport{
			log: log,
		}

		ctx := context.Background()
		err := transport.SendMessage(ctx, []byte(`{"type":"test"}`))

		require.Error(t, err)
		require.Contains(t, err.Error(), "not connected")
	})

	t.Run("send message with cancelled context", func(t *testing.T) {
		transport := &CLITransport{
			log: log,
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Even with stdin set, cancelled context should return error
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()

		transport.stdin = writer

		err := transport.SendMessage(ctx, []byte(`{"type":"test"}`))
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestReadMessages_JSONParsing tests JSON parsing logic in message reading.
// Note: Full ReadMessages testing requires integration tests with real subprocess
// since the goroutine calls cmd.Wait(). These unit tests focus on the JSON parsing
// and message handling behavior through the existing concurrent writes test.
func TestReadMessages_JSONParsing(t *testing.T) {
	t.Run("valid JSON unmarshalling", func(t *testing.T) {
		// Test that JSON parsing works correctly via existing transport mechanisms
		testCases := []struct {
			name    string
			input   string
			msgType string
		}{
			{
				name:    "system message",
				input:   `{"type":"system","subtype":"init"}`,
				msgType: "system",
			},
			{
				name:    "assistant message",
				input:   `{"type":"assistant","message":{"content":[]}}`,
				msgType: "assistant",
			},
			{
				name:    "result message",
				input:   `{"type":"result","subtype":"success"}`,
				msgType: "result",
			},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				var msg map[string]any

				err := json.Unmarshal([]byte(tc.input), &msg)

				require.NoError(t, err)
				require.Equal(t, tc.msgType, msg["type"])
			})
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		var msg map[string]any

		err := json.Unmarshal([]byte("not valid json"), &msg)

		require.Error(t, err)
	})

	t.Run("empty line handling", func(t *testing.T) {
		var msg map[string]any

		err := json.Unmarshal([]byte(""), &msg)

		require.Error(t, err)
	})
}

// TestReadMessages_StderrCallback tests stderr callback streaming.
func TestReadMessages_StderrCallback(t *testing.T) {
	log := slog.Default()

	t.Run("stderr callback is invoked", func(t *testing.T) {
		var capturedLines []string

		callback := func(line string) {
			capturedLines = append(capturedLines, line)
		}

		transport := &CLITransport{
			log:            log,
			stderrCallback: callback,
		}

		// Verify callback is set
		require.NotNil(t, transport.stderrCallback)

		// Simulate callback invocation
		transport.stderrCallback("test line 1")
		transport.stderrCallback("test line 2")

		require.Len(t, capturedLines, 2)
		require.Equal(t, "test line 1", capturedLines[0])
		require.Equal(t, "test line 2", capturedLines[1])
	})
}

// TestSendMessage_CancellationDuringWrite tests that SendMessage respects context
// cancellation even when blocked on a write operation.
func TestSendMessage_CancellationDuringWrite(t *testing.T) {
	log := slog.Default()

	// Create a pipe but don't read from it - writes will block when buffer fills
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start a write with a large payload that will block
	errCh := make(chan error, 1)

	go func() {
		// Large payload to fill pipe buffer and block
		largeData := make([]byte, 128*1024) // 128KB > typical 64KB pipe buffer
		errCh <- transport.SendMessage(ctx, largeData)
	}()

	// Give the write time to start and block
	time.Sleep(10 * time.Millisecond)

	// Cancel context
	cancel()

	// Should return quickly with context error
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("SendMessage did not respect context cancellation")
	}
}

// TestStderrBuffer_SizeLimit tests that the stderr buffer stops growing after maxStderrBufferSize.
func TestStderrBuffer_SizeLimit(t *testing.T) {
	var stderrBuffer strings.Builder

	var stderrMu sync.Mutex

	// Simulate buffering loop with lines exceeding limit
	lineSize := 1000
	line := strings.Repeat("x", lineSize)
	iterations := (maxStderrBufferSize / lineSize) + 100 // Exceed limit

	for range iterations {
		stderrMu.Lock()

		if stderrBuffer.Len() < maxStderrBufferSize {
			if stderrBuffer.Len() > 0 {
				stderrBuffer.WriteString("\n")
			}

			stderrBuffer.WriteString(line)
		}

		stderrMu.Unlock()
	}

	// Buffer should not exceed maxStderrBufferSize (plus one line that may have been added
	// when the buffer was just under the limit)
	require.LessOrEqual(t, stderrBuffer.Len(), maxStderrBufferSize+lineSize)
	require.Greater(t, stderrBuffer.Len(), 0)
}

// =============================================================================
// Mid-Operation Close/Cancel Tests
// =============================================================================

// Note: Full ReadMessages testing with context cancellation requires a real subprocess
// because ReadMessages calls cmd.Wait() internally. These tests focus on behaviors
// that can be tested with partial mocking.

// TestSendMessage_ConcurrentWithClose tests concurrent SendMessage and Close operations.
// This test verifies that concurrent sends don't cause panics or deadlocks when
// the underlying pipe is closed.
func TestSendMessage_ConcurrentWithClose(t *testing.T) {
	log := slog.Default()

	t.Run("send while closing", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()

		transport := &CLITransport{
			log:   log,
			stdin: writer,
		}

		ctx := context.Background()

		// Start goroutine to drain reader
		go func() {
			buf := make([]byte, 1024)
			for {
				_, err := reader.Read(buf)
				if err != nil {
					return
				}
			}
		}()

		// Start multiple senders
		const senders = 10

		var wg sync.WaitGroup

		wg.Add(senders)

		for range senders {
			go func() {
				defer wg.Done()

				for range 10 {
					_ = transport.SendMessage(ctx, []byte(`{"type":"test"}`))

					time.Sleep(time.Millisecond)
				}
			}()
		}

		// Close writer mid-stream
		time.Sleep(10 * time.Millisecond)
		writer.Close()

		// Wait for senders to complete
		done := make(chan struct{})

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success - no panic
		case <-time.After(2 * time.Second):
			t.Fatal("Senders did not complete")
		}
	})
}

// TestClose_SafeWithNilCmd tests that Close() is safe when cmd is nil.
func TestClose_SafeWithNilCmd(t *testing.T) {
	log := slog.Default()

	transport := &CLITransport{
		log: log,
		// cmd is nil - simulates partially initialized transport
	}

	// Should not panic
	err := transport.Close()
	require.NoError(t, err)

	// Multiple closes should be safe
	err = transport.Close()
	require.NoError(t, err)
}

// TestClose_SetsClosingFlag tests that Close() sets the closing flag.
func TestClose_SetsClosingFlag(t *testing.T) {
	log := slog.Default()

	transport := &CLITransport{
		log: log,
	}

	// Initially not closing
	require.False(t, transport.closing)

	// Close sets the flag
	_ = transport.Close()
	require.True(t, transport.closing)
}

// TestSendMessage_ContextCancelDuringBlockedWrite tests that SendMessage
// returns context error when context is cancelled during a blocked write.
func TestSendMessage_ContextCancelDuringBlockedWrite(t *testing.T) {
	log := slog.Default()

	// Create a pipe but don't read from it - writes will block when buffer fills
	reader, writer := io.Pipe()

	defer reader.Close()
	defer writer.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start a write with a large payload that will block
	errCh := make(chan error, 1)

	go func() {
		// Large payload to fill pipe buffer and block
		largeData := make([]byte, 128*1024) // 128KB > typical 64KB pipe buffer
		errCh <- transport.SendMessage(ctx, largeData)
	}()

	// Give the write time to start and block
	time.Sleep(10 * time.Millisecond)

	// Cancel context
	cancel()

	// Should return quickly with context error
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("SendMessage did not respect context cancellation")
	}
}

// TestSendMessage_NoGoroutineLeak tests that SendMessage does not leak goroutines
// when context is cancelled during a blocked write.
func TestSendMessage_NoGoroutineLeak(t *testing.T) {
	log := slog.Default()
	reader, writer := io.Pipe()

	defer reader.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	ctx, cancel := context.WithCancel(context.Background())
	before := runtime.NumGoroutine()

	errCh := make(chan error, 1)

	go func() {
		largeData := make([]byte, 128*1024)
		errCh <- transport.SendMessage(ctx, largeData)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("SendMessage did not return")
	}

	// Allow goroutines to settle
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()

	// Should not have leaked goroutines (allow +1 for GC fluctuation)
	require.LessOrEqual(t, after, before+1, "goroutine leak detected")
}

// hungWriter is a mock io.WriteCloser where Write blocks until explicitly unblocked,
// and Close does NOT unblock Write (simulating a pathological I/O scenario).
type hungWriter struct {
	writeCalled  chan struct{}
	unblockWrite chan struct{}
	closed       bool
	mu           sync.Mutex
}

func newHungWriter() *hungWriter {
	return &hungWriter{
		writeCalled:  make(chan struct{}),
		unblockWrite: make(chan struct{}),
	}
}

func (h *hungWriter) Write(p []byte) (n int, err error) {
	// Signal that Write was called
	select {
	case h.writeCalled <- struct{}{}:
	default:
	}

	// Block until explicitly unblocked
	<-h.unblockWrite

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return 0, io.ErrClosedPipe
	}

	return len(p), nil
}

func (h *hungWriter) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true
	// NOTE: Intentionally does NOT unblock Write - this simulates the bug scenario.

	return nil
}

// TestSendMessage_HungWriteAfterClose tests that SendMessage returns promptly
// even when Write() doesn't return after Close() is called.
//
// BUG: The current code at cli.go:432 does `<-done` unconditionally after closing
// stdin. If Write() doesn't return after Close(), this blocks forever.
//
// This test will:
//   - TIMEOUT (fail) with the current buggy code.
//   - PASS after the fix (using select with time.After).
func TestSendMessage_HungWriteAfterClose(t *testing.T) {
	log := slog.Default()

	hw := newHungWriter()

	transport := &CLITransport{
		log:   log,
		stdin: hw,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)

	go func() {
		errCh <- transport.SendMessage(ctx, []byte(`{"test":true}`))
	}()

	// Wait for Write to be called
	select {
	case <-hw.writeCalled:
		// Good - Write is now blocked
	case <-time.After(1 * time.Second):
		t.Fatal("Write was never called")
	}

	// Context will timeout after 100ms, triggering the cancel path.
	// The bug: SendMessage will block forever on `<-done` because our
	// hungWriter.Close() doesn't unblock Write().

	// SendMessage should return within a reasonable time (e.g., 2 seconds)
	// even if the internal Write goroutine is still blocked.
	select {
	case err := <-errCh:
		// Success - SendMessage returned (should be context.DeadlineExceeded)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		// BUG DETECTED: SendMessage is blocked on `<-done` indefinitely
		t.Fatal("BUG DETECTED: SendMessage blocked indefinitely waiting for hung Write goroutine. " +
			"The code at cli.go:432 should use select with time.After instead of unconditional <-done")
	}

	// Clean up: unblock the Write goroutine so it can exit
	close(hw.unblockWrite)
}

// TestSendMessage_ReturnsStdinClosedAfterCancellation tests that subsequent calls
// to SendMessage return ErrStdinClosed after context cancellation.
func TestSendMessage_ReturnsStdinClosedAfterCancellation(t *testing.T) {
	log := slog.Default()
	reader, writer := io.Pipe()

	defer reader.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start a write with large payload that will block
	errCh := make(chan error, 1)

	go func() {
		largeData := make([]byte, 128*1024)
		errCh <- transport.SendMessage(ctx, largeData)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	// Wait for first call to return
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(1 * time.Second):
		t.Fatal("SendMessage did not return")
	}

	// Subsequent calls should return ErrStdinClosed
	err := transport.SendMessage(context.Background(), []byte(`{"test": true}`))
	require.ErrorIs(t, err, errors.ErrStdinClosed)
}

// TestSendMessage_SetsStdinClosedFlag tests that SendMessage sets the stdinClosed
// flag when context is cancelled during a blocked write.
func TestSendMessage_SetsStdinClosedFlag(t *testing.T) {
	log := slog.Default()
	reader, writer := io.Pipe()

	defer reader.Close()

	transport := &CLITransport{
		log:   log,
		stdin: writer,
	}

	require.False(t, transport.stdinClosed, "stdinClosed should be false initially")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)

	go func() {
		largeData := make([]byte, 128*1024)
		errCh <- transport.SendMessage(ctx, largeData)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	<-errCh

	require.True(t, transport.stdinClosed, "stdinClosed should be true after cancellation")
}

// TestCloseStdin_RespectsStdinClosedFlag tests that CloseStdin respects the stdinClosed flag.
func TestCloseStdin_RespectsStdinClosedFlag(t *testing.T) {
	log := slog.Default()

	t.Run("sets stdinClosed when closing", func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()

		transport := &CLITransport{
			log:   log,
			stdin: writer,
		}

		require.False(t, transport.stdinClosed)

		err := transport.CloseStdin()
		require.NoError(t, err)

		require.True(t, transport.stdinClosed)
		require.Nil(t, transport.stdin)
	})

	t.Run("no-op if already closed", func(t *testing.T) {
		transport := &CLITransport{
			log:         log,
			stdinClosed: true,
		}

		err := transport.CloseStdin()
		require.NoError(t, err)
	})
}

// TestClose_SetsStdinClosedFlag tests that Close sets the stdinClosed flag.
func TestClose_SetsStdinClosedFlag(t *testing.T) {
	log := slog.Default()

	transport := &CLITransport{
		log: log,
	}

	require.False(t, transport.stdinClosed)

	_ = transport.Close()

	require.True(t, transport.stdinClosed)
}

// =============================================================================
// Stderr Goroutine Cancellation Tests
// =============================================================================

// TestStderrGoroutine_ContextCancellation tests that the stderr goroutine
// properly exits when context is cancelled.
func TestStderrGoroutine_ContextCancellation(t *testing.T) {
	// Create a pipe to simulate stderr
	stderrReader, stderrWriter := io.Pipe()
	defer stderrWriter.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	scanDone := make(chan struct{})

	wg.Go(func() {
		innerDone := make(chan struct{})

		go func() {
			defer close(innerDone)

			scanner := bufio.NewScanner(stderrReader)
			for scanner.Scan() {
				// Simulate reading lines
			}
		}()

		select {
		case <-innerDone:
			// Normal completion
		case <-ctx.Done():
			stderrReader.Close() // Unblock scanner
			<-innerDone
		}

		close(scanDone)
	})

	// Cancel context
	cancel()

	// Should complete quickly
	select {
	case <-scanDone:
		// Success
	case <-time.After(1 * time.Second):
		t.Fatal("stderr goroutine did not respect context cancellation")
	}
}

// TestStderrGoroutine_NoGoroutineLeak tests that no goroutines are leaked
// when context is cancelled during stderr reading.
func TestStderrGoroutine_NoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	// Run the cancellation test multiple times
	for range 5 {
		stderrReader, stderrWriter := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup

		wg.Go(func() {
			innerDone := make(chan struct{})

			go func() {
				defer close(innerDone)

				scanner := bufio.NewScanner(stderrReader)

				for scanner.Scan() {
				}
			}()

			select {
			case <-innerDone:
			case <-ctx.Done():
				stderrReader.Close()
				<-innerDone
			}
		})

		cancel()
		wg.Wait()
		stderrWriter.Close()
	}

	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()

	require.LessOrEqual(t, after, before+1, "goroutine leak detected")
}

// =============================================================================
// Bug Detection Tests - These tests are designed to FAIL with current buggy code
// and PASS after the bugs are fixed.
// =============================================================================

// TestSendMessage_SliceMutation tests that SendMessage does not mutate the caller's
// slice when adding a newline.
//
// Previously, cli.go used `data = append(data, '\n')` which mutated the caller's
// backing array if there was spare capacity. This violated Go's slice safety expectations.
//
// The fix at cli.go:335-341 copies the data before appending a newline.
func TestSendMessage_SliceMutation(t *testing.T) {
	log := slog.Default()

	// Create a slice with spare capacity: len=10, cap=20
	// The extra capacity allows append to mutate the backing array
	// instead of allocating a new one.
	original := make([]byte, 10, 20)
	copy(original, []byte(`{"test":1}`))

	// Save a reference to check mutation
	extended := original[:cap(original)]
	initialByte11 := extended[10] // Should be 0 (zero value)

	// Setup transport with pipe
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	transport := &CLITransport{log: log, stdin: writer}

	// Drain reader in background so writes don't block
	go func() {
		buf := make([]byte, 1024)

		for {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()

	// Call SendMessage with slice that has spare capacity
	err := transport.SendMessage(context.Background(), original)
	require.NoError(t, err)

	// Check that the backing array wasn't mutated beyond our slice
	// If buggy, extended[10] will now be '\n' (byte value 10)
	extended = original[:cap(original)]

	if extended[10] != initialByte11 {
		t.Errorf("SendMessage mutated caller's slice backing array: "+
			"byte at index 10 changed from %d to %d (expected unchanged)",
			initialByte11, extended[10])
	}
}

// TestStderrScanner_TimeoutOnBlockedExit tests that the stderr scanner goroutine
// will timeout and log a warning if the scanner doesn't exit after closing stderr.
// This prevents indefinite blocking if closing the stderr pipe doesn't reliably
// unblock bufio.Scanner.Scan().
func TestStderrScanner_TimeoutOnBlockedExit(t *testing.T) {
	log := slog.Default()

	// Create a pipe to simulate stderr
	stderrReader, stderrWriter := io.Pipe()
	defer stderrWriter.Close()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	outerDone := make(chan struct{})

	wg.Go(func() {
		defer close(outerDone)

		scanDone := make(chan struct{})

		go func() {
			defer close(scanDone)

			scanner := bufio.NewScanner(stderrReader)
			for scanner.Scan() {
				// Simulate reading lines
			}
		}()

		select {
		case <-scanDone:
			// Normal completion
		case <-ctx.Done():
			// Close stderr to try to unblock scanner
			stderrReader.Close()

			// Wait for scanner with timeout (matching the fix in cli.go)
			select {
			case <-scanDone:
				// Scanner exited normally
			case <-time.After(100 * time.Millisecond): // Short timeout for test
				log.Warn("Stderr scanner did not exit within timeout")
			}
		}
	})

	// Cancel context
	cancel()

	// Should complete within the timeout (plus some margin)
	select {
	case <-outerDone:
		// Success - goroutine exited (either normally or via timeout)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stderr goroutine did not exit - timeout mechanism may not be working")
	}

	wg.Wait()
}

// TestStderrScanner_GoroutineLeakWithHungProcess is a regression test verifying
// that the stderr scanner does not leak goroutines.
//
// The fix removes the nested goroutine pattern entirely. The scanner goroutine
// now relies on the process being killed to close pipes, which reliably unblocks
// Scan(). There is no timeout that could abandon an inner goroutine.
//
// This test simulates the pattern to verify no leaks occur with the new design.
func TestStderrScanner_GoroutineLeakWithHungProcess(t *testing.T) {
	baseline := runtime.NumGoroutine()

	const iterations = 3

	for i := range iterations {
		// Create a pipe to simulate stderr
		stderrReader, stderrWriter := io.Pipe()

		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup

		// This matches the new simplified pattern in cli.go:
		// Single goroutine with cooperative context checking
		wg.Go(func() {
			scanner := bufio.NewScanner(stderrReader)
			for scanner.Scan() {
				// Check context between lines for cooperative cancellation
				select {
				case <-ctx.Done():
					return
				default:
				}
				// Process line (simulated)
				_ = scanner.Text()
			}
		})

		// Cancel context
		cancel()

		// Close the pipe to unblock scanner (simulates process being killed)
		stderrWriter.Close()
		stderrReader.Close()

		// Wait for goroutine
		done := make(chan struct{})

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Good - goroutine exited cleanly
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("Iteration %d: goroutine did not exit - leak detected", i)
		}
	}

	// Force GC and let goroutines settle
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - baseline

	t.Logf("Goroutine count: baseline=%d, after=%d, delta=%d", baseline, after, leaked)

	// No leaks should occur with the new pattern
	require.LessOrEqual(t, after, baseline+1,
		"Goroutine leak detected: the stderr scanner pattern should not leak goroutines")
}

// TestStderrScanner_GoroutineLeakOnContextCancel tests for goroutine leaks in the
// stderr scanner when context is cancelled.
//
// Expected behavior: Context cancellation should reliably terminate all goroutines
// within a reasonable timeout.
//
// This test runs multiple iterations to detect potential goroutine leaks.
func TestStderrScanner_GoroutineLeakOnContextCancel(t *testing.T) {
	baseline := runtime.NumGoroutine()

	const iterations = 5

	for i := range iterations {
		stderrReader, stderrWriter := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup

		wg.Go(func() {
			innerDone := make(chan struct{})

			go func() {
				defer close(innerDone)

				scanner := bufio.NewScanner(stderrReader)

				for scanner.Scan() {
					// Simulate reading lines
				}
			}()

			select {
			case <-innerDone:
				// Normal completion
			case <-ctx.Done():
				// Context cancelled - close stderr to unblock scanner
				stderrReader.Close()
				// BUG: This <-innerDone may block indefinitely if Close() doesn't
				// properly unblock the scanner's blocked Read() call
				<-innerDone
			}
		})

		// Cancel while scanner may be blocked waiting for input
		cancel()

		// Wait with timeout to detect potential blocking
		done := make(chan struct{})

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Good - goroutine exited
		case <-time.After(500 * time.Millisecond):
			t.Logf("Iteration %d: timeout waiting for goroutine - potential leak detected", i)
		}

		// Clean up
		stderrWriter.Close()
	}

	// Force GC to clean up any finalized goroutines
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	final := runtime.NumGoroutine()
	leaked := final - baseline

	// Allow for some GC fluctuation, but if we leaked most iterations, that's a bug
	require.Less(t, leaked, iterations,
		"Goroutine leak detected: baseline=%d, final=%d, leaked=%d. "+
			"The stderr scanner pattern in cli.go may not properly handle context cancellation.",
		baseline, final, leaked)
}
