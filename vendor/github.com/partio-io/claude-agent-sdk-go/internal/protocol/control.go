package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// ControlRequest is the envelope for requests in either direction.
type ControlRequest struct {
	Type      string          `json:"type"` // "control_request"
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// ControlResponse is the envelope for responses in either direction.
type ControlResponse struct {
	Type     string              `json:"type"` // "control_response"
	Response ControlResponseBody `json:"response"`
}

// ControlResponseBody holds the response data.
type ControlResponseBody struct {
	Subtype   string          `json:"subtype"` // "success" or "error"
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// RequestSubtype extracts the "subtype" from a control request body.
type RequestSubtype struct {
	Subtype string `json:"subtype"`
}

// ParseRequestSubtype extracts the subtype from a control request's request field.
func ParseRequestSubtype(raw json.RawMessage) (string, error) {
	var s RequestSubtype
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Subtype, nil
}

// Controller manages control protocol request/response multiplexing.
type Controller struct {
	counter atomic.Int64
	pending map[string]chan ControlResponseBody
	mu      sync.Mutex
	writeFn func(any) error
}

// NewController creates a Controller with the given write function.
func NewController(writeFn func(any) error) *Controller {
	return &Controller{
		pending: make(map[string]chan ControlResponseBody),
		writeFn: writeFn,
	}
}

// NextRequestID generates the next unique request ID.
func (c *Controller) NextRequestID() string {
	n := c.counter.Add(1)
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read always returns len(p), nil
	return fmt.Sprintf("req_%d_%s", n, hex.EncodeToString(b))
}

// SendRequest sends a control request and waits for the response.
func (c *Controller) SendRequest(subtype string, body any) (*ControlResponseBody, error) {
	reqID := c.NextRequestID()

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// Inject subtype into body.
	var bodyMap map[string]any
	if err := json.Unmarshal(bodyJSON, &bodyMap); err != nil {
		bodyMap = make(map[string]any)
	}
	bodyMap["subtype"] = subtype
	finalBody, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, err
	}

	ch := make(chan ControlResponseBody, 1)
	c.mu.Lock()
	c.pending[reqID] = ch
	c.mu.Unlock()

	req := ControlRequest{
		Type:      "control_request",
		RequestID: reqID,
		Request:   json.RawMessage(finalBody),
	}
	if err := c.writeFn(req); err != nil {
		c.mu.Lock()
		delete(c.pending, reqID)
		c.mu.Unlock()
		return nil, err
	}

	resp := <-ch
	return &resp, nil
}

// HandleResponse routes a control response to its pending request.
func (c *Controller) HandleResponse(resp ControlResponseBody) {
	c.mu.Lock()
	ch, ok := c.pending[resp.RequestID]
	if ok {
		delete(c.pending, resp.RequestID)
	}
	c.mu.Unlock()
	if ok {
		ch <- resp
	}
}

// SendResponse sends a control response back to the CLI.
func (c *Controller) SendResponse(requestID string, subtype string, response any) error {
	var respJSON json.RawMessage
	if response != nil {
		b, err := json.Marshal(response)
		if err != nil {
			return err
		}
		respJSON = b
	}

	resp := ControlResponse{
		Type: "control_response",
		Response: ControlResponseBody{
			Subtype:   subtype,
			RequestID: requestID,
			Response:  respJSON,
		},
	}
	return c.writeFn(resp)
}

// SendErrorResponse sends an error control response back to the CLI.
func (c *Controller) SendErrorResponse(requestID string, errMsg string) error {
	resp := ControlResponse{
		Type: "control_response",
		Response: ControlResponseBody{
			Subtype:   "error",
			RequestID: requestID,
			Error:     errMsg,
		},
	}
	return c.writeFn(resp)
}
