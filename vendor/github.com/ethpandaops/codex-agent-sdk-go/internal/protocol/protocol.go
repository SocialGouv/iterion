package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/google/uuid"
)

// Transport defines the minimal interface needed for protocol operations.
type Transport interface {
	ReadMessages(ctx context.Context) (<-chan map[string]any, <-chan error)
	SendMessage(ctx context.Context, data []byte) error
}

// Controller manages bidirectional control message communication with the CLI.
type Controller struct {
	log       *slog.Logger
	transport Transport

	pendingMu sync.RWMutex
	pending   map[string]*pendingRequest

	inFlightMu sync.RWMutex
	inFlight   map[string]*inFlightOperation

	handlersMu sync.RWMutex
	handlers   map[string]RequestHandler

	messages chan map[string]any

	errMu    sync.RWMutex
	fatalErr error

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// pendingRequest tracks an outgoing request awaiting response.
type pendingRequest struct {
	subtype  string
	response chan *ControlResponse
	timeout  time.Time
}

// inFlightOperation tracks an incoming control request being handled.
type inFlightOperation struct {
	requestID string
	subtype   string
	cancel    context.CancelFunc
	startTime time.Time
	completed bool
}

// NewController creates a new protocol controller.
func NewController(log *slog.Logger, transport Transport) *Controller {
	return &Controller{
		log:       log.With("component", "protocol"),
		transport: transport,
		pending:   make(map[string]*pendingRequest, 10),
		inFlight:  make(map[string]*inFlightOperation, 10),
		handlers:  make(map[string]RequestHandler, 10),
		messages:  make(chan map[string]any, 100),
		done:      make(chan struct{}),
	}
}

// closeDone safely closes the done channel exactly once.
func (c *Controller) closeDone() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

// SetFatalError stores a fatal error and broadcasts to all waiters.
func (c *Controller) SetFatalError(err error) {
	c.errMu.Lock()

	if c.fatalErr == nil {
		c.fatalErr = err
	}

	c.errMu.Unlock()

	c.closeDone()
}

// FatalError returns the fatal error if one occurred.
func (c *Controller) FatalError() error {
	c.errMu.RLock()
	defer c.errMu.RUnlock()

	return c.fatalErr
}

// Done returns a channel that is closed when the controller stops.
func (c *Controller) Done() <-chan struct{} {
	return c.done
}

// Start begins reading messages from the transport and routing control messages.
func (c *Controller) Start(ctx context.Context) error {
	c.log.DebugContext(ctx, "starting protocol controller")

	messages, errs := c.transport.ReadMessages(ctx)

	c.wg.Add(1)

	go c.readLoop(ctx, messages, errs)

	c.log.InfoContext(ctx, "protocol controller started")

	return nil
}

// Stop gracefully shuts down the controller.
func (c *Controller) Stop() {
	c.log.Debug("stopping protocol controller")

	c.closeDone()
	c.cancelAllInFlight()
	c.wg.Wait()

	c.log.Info("protocol controller stopped")
}

// Messages returns a channel for receiving non-control messages.
func (c *Controller) Messages() <-chan map[string]any {
	return c.messages
}

// SendRequest sends a control request and waits for the response.
func (c *Controller) SendRequest(
	ctx context.Context,
	subtype string,
	payload map[string]any,
	timeout time.Duration,
) (*ControlResponse, error) {
	requestID := c.generateRequestID()

	c.log.Debug("sending control request",
		slog.String("request_id", requestID),
		slog.String("subtype", subtype),
	)

	responseChan := make(chan *ControlResponse, 1)
	pending := &pendingRequest{
		subtype:  subtype,
		response: responseChan,
		timeout:  time.Now().Add(timeout),
	}

	c.pendingMu.Lock()
	c.pending[requestID] = pending
	c.pendingMu.Unlock()

	requestPayload := map[string]any{"subtype": subtype}
	maps.Copy(requestPayload, payload)

	req := &ControlRequest{
		Type:      "control_request",
		RequestID: requestID,
		Request:   requestPayload,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if err := c.transport.SendMessage(ctx, data); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	select {
	case resp := <-responseChan:
		if resp.IsError() {
			return nil, fmt.Errorf("request error: %s", resp.ErrorMessage())
		}

		return resp, nil

	case <-c.done:
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()

		if err := c.FatalError(); err != nil {
			return nil, fmt.Errorf("transport error: %w", err)
		}

		return nil, errors.ErrControllerStopped

	case <-time.After(timeout):
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()

		return nil, fmt.Errorf("%w after %s", errors.ErrRequestTimeout, timeout)

	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, requestID)
		c.pendingMu.Unlock()

		return nil, ctx.Err()
	}
}

// RegisterHandler registers a handler for incoming control requests.
func (c *Controller) RegisterHandler(subtype string, handler RequestHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()

	c.handlers[subtype] = handler
}

// readLoop reads messages from the transport and routes control messages.
func (c *Controller) readLoop(
	ctx context.Context,
	messages <-chan map[string]any,
	errs <-chan error,
) {
	defer c.wg.Done()
	defer close(c.messages)

	for {
		select {
		case msg, ok := <-messages:
			if !ok {
				return
			}

			c.handleMessage(ctx, msg)

		case err, ok := <-errs:
			if !ok {
				return
			}

			if err != nil {
				c.log.Debug("transport error in protocol", slog.String("error", err.Error()))
				c.SetFatalError(err)

				return
			}

		case <-c.done:
			return

		case <-ctx.Done():
			return
		}
	}
}

// handleMessage routes a message based on its type.
func (c *Controller) handleMessage(ctx context.Context, msg map[string]any) {
	msgType, _ := msg["type"].(string)

	switch msgType {
	case "control_response":
		c.handleControlResponse(msg)

	case "control_request":
		c.handleControlRequest(ctx, msg)

	case "control_cancel_request":
		c.handleCancelRequest(ctx, msg)

	default:
		select {
		case c.messages <- msg:
		case <-c.done:
		case <-ctx.Done():
		}
	}
}

// handleControlResponse routes a response to the waiting request.
func (c *Controller) handleControlResponse(msg map[string]any) {
	responseData, ok := msg["response"].(map[string]any)
	if !ok {
		c.log.Warn("control response missing 'response' field")

		return
	}

	requestID, ok := responseData["request_id"].(string)
	if !ok {
		c.log.Warn("control response missing request_id")

		return
	}

	c.pendingMu.Lock()

	pending, exists := c.pending[requestID]
	if exists {
		delete(c.pending, requestID)
	}

	c.pendingMu.Unlock()

	if !exists {
		c.log.Warn("no pending request for control response", slog.String("request_id", requestID))

		return
	}

	resp := &ControlResponse{
		Type:     "control_response",
		Response: responseData,
	}

	pending.response <- resp
}

// handleControlRequest invokes the registered handler for an incoming request.
func (c *Controller) handleControlRequest(ctx context.Context, msg map[string]any) {
	requestID, ok := msg["request_id"].(string)
	if !ok {
		c.log.Warn("control request missing request_id")

		return
	}

	requestData, ok := msg["request"].(map[string]any)
	if !ok {
		c.log.Warn("control request missing 'request' field")

		return
	}

	req := &ControlRequest{
		Type:      "control_request",
		RequestID: requestID,
		Request:   requestData,
	}

	subtype := req.Subtype()
	c.log.Debug("received control request",
		slog.String("request_id", requestID),
		slog.String("subtype", subtype),
	)

	c.handlersMu.RLock()
	handler, exists := c.handlers[subtype]
	c.handlersMu.RUnlock()

	if !exists {
		c.log.Warn("no handler for control request subtype",
			slog.String("subtype", subtype),
			slog.Any("request", req.Request),
		)
		c.sendErrorResponse(ctx, requestID, "no handler registered")

		return
	}

	opCtx, cancel := context.WithCancel(ctx)

	op := &inFlightOperation{
		requestID: requestID,
		subtype:   subtype,
		cancel:    cancel,
		startTime: time.Now(),
	}

	c.inFlightMu.Lock()
	c.inFlight[requestID] = op
	c.inFlightMu.Unlock()

	c.wg.Go(func() {
		defer func() {
			c.inFlightMu.Lock()
			defer c.inFlightMu.Unlock()

			op.completed = true

			delete(c.inFlight, requestID)

			cancel()
		}()

		payload, err := handler(opCtx, req)

		if opCtx.Err() == context.Canceled {
			c.sendErrorResponse(ctx, requestID, errors.ErrOperationCancelled.Error())

			return
		}

		if err != nil {
			c.sendErrorResponse(ctx, requestID, err.Error())

			return
		}

		c.sendSuccessResponse(ctx, requestID, payload)
	})
}

// sendSuccessResponse sends a successful control response.
func (c *Controller) sendSuccessResponse(
	ctx context.Context,
	requestID string,
	payload map[string]any,
) {
	resp := &ControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   payload,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		c.log.Error("failed to marshal control response", slog.String("error", err.Error()))

		return
	}

	if err := c.transport.SendMessage(ctx, data); err != nil {
		c.log.Error("failed to send control response", slog.String("error", err.Error()))
	}
}

// sendErrorResponse sends an error control response.
func (c *Controller) sendErrorResponse(
	ctx context.Context,
	requestID string,
	errMsg string,
) {
	resp := &ControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":    "error",
			"request_id": requestID,
			"error":      errMsg,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		c.log.Error("failed to marshal error response", slog.String("error", err.Error()))

		return
	}

	if err := c.transport.SendMessage(ctx, data); err != nil {
		if ctx.Err() != nil {
			return
		}

		c.log.Error("failed to send error response", slog.String("error", err.Error()))
	}
}

// generateRequestID creates a unique request ID using ULID.
func (c *Controller) generateRequestID() string {
	return uuid.New().String()
}

// handleCancelRequest handles control_cancel_request messages from the CLI.
func (c *Controller) handleCancelRequest(ctx context.Context, msg map[string]any) {
	requestID, ok := msg["request_id"].(string)
	if !ok {
		c.log.Warn("cancel request missing request_id")

		return
	}

	c.inFlightMu.Lock()
	op, exists := c.inFlight[requestID]

	if !exists {
		c.inFlightMu.Unlock()
		c.sendCancelAcknowledgment(ctx, requestID, false, false)

		return
	}

	alreadyCompleted := op.completed
	if !alreadyCompleted {
		op.cancel()
	}

	c.inFlightMu.Unlock()

	c.sendCancelAcknowledgment(ctx, requestID, true, alreadyCompleted)
}

// sendCancelAcknowledgment sends a response acknowledging a cancel request.
func (c *Controller) sendCancelAcknowledgment(
	ctx context.Context,
	requestID string,
	found bool,
	alreadyCompleted bool,
) {
	resp := &ControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":           "cancel_acknowledgment",
			"request_id":        requestID,
			"found":             found,
			"already_completed": alreadyCompleted,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		c.log.Error("failed to marshal cancel acknowledgment", slog.String("error", err.Error()))

		return
	}

	if err := c.transport.SendMessage(ctx, data); err != nil {
		c.log.Error("failed to send cancel acknowledgment", slog.String("error", err.Error()))
	}
}

// cancelAllInFlight cancels all in-flight operations.
func (c *Controller) cancelAllInFlight() {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()

	for _, op := range c.inFlight {
		if !op.completed {
			op.cancel()
		}
	}
}
