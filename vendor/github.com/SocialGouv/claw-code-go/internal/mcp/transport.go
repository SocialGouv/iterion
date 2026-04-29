package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// TransportType identifies the kind of MCP transport.
type TransportType string

const (
	TransportStdio        TransportType = "stdio"
	TransportSSE          TransportType = "sse"
	TransportHTTP         TransportType = "http"
	TransportWebSocket    TransportType = "websocket"
	TransportManagedProxy TransportType = "managed_proxy"
	TransportSDK          TransportType = "sdk"
)

// TransportConfig holds common configuration for creating transports.
type TransportConfig struct {
	Type     TransportType
	URL      string            // For SSE, HTTP, WebSocket, ManagedProxy
	Headers  map[string]string // For SSE, HTTP, WebSocket
	Command  string            // For Stdio
	Args     []string          // For Stdio
	Env      map[string]string // For Stdio
	Auth     string            // Static Bearer/Authorization header for SSE/HTTP
	AuthFunc func(ctx context.Context) (string, error) // Dynamic auth header (overrides Auth when set; for SSE/HTTP)
	ID       string            // For ManagedProxy
	Name     string            // For SDK
}

// NewTransport creates a Transport from the given configuration.
// Returns an error for unsupported or misconfigured transport types.
func NewTransport(cfg TransportConfig) (Transport, error) {
	switch cfg.Type {
	case TransportStdio:
		env := make([]string, 0, len(cfg.Env))
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		return NewStdioTransport(cfg.Command, cfg.Args, env)
	case TransportSSE, TransportHTTP:
		return NewSSETransportWithAuthFunc(cfg.URL, cfg.Auth, cfg.AuthFunc), nil
	case TransportWebSocket:
		return NewWebSocketTransport(websocketURL(cfg.URL), cfg.Headers)
	case TransportManagedProxy:
		return NewManagedProxyTransport(cfg.URL, cfg.ID)
	case TransportSDK:
		return NewSDKTransport(cfg.Name, cfg.Command, cfg.Args, cfg.Env)
	default:
		return nil, fmt.Errorf("mcp: unsupported transport type %q", cfg.Type)
	}
}

// SDKTransport wraps an MCP SDK server subprocess, communicating via
// Content-Length framed JSON-RPC over stdin/stdout.
type SDKTransport struct {
	name      string
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	process   *os.Process
	mu        sync.Mutex // protects stdin writes
	responses chan Response
	done      chan struct{}
}

// NewSDKTransport creates a transport that spawns a subprocess with an MCP SDK
// server. The command/args specify the subprocess to run; the server reads from
// the subprocess stdout and writes to its stdin.
func NewSDKTransport(name, command string, args []string, env map[string]string) (*SDKTransport, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, command, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), func() []string {
			s := make([]string, 0, len(env))
			for k, v := range env {
				s = append(s, k+"="+v)
			}
			return s
		}()...)
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp sdk %q: stdin pipe: %w", name, err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp sdk %q: stdout pipe: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("mcp sdk %q: start process: %w", name, err)
	}

	t := &SDKTransport{
		name:      name,
		cmd:       cmd,
		cancel:    cancel,
		stdin:     stdinPipe,
		stdout:    bufio.NewReader(stdoutPipe),
		process:   cmd.Process,
		responses: make(chan Response, 16),
		done:      make(chan struct{}),
	}

	// Reader goroutine: reads LSP-framed JSON-RPC responses from stdout.
	go t.readLoop()

	return t, nil
}

// readLoop reads Content-Length framed responses from the subprocess stdout
// and sends them to the responses channel. On error or EOF it closes done.
func (t *SDKTransport) readLoop() {
	defer close(t.done)
	for {
		frame, err := ReadLSPFrameFrom(t.stdout)
		if err != nil {
			return
		}
		if frame == nil {
			// Clean EOF.
			return
		}

		var resp Response
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue // skip malformed frames
		}

		select {
		case t.responses <- resp:
		case <-t.done:
			return
		}
	}
}

// Send writes a JSON-RPC request to the subprocess stdin and waits for the
// matching response from the reader goroutine. The provided context controls
// the timeout/cancellation instead of a fixed 30s deadline.
func (t *SDKTransport) Send(ctx context.Context, req Request) (Response, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("mcp sdk %q: marshal request: %w", t.name, err)
	}

	t.mu.Lock()
	err = WriteLSPFrameTo(t.stdin, data)
	t.mu.Unlock()
	if err != nil {
		return Response{}, fmt.Errorf("mcp sdk %q: write request: %w", t.name, err)
	}

	// Wait for a response whose ID matches the request ID.
	for {
		select {
		case resp, ok := <-t.responses:
			if !ok {
				return Response{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error: &RPCError{
						Code:    -32603,
						Message: fmt.Sprintf("SDK transport %q: subprocess closed", t.name),
					},
				}, nil
			}
			if !idsMatch(req.ID, resp.ID) {
				// Not our response (e.g. unsolicited notification); skip it.
				continue
			}
			return resp, nil
		case <-t.done:
			return Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &RPCError{
					Code:    -32603,
					Message: fmt.Sprintf("SDK transport %q: subprocess exited", t.name),
				},
			}, nil
		case <-ctx.Done():
			return Response{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &RPCError{
					Code:    -32603,
					Message: fmt.Sprintf("SDK transport %q: %v", t.name, ctx.Err()),
				},
			}, nil
		}
	}
}

// idsMatch reports whether two JSON-RPC IDs are equal. IDs can be int, float64,
// or string after JSON round-tripping, so we normalise numeric types before
// comparison.
func idsMatch(a, b any) bool {
	return normalizeID(a) == normalizeID(b)
}

// normalizeID converts an ID value to a comparable form. JSON numbers
// deserialised via any become float64; we keep that representation so
// int(1) and float64(1) compare equal.
func normalizeID(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float32:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
		return n.String()
	default:
		return v
	}
}

// Notify sends a JSON-RPC notification to the subprocess. Notifications do
// not expect a response.
func (t *SDKTransport) Notify(n Notification) error {
	data, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("mcp sdk %q: marshal notification: %w", t.name, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if err := WriteLSPFrameTo(t.stdin, data); err != nil {
		return fmt.Errorf("mcp sdk %q: write notification: %w", t.name, err)
	}
	return nil
}

// Close shuts down the subprocess. It closes stdin to signal EOF, waits
// briefly for the process to exit, and kills it if still running.
func (t *SDKTransport) Close() error {
	// Close stdin to signal the subprocess to exit.
	if t.stdin != nil {
		t.stdin.Close()
	}

	// Wait briefly for the reader goroutine and process to finish.
	select {
	case <-t.done:
	case <-time.After(3 * time.Second):
	}

	// Kill if still running.
	if t.process != nil {
		_ = t.process.Kill()
	}

	// Cancel the context (stops CommandContext from blocking).
	if t.cancel != nil {
		t.cancel()
	}

	// Reap the process.
	_ = t.cmd.Wait()

	return nil
}
