package subprocess

import (
	"bufio"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/cli"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
)

const (
	// maxScanTokenSize is the maximum buffer size for reading CLI output lines.
	maxScanTokenSize = 1024 * 1024 // 1MB
	// maxStderrBufferSize is the maximum size for the stderr buffer.
	maxStderrBufferSize = 10 * 1024 * 1024 // 10MB
)

// CLITransport implements Transport by spawning a Codex CLI subprocess.
type CLITransport struct {
	log            *slog.Logger
	options        *config.Options
	prompt         string
	cliPath        string
	args           []string
	env            []string
	cwd            string
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	stderr         io.ReadCloser
	stderrCallback func(string)
	mu             sync.Mutex
	isStreaming    bool
	closing        bool
	stdinClosed    bool
	closeOnce      sync.Once
	closeCh        chan struct{}
}

// Compile-time verification that CLITransport implements the Transport interface.
var _ config.Transport = (*CLITransport)(nil)

// NewCLITransport creates a new CLI transport for one-shot exec mode.
func NewCLITransport(
	log *slog.Logger,
	prompt string,
	options *config.Options,
) *CLITransport {
	return NewCLITransportWithMode(log, prompt, options, false)
}

// NewCLITransportWithMode creates a new CLI transport with explicit mode control.
//
// When isStreaming is true, the transport spawns `codex app-server` and keeps
// stdin open for bidirectional JSON-RPC. When false, it spawns `codex exec`
// for one-shot queries.
func NewCLITransportWithMode(
	log *slog.Logger,
	prompt string,
	options *config.Options,
	isStreaming bool,
) *CLITransport {
	return &CLITransport{
		log:            log.With("component", "cli_transport"),
		options:        options,
		prompt:         prompt,
		stderrCallback: options.Stderr,
		isStreaming:    isStreaming,
		closeCh:        make(chan struct{}),
	}
}

// Start starts the CLI subprocess.
func (t *CLITransport) Start(ctx context.Context) error {
	t.log.InfoContext(ctx, "starting Codex CLI subprocess")

	discoverer := cli.NewDiscoverer(&cli.Config{
		CliPath:          t.options.CliPath,
		SkipVersionCheck: t.options.SkipVersionCheck,
		Logger:           t.log,
	})

	cliPath, err := discoverer.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover CLI: %w", err)
	}

	t.cliPath = cliPath

	// Build command arguments based on mode
	if t.isStreaming {
		t.args = cli.BuildAppServerArgs(t.options)
	} else {
		t.args = cli.BuildExecArgs(t.prompt, t.options)
	}

	t.log.DebugContext(ctx, "built command arguments", slog.Any("args", t.args))

	t.env = cli.BuildEnvironment(t.options)

	t.cwd = t.options.Cwd
	if t.cwd == "" {
		t.cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}

	//nolint:gosec // CLI path is validated by the discoverer; args are built internally.
	cmd := exec.CommandContext(ctx, t.cliPath, t.args...)
	cmd.Dir = t.cwd
	cmd.Env = t.env
	configureProcessGroup(cmd)

	// Set up stdin pipe
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &errors.CLIConnectionError{Err: fmt.Errorf("stdin pipe: %w", err)}
	}

	t.stdin = stdin

	// Set up stdout pipe
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &errors.CLIConnectionError{Err: fmt.Errorf("stdout pipe: %w", err)}
	}

	t.stdout = stdout

	// Set up stderr pipe
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return &errors.CLIConnectionError{Err: fmt.Errorf("stderr pipe: %w", err)}
	}

	t.stderr = stderr

	if err := cmd.Start(); err != nil {
		return &errors.CLIConnectionError{Err: fmt.Errorf("start process: %w", err)}
	}

	t.cmd = cmd
	t.log.InfoContext(ctx, "Codex CLI subprocess started", slog.Int("pid", cmd.Process.Pid))
	t.watchContextCancellation(ctx)

	return nil
}

func (t *CLITransport) watchContextCancellation(ctx context.Context) {
	if ctx == nil || t.closeCh == nil {
		return
	}

	go func() {
		select {
		case <-ctx.Done():
			t.log.Debug("Context cancelled, terminating Codex CLI process tree", "error", ctx.Err())

			if err := t.Close(); err != nil && !stderrors.Is(err, os.ErrProcessDone) {
				t.log.Debug("Failed to terminate Codex CLI process tree after context cancellation", "error", err)
			}
		case <-t.closeCh:
		}
	}()
}

// ReadMessages reads JSON messages from the CLI stdout.
func (t *CLITransport) ReadMessages(
	ctx context.Context,
) (<-chan map[string]any, <-chan error) {
	messages := make(chan map[string]any)
	errs := make(chan error, 1)

	var stderrWg sync.WaitGroup

	var stderrBuffer strings.Builder

	var stderrMu sync.Mutex

	// Always buffer stderr for error reporting (must complete reads before Wait())
	// See: https://pkg.go.dev/os/exec#Cmd.StderrPipe

	stderrWg.Go(func() {
		// Simple scanner loop - relies on process kill to close pipes and unblock Scan().
		// No nested goroutine needed: when Close() kills the process, the OS closes all
		// pipes, which reliably returns from blocked Read() calls.
		scanner := bufio.NewScanner(t.stderr)
		for scanner.Scan() {
			// Check context between lines for cooperative cancellation
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()

			// Buffer stderr for error reporting (capped at maxStderrBufferSize)
			stderrMu.Lock()

			if stderrBuffer.Len() < maxStderrBufferSize {
				if stderrBuffer.Len() > 0 {
					stderrBuffer.WriteString("\n")
				}

				stderrBuffer.WriteString(line)
			}

			stderrMu.Unlock()

			// Invoke callback if set
			if t.stderrCallback != nil {
				t.stderrCallback(line)
			}
		}

		// Log scanner errors (don't fail - process may have exited)
		if err := scanner.Err(); err != nil {
			t.log.Debug("Stderr scanner error", "error", err)
		}
	})

	go func() {
		defer close(messages)
		defer close(errs)
		defer t.log.Debug("ReadMessages goroutine stopped")

		scanner := bufio.NewScanner(t.stdout)
		// Set large buffer for big messages
		buf := make([]byte, maxScanTokenSize)
		scanner.Buffer(buf, maxScanTokenSize)

		messageCount := 0

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				t.log.Debug("Context cancelled during scan", "error", ctx.Err())

				errs <- ctx.Err()

				return
			default:
			}

			line := scanner.Bytes()

			var msg map[string]any

			if err := json.Unmarshal(line, &msg); err != nil {
				t.log.Debug("Failed to unmarshal JSON message", "error", err, "message", string(line))

				errs <- &errors.CLIJSONDecodeError{
					RawData: string(line),
					Err:     err,
				}

				continue
			}

			messageCount++
			t.log.Debug("Received message from CLI", "message_count", messageCount)

			select {
			case messages <- msg:
			case <-ctx.Done():
				t.log.Debug("Context cancelled during message send", "error", ctx.Err())

				errs <- ctx.Err()

				return
			}
		}

		if err := scanner.Err(); err != nil {
			t.log.Error("Scanner error while reading CLI output", "error", err)

			errs <- fmt.Errorf("scanner error: %w", err)
		}

		// Wait for stderr goroutine before process wait
		stderrWg.Wait()

		// Wait for process to exit and capture any errors
		t.log.Debug("Waiting for CLI process to exit")

		if err := t.cmd.Wait(); err != nil {
			// Check if this is an intentional shutdown
			t.mu.Lock()
			isClosing := t.closing
			t.mu.Unlock()

			if isClosing {
				t.log.Debug("CLI process terminated during shutdown")

				return
			}

			// Use buffered stderr for error reporting (cleaned of Bun source context)
			stderrMu.Lock()

			stderrOutput := cleanStderr(stderrBuffer.String())

			stderrMu.Unlock()

			exitCode := 0

			if exitErr, ok := stderrors.AsType[*exec.ExitError](err); ok {
				exitCode = exitErr.ExitCode()
			}

			t.log.Error("CLI process exited with error", "exit_code", exitCode, "stderr", stderrOutput)

			errs <- &errors.ProcessError{
				ExitCode: exitCode,
				Stderr:   stderrOutput,
				Err:      err,
			}
		} else {
			t.log.Info("CLI process exited successfully")
		}
	}()

	return messages, errs
}

// SendMessage sends a JSON message to the CLI stdin.
func (t *CLITransport) SendMessage(ctx context.Context, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stdin == nil {
		return errors.ErrTransportNotConnected
	}

	if t.stdinClosed {
		return errors.ErrStdinClosed
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Ensure data ends with newline
	if len(data) == 0 || data[len(data)-1] != '\n' {
		newData := make([]byte, len(data)+1)
		copy(newData, data)
		newData[len(data)] = '\n'
		data = newData
	}

	done := make(chan error, 1)

	go func() {
		_, err := t.stdin.Write(data)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("write to stdin: %w", err)
		}

		return nil

	case <-ctx.Done():
		if t.stdin != nil {
			_ = t.stdin.Close()
			t.stdinClosed = true
		}

		select {
		case <-done:
		case <-time.After(1 * time.Second):
			t.log.Warn("write goroutine did not exit after stdin close")
		}

		return ctx.Err()
	}
}

// IsReady checks if the transport is ready for communication.
func (t *CLITransport) IsReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.cmd != nil && t.cmd.Process != nil && t.stdin != nil
}

// EndInput ends the input stream (closes stdin for process transports).
//
// This signals to the CLI that no more input will be sent. The CLI process
// will continue processing any pending input and then exit normally.
func (t *CLITransport) EndInput() error {
	return t.CloseStdin()
}

// CloseStdin closes the stdin pipe to signal end of input in streaming mode.
//
// This is used in streaming mode to indicate that no more messages will be sent.
// The CLI process will continue processing any pending input and then exit normally.
func (t *CLITransport) CloseStdin() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stdin != nil && !t.stdinClosed {
		t.log.Debug("Closing stdin pipe")

		err := t.stdin.Close()
		t.stdinClosed = true
		t.stdin = nil

		return err
	}

	return nil
}

// Close terminates the CLI process.
func (t *CLITransport) Close() error {
	var closeErr error

	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
		t.stdinClosed = true

		stdin := t.stdin
		t.stdin = nil

		cmd := t.cmd
		closeCh := t.closeCh
		t.mu.Unlock()

		if closeCh != nil {
			close(closeCh)
		}

		if stdin != nil {
			_ = stdin.Close()
		}

		if cmd != nil && cmd.Process != nil {
			t.log.Debug("Killing Codex CLI process tree", "pid", cmd.Process.Pid)
			closeErr = killProcessTree(cmd)
		}
	})

	return closeErr
}

// cleanStderr parses and cleans stderr output from the CLI.
// Bun includes minified source context in error output which is not useful.
// This extracts just the error message and stack trace.
func cleanStderr(stderr string) string {
	if stderr == "" {
		return ""
	}

	var cleaned strings.Builder

	lines := strings.SplitSeq(stderr, "\n")

	for line := range lines {
		// Skip Bun source context lines (format: "1234 | <minified code>")
		trimmed := strings.TrimSpace(line)
		if isSourceContextLine(trimmed) {
			continue
		}

		// Keep error messages, stack traces, and other useful output
		if cleaned.Len() > 0 {
			cleaned.WriteString("\n")
		}

		cleaned.WriteString(line)
	}

	return strings.TrimSpace(cleaned.String())
}

// isSourceContextLine checks if a line is Bun's source code context.
// These lines have the format: "1234 | <code>" where 1234 is a line number.
func isSourceContextLine(line string) bool {
	// Find the pipe separator
	pipeIdx := strings.Index(line, "|")
	if pipeIdx < 1 {
		return false
	}

	// Check if everything before the pipe is digits and whitespace
	prefix := strings.TrimSpace(line[:pipeIdx])
	if prefix == "" {
		return false
	}

	for _, ch := range prefix {
		if ch < '0' || ch > '9' {
			return false
		}
	}

	return true
}
