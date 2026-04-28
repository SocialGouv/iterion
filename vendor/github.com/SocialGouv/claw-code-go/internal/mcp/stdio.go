package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
)

// StdioTransport communicates with an MCP server over stdin/stdout.
// It spawns the server as a child process and uses newline-delimited JSON.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
}

// NewStdioTransport spawns the MCP server process and returns a connected transport.
// env is a slice of "KEY=VALUE" strings appended to the current environment.
func NewStdioTransport(command string, args []string, env []string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp stdio: start process: %w", err)
	}

	t := &StdioTransport{
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: bufio.NewScanner(stdoutPipe),
	}

	// Drain stderr to a debug logger so it doesn't block the process.
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			slog.Debug("MCP server stderr", "command", command, "line", scanner.Text())
		}
	}()

	return t, nil
}

// Send writes a JSON-RPC request to the server's stdin and reads the response from stdout.
func (t *StdioTransport) Send(_ context.Context, req Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("mcp stdio: marshal request: %w", err)
	}

	if _, err := fmt.Fprintf(t.stdin, "%s\n", data); err != nil {
		return Response{}, fmt.Errorf("mcp stdio: write request: %w", err)
	}

	if !t.stdout.Scan() {
		if err := t.stdout.Err(); err != nil {
			return Response{}, fmt.Errorf("mcp stdio: read response: %w", err)
		}
		return Response{}, fmt.Errorf("mcp stdio: server closed connection")
	}

	var resp Response
	if err := json.Unmarshal(t.stdout.Bytes(), &resp); err != nil {
		return Response{}, fmt.Errorf("mcp stdio: unmarshal response: %w", err)
	}

	return resp, nil
}

// Notify sends a JSON-RPC notification (no response expected).
func (t *StdioTransport) Notify(n Notification) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("mcp stdio: marshal notification: %w", err)
	}

	if _, err := fmt.Fprintf(t.stdin, "%s\n", data); err != nil {
		return fmt.Errorf("mcp stdio: write notification: %w", err)
	}

	return nil
}

// Close shuts down the child process.
func (t *StdioTransport) Close() error {
	t.stdin.Close()
	return t.cmd.Wait()
}
