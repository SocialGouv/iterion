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
	"sync"
	"sync/atomic"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/cli"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
)

const appServerScannerBuffer = 1 << 20 // 1 MB

// AppServerTransport manages a codex app-server subprocess communicating via
// JSON-RPC 2.0 over stdin/stdout.
type AppServerTransport struct {
	log  *slog.Logger
	opts *config.Options

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu        sync.Mutex
	nextID    int64
	ready     bool
	closing   bool
	pending   map[int64]chan *RPCResponse
	notifyCh  chan *RPCNotification
	requestCh chan *RPCIncomingRequest
	readDone  chan struct{}
	closeOnce sync.Once
	closeCh   chan struct{}
}

// NewAppServerTransport creates a new app server transport.
func NewAppServerTransport(log *slog.Logger, opts *config.Options) *AppServerTransport {
	return &AppServerTransport{
		log:       log.With(slog.String("component", "appserver")),
		opts:      opts,
		pending:   make(map[int64]chan *RPCResponse, 4),
		notifyCh:  make(chan *RPCNotification, 128),
		requestCh: make(chan *RPCIncomingRequest, 32),
		readDone:  make(chan struct{}),
		closeCh:   make(chan struct{}),
	}
}

// Start discovers the Codex CLI binary, spawns the app-server subprocess,
// and performs the initialize handshake.
func (t *AppServerTransport) Start(ctx context.Context) error {
	discoverer := cli.NewDiscoverer(&cli.Config{
		CliPath:          t.opts.CliPath,
		SkipVersionCheck: t.opts.SkipVersionCheck,
		Logger:           t.log,
	})

	path, err := discoverer.Discover(ctx)
	if err != nil {
		return err
	}

	args := cli.BuildAppServerArgs(t.opts)

	t.log.InfoContext(ctx, "starting codex app-server",
		slog.String("path", path),
		slog.Any("args", args),
	)

	t.cmd = exec.CommandContext(ctx, path, args...)
	t.cmd.Env = cli.BuildEnvironment(t.opts)
	configureProcessGroup(t.cmd)

	if t.opts.Cwd != "" {
		t.cmd.Dir = t.opts.Cwd
	}

	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating stdin pipe: %w", err)
	}

	stdoutPipe, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}

	t.stdout = bufio.NewScanner(stdoutPipe)
	t.stdout.Buffer(
		make([]byte, 0, appServerScannerBuffer),
		appServerScannerBuffer,
	)

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("starting codex app-server: %w", err)
	}

	t.log.InfoContext(ctx, "codex app-server started",
		slog.Int("pid", t.cmd.Process.Pid),
	)
	t.watchContextCancellation(ctx)

	go t.readLoop()

	if err := t.initialize(ctx); err != nil {
		_ = t.Close()

		return fmt.Errorf("initialize handshake: %w", err)
	}

	t.mu.Lock()
	t.ready = true
	t.mu.Unlock()

	return nil
}

// initialize sends the initialize request and initialized notification.
func (t *AppServerTransport) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2025-01-01",
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
		"clientInfo": map[string]string{
			"name":    "codex-agent-sdk-go",
			"version": "0.1.0",
		},
	}

	_, err := t.SendRequest(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("sending initialize request: %w", err)
	}

	if err := t.sendNotification("initialized", nil); err != nil {
		return fmt.Errorf("sending initialized notification: %w", err)
	}

	return nil
}

// SendRequest sends a JSON-RPC request and waits for the matching response.
func (t *AppServerTransport) SendRequest(
	ctx context.Context,
	method string,
	params any,
) (*RPCResponse, error) {
	id := atomic.AddInt64(&t.nextID, 1)
	ch := make(chan *RPCResponse, 1)

	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()

		return nil, fmt.Errorf("transport is closing")
	}

	t.pending[id] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()

	req := &RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		ID:      id,
		Params:  params,
	}

	if err := t.writeMessage(req); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}

		return resp, nil
	case <-t.readDone:
		return nil, fmt.Errorf("transport closed while waiting for response")
	}
}

// sendNotification sends a JSON-RPC notification (no response expected).
func (t *AppServerTransport) sendNotification(method string, params any) error {
	var rawParams json.RawMessage

	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshaling params: %w", err)
		}

		rawParams = data
	}

	notif := &RPCNotification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}

	return t.writeMessage(notif)
}

// Notifications returns the channel of incoming notifications.
func (t *AppServerTransport) Notifications() <-chan *RPCNotification {
	return t.notifyCh
}

// Requests returns the channel of incoming server-to-client requests.
func (t *AppServerTransport) Requests() <-chan *RPCIncomingRequest {
	return t.requestCh
}

// SendResponse sends a JSON-RPC response back to the server for a given request ID.
func (t *AppServerTransport) SendResponse(
	id int64,
	result json.RawMessage,
	rpcErr *RPCError,
) error {
	resp := &RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
		Error:   rpcErr,
	}

	return t.writeMessage(resp)
}

// IsReady reports whether the transport has completed the initialize handshake.
func (t *AppServerTransport) IsReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.ready
}

func (t *AppServerTransport) watchContextCancellation(ctx context.Context) {
	if ctx == nil || t.closeCh == nil {
		return
	}

	go func() {
		select {
		case <-ctx.Done():
			t.log.Debug("context cancelled, terminating codex app-server process tree",
				slog.Any("error", ctx.Err()),
			)

			if err := t.Close(); err != nil && !stderrors.Is(err, os.ErrProcessDone) {
				t.log.Debug("failed to terminate codex app-server process tree after context cancellation",
					slog.Any("error", err),
				)
			}
		case <-t.closeCh:
		}
	}()
}

// Close terminates the app server subprocess.
func (t *AppServerTransport) Close() error {
	var closeErr error

	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closing = true
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
			t.log.Debug("killing codex app-server process tree",
				slog.Int("pid", cmd.Process.Pid),
			)

			closeErr = killProcessTree(cmd)
			_ = cmd.Wait()
		}
	})

	return closeErr
}

// writeMessage marshals and writes a JSON-RPC message to stdin.
func (t *AppServerTransport) writeMessage(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closing {
		return fmt.Errorf("transport is closing")
	}

	data = append(data, '\n')

	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("writing to stdin: %w", err)
	}

	return nil
}

// readLoop reads JSON-RPC messages from stdout and dispatches them.
func (t *AppServerTransport) readLoop() {
	defer close(t.readDone)
	defer close(t.notifyCh)
	defer close(t.requestCh)

	for t.stdout.Scan() {
		line := t.stdout.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			t.log.Warn("skipping malformed JSON-RPC message",
				slog.String("error", err.Error()),
			)

			continue
		}

		if msg.isResponse() {
			t.mu.Lock()
			ch, ok := t.pending[*msg.ID]
			t.mu.Unlock()

			if ok {
				ch <- msg.toResponse()
			} else {
				t.log.Warn("received response for unknown request",
					slog.Int64("id", *msg.ID),
				)
			}
		} else if msg.isRequest() {
			select {
			case t.requestCh <- msg.toIncomingRequest():
			default:
				t.log.Warn("request channel full, dropping",
					slog.String("method", msg.Method),
					slog.Int64("id", *msg.ID),
				)
			}
		} else if msg.isNotification() {
			select {
			case t.notifyCh <- msg.toNotification(line):
			default:
				t.log.Warn("notification channel full, dropping",
					slog.String("method", msg.Method),
				)
			}
		}
	}
}
