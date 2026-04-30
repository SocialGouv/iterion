package claudesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"sync"
)

// Session manages a multi-turn conversation with the Claude CLI.
type Session struct {
	cfg  *config
	proc *cliProcess
	ctrl *controller

	sessionID string
	mu        sync.Mutex
	closed    bool
	started   bool

	// hookCallbacks maps callback IDs to their handlers.
	hookCallbacks map[string]HookCallback

	// mcpCleanup removes the temporary MCP config file, if any.
	mcpCleanup func()
}

// NewSession creates a new interactive session with the Claude CLI.
// The CLI subprocess is started lazily on the first Send call.
func NewSession(opts ...Option) *Session {
	cfg := applyOptions(opts)
	return &Session{
		cfg:           cfg,
		hookCallbacks: make(map[string]HookCallback),
	}
}

// ResumeSession creates a session that resumes a previous conversation.
func ResumeSession(sessionID string, opts ...Option) *Session {
	opts = append([]Option{WithResume(sessionID)}, opts...)
	return NewSession(opts...)
}

// SessionID returns the session ID assigned by the CLI.
// This is available after the first message is received.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// Send queues a user message to be sent to the CLI.
// Call Stream to receive the response messages.
func (s *Session) Send(ctx context.Context, prompt string) error {
	if prompt == "" {
		return ErrEmptyPrompt
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	s.mu.Unlock()

	if err := s.ensureStarted(ctx); err != nil {
		return err
	}

	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": prompt,
		},
	}
	return s.proc.writeLine(msg)
}

// Stream returns an iterator over messages from the CLI.
// It yields messages until a result message is received or an error occurs.
func (s *Session) Stream(ctx context.Context) iter.Seq2[Message, error] {
	return func(yield func(Message, error) bool) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			yield(nil, ErrSessionClosed)
			return
		}
		s.mu.Unlock()

		for {
			select {
			case <-ctx.Done():
				yield(nil, ctx.Err())
				return
			default:
			}

			line, err := s.proc.readLine()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if len(line) == 0 {
				continue
			}

			rm, err := parseLine(line)
			if err != nil {
				continue // skip malformed lines
			}

			// Invoke message callback before processing, if configured.
			if s.cfg.messageCallback != nil {
				s.cfg.messageCallback(rm.Type, rm.Data)
			}

			// Handle control protocol messages.
			if isControlRequest(rm) {
				s.handleControlRequest(rm.Data)
				continue
			}
			if isControlResponse(rm) {
				s.handleControlResponse(rm.Data)
				continue
			}

			if !isMessage(rm) {
				continue
			}

			msg, err := unmarshalMessage(rm.Data)
			if err != nil {
				if !yield(nil, err) {
					return
				}
				continue
			}

			// Capture session ID from system init message.
			if sys, ok := msg.(*SystemMessage); ok {
				s.mu.Lock()
				s.sessionID = sys.SessionID
				s.mu.Unlock()
			}

			if !yield(msg, nil) {
				return
			}

			// Stop after result message (turn complete).
			if _, ok := msg.(*ResultMessage); ok {
				return
			}
		}
	}
}

// Close shuts down the session and its subprocess.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.mcpCleanup != nil {
		s.mcpCleanup()
	}

	if s.proc != nil {
		return s.proc.close()
	}
	return nil
}

// ensureStarted starts the CLI subprocess if it hasn't been started yet.
func (s *Session) ensureStarted(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return nil
	}

	cliPath, err := findCLI(s.cfg.cliPath)
	if err != nil {
		return ErrCLINotFound
	}

	procCfg := configToProcess(s.cfg)
	args := buildArgs(procCfg, true)

	// Write MCP config to a temp file if needed.
	mcpCleanup, err := s.setupMcpConfig(&args)
	if err != nil {
		return err
	}
	s.mcpCleanup = mcpCleanup

	spOpts := spawnOptions{
		Cwd:            s.cfg.cwd,
		Env:            s.cfg.env,
		StderrCallback: s.cfg.stderrCallback,
		// Streaming Session: NDJSON user/control messages are written via
		// proc.writeLine, so the child stdin must be a writable pipe.
		OpenStdin: true,
	}

	proc, err := spawnProcess(ctx, cliPath, args, spOpts)
	if err != nil {
		return err
	}

	s.proc = proc
	s.ctrl = newController(proc.writeLine)
	s.started = true

	// Send initialize request with hooks if configured.
	if len(s.cfg.hooks) > 0 {
		if err := s.sendInitialize(); err != nil {
			return fmt.Errorf("claude: initialize: %w", err)
		}
	}

	return nil
}

// setupMcpConfig writes MCP server configuration to a temp file and adds the flag.
func (s *Session) setupMcpConfig(args *[]string) (func(), error) {
	if len(s.cfg.mcpServers) == 0 {
		return func() {}, nil
	}

	servers := make(map[string]any)
	for name, srv := range s.cfg.mcpServers {
		servers[name] = mcpServerJSON(srv)
	}

	configJSON, err := buildMCPConfigJSON(servers)
	if err != nil {
		return nil, fmt.Errorf("claude: mcp config: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "claude-mcp-*.json")
	if err != nil {
		return nil, fmt.Errorf("claude: mcp temp file: %w", err)
	}
	if _, err := tmpFile.Write(configJSON); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return nil, err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpFile.Name())
		return nil, fmt.Errorf("claude: mcp temp file close: %w", err)
	}

	*args = append(*args, "--mcp-config", tmpFile.Name())

	cleanup := func() {
		_ = os.Remove(tmpFile.Name())
	}
	return cleanup, nil
}

// sendInitialize sends the initialize control request with hook
// configuration. The request must be written to stdin BEFORE the
// first user message (FIFO ordering ensures the CLI registers hooks
// before processing the prompt) — but we do NOT wait for the
// response, because the response can only be dispatched from inside
// Stream() and Stream() is called AFTER ensureStarted returns.
// Calling sendRequest here would deadlock the session forever.
func (s *Session) sendInitialize() error {
	hooks := make(map[string][]hookMatcherConfig)
	for event, matchers := range s.cfg.hooks {
		var configs []hookMatcherConfig
		for i, m := range matchers {
			callbackID := fmt.Sprintf("hook_%s_%d", event, i)
			s.hookCallbacks[callbackID] = m.Handler
			configs = append(configs, hookMatcherConfig{
				Matcher:         m.Matcher,
				HookCallbackIDs: []string{callbackID},
				Timeout:         m.Timeout,
			})
		}
		hooks[string(event)] = configs
	}

	body := map[string]any{
		"hooks": hooks,
	}

	return s.ctrl.sendRequestNoWait("initialize", body)
}

// handleControlRequest dispatches incoming control requests from the CLI.
func (s *Session) handleControlRequest(data json.RawMessage) {
	var req controlRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	subtype, err := parseRequestSubtype(req.Request)
	if err != nil {
		return
	}

	switch subtype {
	case "can_use_tool":
		handleCanUseTool(s.cfg, s.ctrl, req)
	case "hook_callback":
		s.handleHookCallback(req)
	default:
		_ = s.ctrl.sendErrorResponse(req.RequestID, "unsupported: "+subtype)
	}
}

// handleControlResponse routes responses to pending requests.
func (s *Session) handleControlResponse(data json.RawMessage) {
	var resp controlResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}
	s.ctrl.handleResponse(resp.Response)
}

// handleHookCallback processes hook callbacks from the CLI.
func (s *Session) handleHookCallback(req controlRequest) {
	var body struct {
		CallbackID string         `json:"callback_id"`
		Input      map[string]any `json:"input"`
		ToolUseID  *string        `json:"tool_use_id,omitempty"`
	}
	if err := json.Unmarshal(req.Request, &body); err != nil {
		_ = s.ctrl.sendErrorResponse(req.RequestID, "parse error: "+err.Error())
		return
	}

	handler, ok := s.hookCallbacks[body.CallbackID]
	if !ok {
		_ = s.ctrl.sendErrorResponse(req.RequestID, "unknown callback: "+body.CallbackID)
		return
	}

	inputJSON, _ := json.Marshal(body.Input)
	var input HookCallbackInput
	_ = json.Unmarshal(inputJSON, &input)
	input.ToolUseID = body.ToolUseID

	output, err := handler(context.TODO(), input)
	if err != nil {
		_ = s.ctrl.sendErrorResponse(req.RequestID, err.Error())
		return
	}

	// Build hook-specific output.
	hookSpecific := make(map[string]any)
	hookSpecific["hookEventName"] = input.HookEventName
	if output.Decision != "" {
		hookSpecific["permissionDecision"] = output.Decision
	}
	if output.DecisionReason != "" {
		hookSpecific["permissionDecisionReason"] = output.DecisionReason
	}
	if output.UpdatedInput != nil {
		hookSpecific["updatedInput"] = output.UpdatedInput
	}
	if output.AdditionalContext != "" {
		hookSpecific["additionalContext"] = output.AdditionalContext
	}

	resp := map[string]any{
		"hookSpecificOutput": hookSpecific,
	}
	if output.SystemMessage != "" {
		resp["systemMessage"] = output.SystemMessage
	}
	if output.Continue != nil {
		resp["continue"] = *output.Continue
	}

	_ = s.ctrl.sendResponse(req.RequestID, "success", resp)
}
