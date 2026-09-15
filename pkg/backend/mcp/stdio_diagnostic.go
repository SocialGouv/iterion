package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const startupStderrLimit = 8192

// startupStderr drains all writes but retains at most a small tail in memory.
// Neither the tail nor arbitrary protocol errors may enter diagnostics: an
// MCP process can print credentials unknown to the run's secret guard.
type startupStderr struct {
	mu       sync.Mutex
	tail     [startupStderrLimit]byte
	size     int
	total    int64
	finished bool
}

func (s *startupStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if s.finished {
		return n, nil
	}
	s.total += int64(n)
	if n >= len(s.tail) {
		copy(s.tail[:], p[n-len(s.tail):])
		s.size = len(s.tail)
	} else {
		if excess := s.size + n - len(s.tail); excess > 0 {
			copy(s.tail[:], s.tail[excess:s.size])
			s.size -= excess
		}
		copy(s.tail[s.size:], p)
		s.size += n
	}
	return n, nil
}

type stderrSummary struct {
	bytes     int64
	truncated bool
	hint      string
}

// finish also clears retained bytes and turns subsequent writes into a sink.
func (s *startupStderr) finish() stderrSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	summary := stderrSummary{
		bytes:     s.total,
		truncated: s.total > int64(len(s.tail)),
		hint:      startupStderrHint(s.tail[:s.size]),
	}
	clear(s.tail[:])
	s.size = 0
	s.finished = true
	return summary
}

// Hints are a fixed vocabulary, not a diagnosis. In particular, an EOF alone
// does not mean a missing token. No substring from stderr is ever returned.
func startupStderrHint(tail []byte) string {
	if len(tail) == 0 {
		return "none"
	}
	text := strings.ToLower(string(tail))
	for _, group := range []struct {
		hint    string
		markers []string
	}{
		{"authentication", []string{"unauthorized", "authentication failed", "invalid access token", "missing access token", "access token is required"}},
		{"dependency", []string{"cannot find module", "err_module_not_found", "ebadengine", "unsupported engine", "command not found"}},
		{"network", []string{"enotfound", "econnrefused", "econnreset", "etimedout", "unable to verify the first certificate"}},
		{"configuration", []string{"unknown option", "missing required argument"}},
	} {
		for _, marker := range group.markers {
			if strings.Contains(text, marker) {
				return group.hint
			}
		}
	}
	return "unclassified"
}

// diagnosticCommandTransport uses the SDK's public Command.Stderr hook and
// returns its connection unchanged (including its protocol capabilities).
// It adds no goroutine and delegates process shutdown/reaping to the SDK.
type diagnosticCommandTransport struct {
	*mcp.CommandTransport
	stderr *startupStderr
	conn   mcp.Connection
}

func newDiagnosticCommandTransport(cmd *exec.Cmd) *diagnosticCommandTransport {
	stderr := &startupStderr{}
	cmd.Stderr = stderr
	// exec's stderr copier must finish even if a descendant inherits the pipe
	// after the MCP process exits. Without WaitDelay, SDK Close could hang.
	// Keep this below the SDK's termination grace: otherwise its timeout can
	// race Wait's pipe cleanup and lose an already-exited process's status.
	cmd.WaitDelay = 500 * time.Millisecond
	return &diagnosticCommandTransport{
		CommandTransport: &mcp.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second},
		stderr:           stderr,
	}
}

func (t *diagnosticCommandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.CommandTransport.Connect(ctx)
	t.conn = conn
	return conn, err
}

func (t *diagnosticCommandTransport) startupError(cause error) error {
	exit := "unavailable"
	if t.conn != nil {
		// Close is concurrent/idempotent in the SDK. Wait for its process
		// cleanup even if Connect returned an error without closing a session.
		// Read exit status from Close's result, never race on Cmd.ProcessState
		// while a timed-out SDK shutdown may still be waiting for the process.
		closeErr := t.conn.Close()
		var exited *exec.ExitError
		switch {
		case closeErr == nil, errors.Is(closeErr, exec.ErrWaitDelay):
			exit = "0"
		case errors.As(closeErr, &exited):
			exit = fmt.Sprint(exited.ExitCode())
		}
	}
	return &stdioStartupError{cause: cause, reason: startupFailureReason(cause), exit: exit, stderr: t.stderr.finish()}
}

type stdioStartupError struct {
	cause  error
	reason string
	exit   string
	stderr stderrSummary
}

func (e *stdioStartupError) Error() string {
	return fmt.Sprintf("stdio initialization failed (reason=%s, exit_code=%s, stderr_bytes=%d, stderr_truncated=%t, stderr_hint=%s; raw diagnostics withheld)",
		e.reason, e.exit, e.stderr.bytes, e.stderr.truncated, e.stderr.hint)
}

// Preserve cancellation/retry and sentinel checks without exposing the raw
// error through Unwrap or formatting it into operator/model-visible output.
func (e *stdioStartupError) Is(target error) bool { return errors.Is(e.cause, target) }

func startupFailureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return "command_not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, mcp.ErrConnectionClosed):
		return "connection_closed"
	default:
		return "protocol_or_startup_failure"
	}
}
