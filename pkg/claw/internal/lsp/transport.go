package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LspTransport manages a JSON-RPC connection to an LSP server process.
type LspTransport struct {
	command  string
	args     []string
	rootPath string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex // protects writes to stdin
	nextID int64

	pending map[int64]chan json.RawMessage
	pendMu  sync.Mutex

	done   chan struct{}
	cancel context.CancelFunc
}

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response (partial, for reading).
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError represents a JSON-RPC error object.
type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// NewLspTransport creates a new LSP transport but does not start it.
func NewLspTransport(command string, args []string, rootPath string) *LspTransport {
	return &LspTransport{
		command:  command,
		args:     args,
		rootPath: rootPath,
		pending:  make(map[int64]chan json.RawMessage),
		done:     make(chan struct{}),
	}
}

// Start starts the LSP server process, performs the initialize handshake,
// and begins reading responses.
func (t *LspTransport) Start(ctx context.Context) error {
	ctx, t.cancel = context.WithCancel(ctx)

	t.cmd = exec.CommandContext(ctx, t.command, t.args...)
	t.cmd.Dir = t.rootPath

	var err error
	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp transport: stdin pipe: %w", err)
	}

	stdoutPipe, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp transport: stdout pipe: %w", err)
	}
	t.stdout = bufio.NewReader(stdoutPipe)

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("lsp transport: start process: %w", err)
	}

	go t.readLoop()

	// Perform initialize handshake.
	initParams := map[string]interface{}{
		"processId": nil,
		"rootUri":   "file://" + t.rootPath,
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"hover":          map[string]interface{}{},
				"definition":     map[string]interface{}{},
				"references":     map[string]interface{}{},
				"completion":     map[string]interface{}{},
				"documentSymbol": map[string]interface{}{},
				"formatting":     map[string]interface{}{},
			},
		},
	}

	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()

	_, err = t.Request(initCtx, "initialize", initParams)
	if err != nil {
		t.cancel()
		return fmt.Errorf("lsp transport: initialize handshake failed: %w", err)
	}

	// Send initialized notification.
	if err := t.Notify("initialized", map[string]interface{}{}); err != nil {
		t.cancel()
		return fmt.Errorf("lsp transport: initialized notification failed: %w", err)
	}

	return nil
}

// Request sends a JSON-RPC request and waits for the response.
func (t *LspTransport) Request(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	t.pendMu.Lock()
	t.nextID++
	id := t.nextID
	ch := make(chan json.RawMessage, 1)
	t.pending[id] = ch
	t.pendMu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		t.removePending(id)
		return nil, fmt.Errorf("lsp request: marshal: %w", err)
	}

	t.mu.Lock()
	err = writeLSPFrame(t.stdin, payload)
	t.mu.Unlock()
	if err != nil {
		t.removePending(id)
		return nil, fmt.Errorf("lsp request: write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-t.done:
		t.removePending(id)
		return nil, fmt.Errorf("lsp request: transport closed")
	case <-ctx.Done():
		t.removePending(id)
		return nil, ctx.Err()
	}
}

// Notify sends a JSON-RPC notification (no ID, no response expected).
func (t *LspTransport) Notify(method string, params interface{}) error {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("lsp notify: marshal: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	return writeLSPFrame(t.stdin, payload)
}

// Shutdown gracefully shuts down the LSP server.
func (t *LspTransport) Shutdown(ctx context.Context) error {
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()

	_, err := t.Request(shutdownCtx, "shutdown", nil)
	if err != nil {
		// Best effort: try to kill the process.
		if t.cmd.Process != nil {
			t.cmd.Process.Kill()
		}
		return fmt.Errorf("lsp shutdown: %w", err)
	}

	if notifyErr := t.Notify("exit", nil); notifyErr != nil {
		if t.cmd.Process != nil {
			t.cmd.Process.Kill()
		}
		return nil
	}

	// Wait for process exit with timeout.
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- t.cmd.Wait()
	}()

	select {
	case <-waitDone:
		// Process exited.
	case <-time.After(5 * time.Second):
		if t.cmd.Process != nil {
			t.cmd.Process.Kill()
		}
	}

	if t.cancel != nil {
		t.cancel()
	}

	return nil
}

// Done returns a channel that is closed when the transport reader exits.
func (t *LspTransport) Done() <-chan struct{} {
	return t.done
}

func (t *LspTransport) removePending(id int64) {
	t.pendMu.Lock()
	delete(t.pending, id)
	t.pendMu.Unlock()
}

func (t *LspTransport) readLoop() {
	defer close(t.done)
	for {
		frame, err := readLSPFrame(t.stdout)
		if err != nil {
			return
		}
		if frame == nil {
			return // clean EOF
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal(frame, &resp); err != nil {
			continue
		}

		// Only dispatch responses (messages with an ID).
		if resp.ID == nil {
			// Server-initiated notification; ignore for now.
			continue
		}

		t.pendMu.Lock()
		ch, ok := t.pending[*resp.ID]
		if ok {
			delete(t.pending, *resp.ID)
		}
		t.pendMu.Unlock()

		if !ok {
			continue
		}

		if resp.Error != nil {
			// Marshal the error as JSON so the caller can inspect it.
			errJSON, _ := json.Marshal(resp.Error)
			ch <- errJSON
		} else {
			ch <- resp.Result
		}
	}
}

// writeLSPFrame writes a Content-Length framed message to w.
func writeLSPFrame(w io.Writer, payload []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	if f, ok := w.(interface{ Flush() error }); ok {
		f.Flush()
	}
	return err
}

// readLSPFrame reads a Content-Length framed message from r.
// Returns (nil, nil) on clean EOF before any headers have been read.
func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	firstHeader := true
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && firstHeader {
				return nil, nil // clean EOF before any headers
			}
			return nil, err
		}
		firstHeader = false
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			val := strings.TrimPrefix(line, "Content-Length: ")
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %q", val)
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
