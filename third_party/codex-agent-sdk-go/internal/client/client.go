package client

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"sync"
	"time"

	agenttracer "github.com/ethpandaops/agent-sdk-observability/tracer"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/model"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/protocol"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/subprocess"
)

const (
	defaultMessageBufferSize = 10
	interruptTimeout         = 5 * time.Second
	rewindFilesTimeout       = 10 * time.Second
	setPermissionModeTimeout = 5 * time.Second
	setModelTimeout          = 5 * time.Second
	mcpStatusTimeout         = 10 * time.Second
	listModelsTimeout        = 10 * time.Second
)

// Client implements the interactive client interface.
type Client struct {
	log        *slog.Logger
	transport  config.Transport
	controller *protocol.Controller
	session    *protocol.Session
	options    *config.Options

	messages chan message.Message

	errMu    sync.RWMutex
	fatalErr error

	eg *errgroup.Group

	// Session-level OTel trace span covering Start() → Close().
	// Nil when no tracer provider is configured.
	sessionSpan *agenttracer.Span

	mu        sync.Mutex
	done      chan struct{}
	connected bool
	closed    bool
	closeOnce sync.Once

	// Per-query OTel trace span, active from Query() to ResultMessage.
	// Protected by activeQueryMu since Query() and readLoop() are concurrent.
	activeQueryMu   sync.Mutex
	activeQuerySpan *agenttracer.Span
	activeQueryCtx  context.Context //nolint:containedctx // intentional: carries per-query span across Query()→readLoop() boundary
}

// New creates a new interactive client.
func New() *Client {
	return &Client{
		messages: make(chan message.Message, defaultMessageBufferSize),
		done:     make(chan struct{}),
	}
}

// setFatalError stores the first fatal error encountered.
func (c *Client) setFatalError(err error) {
	if err == nil {
		return
	}

	c.errMu.Lock()
	defer c.errMu.Unlock()

	if c.fatalErr == nil {
		c.fatalErr = err
	}
}

// getFatalError returns the stored fatal error, if any.
func (c *Client) getFatalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()

	return c.fatalErr
}

// isConnected returns true if the client is connected.
func (c *Client) isConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.connected
}

// startSessionSpan opens a session-level OTel trace span.
// Safe to call when no observer is configured (no-op).
func (c *Client) startSessionSpan(ctx context.Context) {
	if c.options == nil || c.options.Observer == nil {
		return
	}

	model := c.options.Model
	sessionID := c.options.Resume

	_, span := c.options.Observer.StartSessionSpan(ctx, model, sessionID)
	c.sessionSpan = span
}

// endSessionSpan ends the session-level OTel trace span.
// Safe to call multiple times or when no span is active.
func (c *Client) endSessionSpan() {
	if c.sessionSpan != nil {
		c.sessionSpan.End()
		c.sessionSpan = nil
	}
}

// traceCtx returns ctx enriched with the session span for child span propagation.
// If no session span is active, returns ctx as-is.
func (c *Client) traceCtx(ctx context.Context) context.Context {
	if c.sessionSpan == nil {
		return ctx
	}

	return trace.ContextWithSpan(ctx, c.sessionSpan.Raw())
}

// startQuerySpan creates a per-query chat span from the caller's context.
// The span is parented under whatever span the caller carries (e.g., the
// consumer's application span), NOT the session span. This makes tool spans
// within a query nest correctly under the per-query span.
func (c *Client) startQuerySpan(callerCtx context.Context) {
	if c.options == nil || c.options.Observer == nil {
		return
	}

	model := c.options.Model
	sessionID := c.options.Resume

	_, span := c.options.Observer.StartSessionSpan(callerCtx, model, sessionID)

	c.activeQueryMu.Lock()
	c.activeQuerySpan = span
	// Use a detached context so the span outlives the caller's context timeout/cancel.
	c.activeQueryCtx = trace.ContextWithSpan(context.Background(), span.Raw())
	c.activeQueryMu.Unlock()
}

// endQuerySpan ends the active per-query span. Safe to call when no span is active.
func (c *Client) endQuerySpan() {
	c.activeQueryMu.Lock()
	span := c.activeQuerySpan
	c.activeQuerySpan = nil
	c.activeQueryCtx = nil
	c.activeQueryMu.Unlock()

	if span != nil {
		span.End()
	}
}

// observeCtx returns the per-query span context if a query is active,
// otherwise falls back to the provided context (which carries the session span).
func (c *Client) observeCtx(fallback context.Context) context.Context {
	c.activeQueryMu.Lock()
	ctx := c.activeQueryCtx
	c.activeQueryMu.Unlock()

	if ctx != nil {
		return ctx
	}

	return fallback
}

// notifyQueryStart marks the start of a query for TTFT tracking.
func (c *Client) notifyQueryStart() {
	if c.options == nil {
		return
	}

	if notifier, ok := c.options.MetricsRecorder.(config.QueryLifecycleNotifier); ok {
		notifier.MarkQueryStart()
	}
}

// initializeCore performs common client initialization.
// Caller must hold c.mu lock.
func (c *Client) initializeCore(ctx context.Context, options *config.Options) error {
	if options == nil {
		options = &config.Options{}
	}

	log := options.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c.log = log.With("component", "client")

	if err := config.ConfigureToolPermissionPolicy(options); err != nil {
		return err
	}

	c.options = options

	// Start session-level OTel trace span if a tracer provider is configured.
	c.startSessionSpan(ctx)

	var transport config.Transport

	if options.Transport != nil {
		transport = options.Transport

		c.log.Debug("using injected custom transport")
	} else {
		if err := config.ValidateOptionsForBackend(options, config.QueryBackendAppServer); err != nil {
			return err
		}

		transport = subprocess.NewAppServerAdapter(c.log, options)
	}

	if err := transport.Start(ctx); err != nil {
		c.endSessionSpan()

		return fmt.Errorf("start transport: %w", err)
	}

	c.transport = transport

	c.controller = protocol.NewController(c.log, transport)
	if err := c.controller.Start(ctx); err != nil {
		c.endSessionSpan()

		_ = transport.Close()

		return fmt.Errorf("start protocol controller: %w", err)
	}

	c.session = protocol.NewSession(c.log, c.controller, options)
	c.session.RegisterMCPServers()
	c.session.RegisterDynamicTools()
	c.session.RegisterHandlers()

	// Client always initializes because app-server mode requires thread/start
	// to establish the bidirectional session. This differs from Query() which
	// conditionally initializes only when hooks/callbacks/MCP are configured.
	if err := c.session.Initialize(ctx); err != nil {
		c.endSessionSpan()

		_ = transport.Close()

		return fmt.Errorf("initialize session: %w", err)
	}

	return nil
}

// Start establishes a connection to the Codex CLI.
func (c *Client) Start(ctx context.Context, options *config.Options) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return errors.ErrClientClosed
	}

	if c.connected {
		return errors.ErrClientAlreadyConnected
	}

	if err := c.initializeCore(ctx, options); err != nil {
		return err
	}

	c.log.Info("starting transport")

	var egCtx context.Context

	c.eg, egCtx = errgroup.WithContext(context.Background())

	c.emitInitMessage()

	readCtx := c.traceCtx(egCtx)

	c.eg.Go(func() error {
		return c.readLoop(readCtx)
	})

	c.connected = true
	c.log.Info("client started successfully")

	return nil
}

// StartWithContent establishes a connection and immediately sends an initial message.
func (c *Client) StartWithContent(
	ctx context.Context,
	content message.UserMessageContent,
	options *config.Options,
) error {
	if err := c.Start(ctx, options); err != nil {
		return err
	}

	return c.Query(ctx, content)
}

// StartWithStream establishes a connection and streams initial messages.
func (c *Client) StartWithStream(
	ctx context.Context,
	messages iter.Seq[message.StreamingMessage],
	options *config.Options,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return errors.ErrClientClosed
	}

	if c.connected {
		return errors.ErrClientAlreadyConnected
	}

	if err := c.initializeCore(ctx, options); err != nil {
		return err
	}

	var egCtx context.Context

	c.eg, egCtx = errgroup.WithContext(context.Background())

	c.emitInitMessage()

	readCtx := c.traceCtx(egCtx)

	c.eg.Go(func() error {
		return c.streamMessages(egCtx, messages)
	})

	c.eg.Go(func() error {
		return c.readLoop(readCtx)
	})

	c.connected = true

	// Start per-query span for the initial stream.
	c.startQuerySpan(ctx)

	// Mark query start for TTFT tracking of the initial stream.
	c.notifyQueryStart()

	c.log.Info("client started in streaming mode")

	return nil
}

// emitInitMessage sends a synthetic init system message containing server info.
func (c *Client) emitInitMessage() {
	if c.session == nil {
		return
	}

	serverInfo := c.session.GetInitializationResult()
	if len(serverInfo) == 0 {
		return
	}

	initMsg := &message.SystemMessage{
		Type:    "system",
		Subtype: "init",
		Data:    serverInfo,
	}

	select {
	case c.messages <- initMsg:
	default:
		// Avoid blocking startup if the buffer is unexpectedly full.
	}
}

func mergeQueryContentAndOptionImages(
	content message.UserMessageContent,
	images []string,
) message.UserMessageContent {
	if len(images) == 0 {
		return content
	}

	blocks := append([]message.ContentBlock{}, content.Blocks()...)
	for _, path := range images {
		blocks = append(blocks, &message.InputLocalImageBlock{
			Type: message.BlockTypeLocalImage,
			Path: path,
		})
	}

	return message.NewUserMessageContentBlocks(blocks)
}

// streamMessages sends streaming messages to the transport.
func (c *Client) streamMessages(
	ctx context.Context,
	messages iter.Seq[message.StreamingMessage],
) (err error) {
	defer func() {
		if endErr := c.transport.EndInput(); endErr != nil {
			if err == nil {
				err = fmt.Errorf("end input: %w", endErr)
			}
		}
	}()

	for msg := range messages {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return nil
		default:
		}

		data, marshalErr := json.Marshal(msg)
		if marshalErr != nil {
			return fmt.Errorf("marshal streaming message: %w", marshalErr)
		}

		if sendErr := c.transport.SendMessage(ctx, data); sendErr != nil {
			return fmt.Errorf("send streaming message: %w", sendErr)
		}
	}

	return nil
}

// readLoop reads messages from the controller and routes them.
func (c *Client) readLoop(ctx context.Context) error {
	defer close(c.messages)

	rawMessages := c.controller.Messages()

	for {
		select {
		case msg, ok := <-rawMessages:
			if !ok {
				if err := c.controller.FatalError(); err != nil {
					c.setFatalError(err)

					return err
				}

				return nil
			}

			parsed, err := message.Parse(c.log, msg)
			if stderrors.Is(err, errors.ErrUnknownMessageType) {
				continue
			}

			if err != nil {
				c.log.Warn("failed to parse message", "error", err)
				c.setFatalError(fmt.Errorf("parse message: %w", err))

				return fmt.Errorf("parse message: %w", err)
			}

			// Use per-query span context if active, otherwise session span context.
			msgCtx := c.observeCtx(ctx)

			if c.options != nil && c.options.MetricsRecorder != nil {
				c.options.MetricsRecorder.Observe(msgCtx, parsed)
			}

			// End per-query span when a ResultMessage arrives.
			if _, isResult := parsed.(*message.ResultMessage); isResult {
				c.endQuerySpan()
			}

			select {
			case c.messages <- parsed:
			case <-c.done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}

		case <-c.controller.Done():
			if err := c.controller.FatalError(); err != nil {
				c.setFatalError(err)

				return err
			}

			return nil

		case <-c.done:
			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Query sends user content to the agent.
func (c *Client) Query(ctx context.Context, content message.UserMessageContent, sessionID ...string) error {
	if !c.isConnected() {
		return errors.ErrClientNotConnected
	}

	// End any previous per-query span and start a new one from the caller's context.
	c.endQuerySpan()
	c.startQuerySpan(ctx)

	// Mark query start for TTFT tracking.
	c.notifyQueryStart()

	sid := "default"
	if len(sessionID) > 0 && sessionID[0] != "" {
		sid = sessionID[0]
	}

	content = mergeQueryContentAndOptionImages(content, c.options.Images)
	c.log.Debug("sending query", slog.Bool("string_content", content.IsString()), slog.String("session_id", sid))

	payload := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         sid,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal query: %w", err)
	}

	return c.transport.SendMessage(ctx, data)
}

// receive waits for and returns the next message from the agent.
//
// This method blocks until a message is available, an error occurs, or the
// context is cancelled. Returns io.EOF when the session ends normally.
// This is an internal method used by ReceiveMessages and ReceiveResponse.
func (c *Client) receive(ctx context.Context) (message.Message, error) {
	if err := c.getFatalError(); err != nil {
		return nil, err
	}

	select {
	case msg, ok := <-c.messages:
		if !ok {
			if c.eg != nil {
				if err := c.eg.Wait(); err != nil {
					c.setFatalError(err)

					return nil, err
				}
			}

			return nil, io.EOF
		}

		return msg, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ReceiveMessages returns an iterator that yields messages indefinitely.
func (c *Client) ReceiveMessages(ctx context.Context) iter.Seq2[message.Message, error] {
	return func(yield func(message.Message, error) bool) {
		if !c.isConnected() {
			yield(nil, errors.ErrClientNotConnected)

			return
		}

		for {
			msg, err := c.receive(ctx)
			if err != nil {
				yield(nil, err)

				return
			}

			if !yield(msg, nil) {
				return
			}
		}
	}
}

// ReceiveResponse returns an iterator that yields messages until a ResultMessage.
func (c *Client) ReceiveResponse(ctx context.Context) iter.Seq2[message.Message, error] {
	return func(yield func(message.Message, error) bool) {
		if !c.isConnected() {
			yield(nil, errors.ErrClientNotConnected)

			return
		}

		for {
			msg, err := c.receive(ctx)
			if err != nil {
				yield(nil, fmt.Errorf("receive response: %w", err))

				return
			}

			if !yield(msg, nil) {
				return
			}

			if _, ok := msg.(*message.ResultMessage); ok {
				return
			}
		}
	}
}

// Interrupt sends an interrupt signal to stop current processing.
func (c *Client) Interrupt(ctx context.Context) error {
	if !c.isConnected() {
		return errors.ErrClientNotConnected
	}

	_, err := c.controller.SendRequest(ctx, "interrupt", nil, interruptTimeout)
	if err != nil {
		return fmt.Errorf("send interrupt signal: %w", err)
	}

	return nil
}

// RewindFiles rewinds tracked files to their state at a specific user message.
func (c *Client) RewindFiles(ctx context.Context, userMessageID string) error {
	if !c.isConnected() {
		return errors.ErrClientNotConnected
	}

	payload := map[string]any{
		"user_message_id": userMessageID,
	}

	_, err := c.controller.SendRequest(ctx, "rewind_files", payload, rewindFilesTimeout)
	if err != nil {
		return fmt.Errorf("rewind files: %w", err)
	}

	return nil
}

// SetPermissionMode changes the permission mode during conversation.
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	if !c.isConnected() {
		return errors.ErrClientNotConnected
	}

	payload := map[string]any{"mode": mode}

	_, err := c.controller.SendRequest(ctx, "set_permission_mode", payload, setPermissionModeTimeout)
	if err != nil {
		return fmt.Errorf("set permission mode to %q: %w", mode, err)
	}

	return nil
}

// SetModel changes the AI model during conversation.
func (c *Client) SetModel(ctx context.Context, model *string) error {
	if !c.isConnected() {
		return errors.ErrClientNotConnected
	}

	payload := map[string]any{"model": model}

	_, err := c.controller.SendRequest(ctx, "set_model", payload, setModelTimeout)
	if err != nil {
		return fmt.Errorf("set model: %w", err)
	}

	return nil
}

// GetMCPStatus queries the CLI for live MCP server connection status.
func (c *Client) GetMCPStatus(ctx context.Context) (*mcp.Status, error) {
	if !c.isConnected() {
		return nil, errors.ErrClientNotConnected
	}

	resp, err := c.controller.SendRequest(ctx, "mcp_status", nil, mcpStatusTimeout)
	if err != nil {
		return nil, fmt.Errorf("get mcp status: %w", err)
	}

	payload := resp.Payload()

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp status payload: %w", err)
	}

	var status mcp.Status
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("unmarshal mcp status: %w", err)
	}

	c.mu.Lock()
	session := c.session
	c.mu.Unlock()

	if session != nil {
		for _, name := range session.GetSDKMCPServerNames() {
			server, ok := session.GetSDKMCPServer(name)
			if !ok {
				continue
			}

			tools := make(map[string]mcp.Tool)

			for _, rawTool := range server.ListTools() {
				toolName, _ := rawTool["name"].(string)
				if toolName == "" {
					continue
				}

				tool := mcp.Tool{
					Name:        toolName,
					InputSchema: rawTool["inputSchema"],
				}

				if description, ok := rawTool["description"].(string); ok && description != "" {
					tool.Description = &description
				}

				if title, ok := rawTool["title"].(string); ok && title != "" {
					tool.Title = &title
				}

				if outputSchema, ok := rawTool["outputSchema"]; ok {
					tool.OutputSchema = outputSchema
				}

				if annotations, ok := rawTool["annotations"].(map[string]any); ok {
					tool.Annotations = annotations
				}

				if icons, ok := rawTool["icons"].([]any); ok {
					tool.Icons = icons
				}

				if meta, ok := rawTool["_meta"]; ok {
					tool.Meta = meta
				}

				tools[toolName] = tool
			}

			status.MCPServers = append(status.MCPServers, mcp.ServerStatus{
				Name:       name,
				Status:     "connected",
				AuthStatus: mcp.AuthStatusUnsupported,
				Tools:      tools,
			})
		}
	}

	return &status, nil
}

// ListModels queries the CLI for available models.
func (c *Client) ListModels(ctx context.Context) ([]model.Info, error) {
	resp, err := c.ListModelsResponse(ctx)
	if err != nil {
		return nil, err
	}

	return resp.Models, nil
}

// ListModelsResponse queries the CLI for the full model list payload.
func (c *Client) ListModelsResponse(ctx context.Context) (*model.ListResponse, error) {
	if !c.isConnected() {
		return nil, errors.ErrClientNotConnected
	}

	resp, err := c.controller.SendRequest(ctx, "list_models", nil, listModelsTimeout)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	payload := resp.Payload()

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal list models payload: %w", err)
	}

	var listResp model.ListResponse
	if err := json.Unmarshal(raw, &listResp); err != nil {
		return nil, fmt.Errorf("unmarshal list models: %w", err)
	}

	return &listResp, nil
}

// GetServerInfo returns server initialization info.
func (c *Client) GetServerInfo() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.session == nil {
		return nil
	}

	return c.session.GetInitializationResult()
}

// Close terminates the session and cleans up resources.
func (c *Client) Close() error {
	var closeErr error

	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		wasConnected := c.connected
		c.connected = false
		c.mu.Unlock()

		if !wasConnected {
			return
		}

		c.log.Info("closing client")

		close(c.done)

		if c.controller != nil {
			c.controller.Stop()
		}

		if c.transport != nil {
			closeErr = c.transport.Close()
		}

		if c.eg != nil {
			if err := c.eg.Wait(); err != nil && closeErr == nil {
				closeErr = err
			}
		}

		// End any active per-query span and session span after all activity has stopped.
		c.endQuerySpan()

		if fatalErr := c.getFatalError(); fatalErr != nil && c.sessionSpan != nil {
			c.sessionSpan.RecordError(fatalErr)
		}

		c.endSessionSpan()

		c.log.Info("client closed")
	})

	return closeErr
}
