package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxMessageSize = 10 * 1024 * 1024

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type listToolsResult struct {
	Tools []ToolInfo `json:"tools"`
}

type callToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

type stdioClient struct {
	cfg  *ServerConfig
	info clientInfo

	startMu  sync.Mutex
	started  bool
	startErr error

	writeMu sync.Mutex
	stateMu sync.Mutex

	closed  bool
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	pending map[int]chan rpcReply
	nextID  int
	readErr error
}

func newStdioClient(cfg *ServerConfig, info clientInfo) *stdioClient {
	return &stdioClient{cfg: cloneServerConfig(cfg), info: info}
}

func (c *stdioClient) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var result listToolsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list result: %w", err)
	}
	return result.Tools, nil
}

func (c *stdioClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*ToolCallResult, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	raw, err := c.call(ctx, "tools/call", callToolParams{Name: toolName, Arguments: args})
	if err != nil {
		return nil, err
	}
	var result ToolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	return &result, nil
}

func (c *stdioClient) Close() error {
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		return nil
	}
	c.closed = true
	cmd := c.cmd
	stdin := c.stdin
	stdout := c.stdout
	c.stateMu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	// Close stdout explicitly to unblock readLoop's scanner.Scan() call.
	// Without this, readLoop may hang indefinitely if the subprocess does
	// not close its stdout (e.g., it is stuck or buffering).
	if stdout != nil {
		_ = stdout.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		// If the process was already terminated by Close/EOF, treat it as closed.
		if strings.Contains(err.Error(), "waitid: no child processes") {
			return nil
		}
		return err
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil
	}
}

func (c *stdioClient) ensureStarted(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return c.startErr
	}
	c.startErr = c.start(ctx)
	if c.startErr == nil {
		c.started = true
	}
	return c.startErr
}

func (c *stdioClient) start(ctx context.Context) error {
	cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
	if c.cfg.WorkDir != "" {
		cmd.Dir = c.cfg.WorkDir
	}
	if len(c.cfg.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range c.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdio stdin pipe for %q: %w", c.cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdio stdout pipe for %q: %w", c.cfg.Name, err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: start stdio server %q: %w", c.cfg.Name, err)
	}

	c.stateMu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.stdout = stdout
	c.pending = make(map[int]chan rpcReply)
	c.stateMu.Unlock()

	go c.readLoop(stdout)

	raw, err := c.call(ctx, "initialize", map[string]interface{}{
		"protocolVersion": DefaultProtocolVersion,
		"capabilities": map[string]interface{}{
			"elicitation": map[string]interface{}{"supported": true},
		},
		"clientInfo": c.info,
	})
	if err != nil {
		_ = c.Close()
		return err
	}

	var init initializeResult
	if err := json.Unmarshal(raw, &init); err != nil {
		_ = c.Close()
		return fmt.Errorf("mcp: decode initialize result for %q: %w", c.cfg.Name, err)
	}

	if err := c.notify("notifications/initialized", map[string]interface{}{}); err != nil {
		_ = c.Close()
		return err
	}

	return nil
}

func (c *stdioClient) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if err := c.currentErr(); err != nil {
		return nil, err
	}

	id, ch := c.registerPending()
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	if err := c.write(req); err != nil {
		c.unregisterPending(id)
		return nil, err
	}

	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		c.unregisterPending(id)
		return nil, ctx.Err()
	}
}

func (c *stdioClient) notify(method string, params interface{}) error {
	return c.write(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

func (c *stdioClient) write(msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcp: encode request: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.stateMu.Lock()
	stdin := c.stdin
	c.stateMu.Unlock()
	if stdin == nil {
		return fmt.Errorf("mcp: stdio client is closed")
	}

	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("mcp: write request: %w", err)
	}
	return nil
}

func (c *stdioClient) registerPending() (int, chan rpcReply) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcReply, 1)
	c.pending[id] = ch
	return id, ch
}

func (c *stdioClient) unregisterPending(id int) {
	c.stateMu.Lock()
	delete(c.pending, id)
	c.stateMu.Unlock()
}

func (c *stdioClient) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024), maxMessageSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			c.failAll(fmt.Errorf("mcp: decode stdio response from %q: %w", c.cfg.Name, err))
			return
		}

		// Handle server→client requests (elicitations, approvals).
		// Auto-approve all elicitation requests so Codex MCP can proceed
		// without interactive confirmation.
		if msg.Method == "elicitation/create" {
			go c.autoApproveElicitation(msg.ID)
			continue
		}

		// Skip notifications (no ID).
		if len(msg.ID) == 0 {
			continue
		}
		id, ok := decodeIntID(msg.ID)
		if !ok {
			continue
		}

		c.stateMu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.stateMu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- rpcReply{err: fmt.Errorf("mcp: %s", msg.Error.Message)}
			continue
		}
		ch <- rpcReply{result: msg.Result}
	}

	if err := scanner.Err(); err != nil {
		c.failAll(fmt.Errorf("mcp: stdio read from %q: %w", c.cfg.Name, err))
		return
	}
	c.failAll(io.EOF)
}

// autoApproveElicitation sends an approval response for a server→client
// elicitation request (e.g. Codex patch approval). This allows the MCP
// server to proceed without interactive confirmation.
func (c *stdioClient) autoApproveElicitation(rawID json.RawMessage) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  interface{}     `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      rawID,
		Result: map[string]interface{}{
			"action": "approve",
		},
	}
	_ = c.write(resp)
}

func (c *stdioClient) failAll(err error) {
	c.stateMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	pending := c.pending
	c.pending = make(map[int]chan rpcReply)
	c.stateMu.Unlock()

	for _, ch := range pending {
		ch <- rpcReply{err: err}
	}
}

func (c *stdioClient) currentErr() error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.readErr
}

type httpClient struct {
	cfg  *ServerConfig
	info clientInfo
	http *http.Client

	startMu  sync.Mutex
	started  bool
	startErr error

	mu              sync.Mutex
	nextID          int
	sessionID       string
	protocolVersion string
}

func newHTTPClient(cfg *ServerConfig, info clientInfo) *httpClient {
	return &httpClient{
		cfg:  cloneServerConfig(cfg),
		info: info,
		http: &http.Client{
			// Safety timeout for requests without a context deadline.
			// Individual requests use context for cancellation; this is
			// a backstop to prevent indefinite blocking.
			Timeout: 60 * time.Second,
		},
		protocolVersion: DefaultProtocolVersion,
	}
}

func (c *httpClient) ListTools(ctx context.Context) ([]ToolInfo, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	raw, _, err := c.call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var result listToolsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list result: %w", err)
	}
	return result.Tools, nil
}

func (c *httpClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*ToolCallResult, error) {
	if err := c.ensureStarted(ctx); err != nil {
		return nil, err
	}
	raw, _, err := c.call(ctx, "tools/call", callToolParams{Name: toolName, Arguments: args})
	if err != nil {
		return nil, err
	}
	var result ToolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/call result: %w", err)
	}
	return &result, nil
}

func (c *httpClient) Close() error { return nil }

func (c *httpClient) ensureStarted(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return c.startErr
	}
	c.startErr = c.start(ctx)
	if c.startErr == nil {
		c.started = true
	}
	return c.startErr
}

func (c *httpClient) start(ctx context.Context) error {
	raw, sessionID, err := c.call(ctx, "initialize", map[string]interface{}{
		"protocolVersion": DefaultProtocolVersion,
		"capabilities":    map[string]interface{}{},
		"clientInfo":      c.info,
	})
	if err != nil {
		return err
	}
	var init initializeResult
	if err := json.Unmarshal(raw, &init); err != nil {
		return fmt.Errorf("mcp: decode initialize result for %q: %w", c.cfg.Name, err)
	}

	c.mu.Lock()
	if sessionID != "" {
		c.sessionID = sessionID
	}
	if strings.TrimSpace(init.ProtocolVersion) != "" {
		c.protocolVersion = strings.TrimSpace(init.ProtocolVersion)
	}
	c.mu.Unlock()

	if _, _, err := c.callNotification(ctx, "notifications/initialized", map[string]interface{}{}); err != nil {
		return err
	}
	return nil
}

func (c *httpClient) call(ctx context.Context, method string, params interface{}) (json.RawMessage, string, error) {
	id := c.nextRequestID()
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, "", fmt.Errorf("mcp: encode %s request: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("mcp: create %s request: %w", method, err)
	}
	c.applyHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: %s request to %q: %w", method, c.cfg.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("mcp: %s request to %q failed with %s: %s", method, c.cfg.Name, resp.Status, strings.TrimSpace(string(data)))
	}

	raw, err := decodeHTTPResponse(resp, id)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: %s response from %q: %w", method, c.cfg.Name, err)
	}
	return raw, resp.Header.Get("Mcp-Session-Id"), nil
}

func (c *httpClient) callNotification(ctx context.Context, method string, params interface{}) (json.RawMessage, string, error) {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, "", fmt.Errorf("mcp: encode %s notification: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("mcp: create %s notification: %w", method, err)
	}
	c.applyHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("mcp: %s notification to %q: %w", method, c.cfg.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", fmt.Errorf("mcp: %s notification to %q failed with %s: %s", method, c.cfg.Name, resp.Status, strings.TrimSpace(string(data)))
	}
	return nil, resp.Header.Get("Mcp-Session-Id"), nil
}

func (c *httpClient) nextRequestID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

func (c *httpClient) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	c.mu.Lock()
	protocolVersion := c.protocolVersion
	sessionID := c.sessionID
	c.mu.Unlock()

	if protocolVersion == "" {
		protocolVersion = DefaultProtocolVersion
	}
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
}

func decodeHTTPResponse(resp *http.Response, expectedID int) (json.RawMessage, error) {
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		return decodeSSEResponse(resp.Body, expectedID)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageSize))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	return decodeRPCPayload(data, expectedID)
}

func decodeSSEResponse(r io.Reader, expectedID int) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1024), maxMessageSize)

	var dataLines []string
	flush := func() (json.RawMessage, bool, error) {
		if len(dataLines) == 0 {
			return nil, false, nil
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = nil
		raw, err := decodeRPCPayload([]byte(payload), expectedID)
		if err != nil {
			return nil, false, err
		}
		return raw, true, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			raw, ok, err := flush()
			if err != nil {
				return nil, err
			}
			if ok {
				return raw, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	raw, ok, err := flush()
	if err != nil {
		return nil, err
	}
	if ok {
		return raw, nil
	}
	return nil, io.EOF
}

func decodeRPCPayload(data []byte, expectedID int) (json.RawMessage, error) {
	var msg rpcMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("mcp: %s", msg.Error.Message)
	}
	if len(msg.ID) > 0 {
		if id, ok := decodeIntID(msg.ID); ok && id != expectedID {
			return nil, fmt.Errorf("unexpected response id %d (want %d)", id, expectedID)
		}
	}
	return msg.Result, nil
}

func decodeIntID(raw json.RawMessage) (int, bool) {
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}
	return 0, false
}
