package claudesdk

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// rawMessage is a raw NDJSON line with the "type" field pre-parsed.
type rawMessage struct {
	Type string
	Data json.RawMessage
}

// parseLine extracts the "type" field from a raw NDJSON line.
func parseLine(line []byte) (*rawMessage, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, err
	}
	// Make a copy of the line data since bufio.Scanner reuses its buffer.
	data := make([]byte, len(line))
	copy(data, line)
	return &rawMessage{
		Type: probe.Type,
		Data: json.RawMessage(data),
	}, nil
}

// isControlRequest returns true if the line is a control_request from the CLI.
func isControlRequest(rm *rawMessage) bool {
	return rm.Type == "control_request"
}

// isControlResponse returns true if the line is a control_response from the CLI.
func isControlResponse(rm *rawMessage) bool {
	return rm.Type == "control_response"
}

// isMessage returns true if the line is a regular message (not control protocol).
func isMessage(rm *rawMessage) bool {
	switch rm.Type {
	case "system", "assistant", "user", "result", "stream_event":
		return true
	}
	return false
}

// controlRequest is the envelope for requests in either direction.
type controlRequest struct {
	Type      string          `json:"type"` // "control_request"
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// controlResponse is the envelope for responses in either direction.
type controlResponse struct {
	Type     string              `json:"type"` // "control_response"
	Response controlResponseBody `json:"response"`
}

// controlResponseBody holds the response data.
type controlResponseBody struct {
	Subtype   string          `json:"subtype"` // "success" or "error"
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// parseRequestSubtype extracts the subtype from a control request's request field.
func parseRequestSubtype(raw json.RawMessage) (string, error) {
	var s struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Subtype, nil
}

// controller manages control protocol request/response multiplexing.
type controller struct {
	counter atomic.Int64
	pending map[string]chan controlResponseBody
	mu      sync.Mutex
	writeFn func(any) error
}

// newController creates a controller with the given write function.
func newController(writeFn func(any) error) *controller {
	return &controller{
		pending: make(map[string]chan controlResponseBody),
		writeFn: writeFn,
	}
}

// nextRequestID generates the next unique request ID.
func (c *controller) nextRequestID() string {
	n := c.counter.Add(1)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("req_%d_%s", n, hex.EncodeToString(b))
}

// sendRequest sends a control request and waits for the response.
func (c *controller) sendRequest(subtype string, body any) (*controlResponseBody, error) {
	reqID := c.nextRequestID()

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

	ch := make(chan controlResponseBody, 1)
	c.mu.Lock()
	c.pending[reqID] = ch
	c.mu.Unlock()

	req := controlRequest{
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

// sendRequestNoWait writes a control request without blocking on the
// response. Use this in setup paths that run BEFORE Stream() begins
// reading stdout — calling sendRequest there would deadlock because
// the response can only be dispatched from inside Stream(). Ordering
// vs. subsequent writeLine() calls is preserved (the underlying
// writeFn is synchronous), so the CLI receives the request before
// any subsequent user message.
func (c *controller) sendRequestNoWait(subtype string, body any) error {
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(bodyJSON, &bodyMap); err != nil {
		bodyMap = make(map[string]any)
	}
	bodyMap["subtype"] = subtype
	finalBody, err := json.Marshal(bodyMap)
	if err != nil {
		return err
	}
	req := controlRequest{
		Type:      "control_request",
		RequestID: c.nextRequestID(),
		Request:   json.RawMessage(finalBody),
	}
	return c.writeFn(req)
}

// handleResponse routes a control response to its pending request.
func (c *controller) handleResponse(resp controlResponseBody) {
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

// sendResponse sends a control response back to the CLI.
func (c *controller) sendResponse(requestID string, subtype string, response any) error {
	var respJSON json.RawMessage
	if response != nil {
		b, err := json.Marshal(response)
		if err != nil {
			return err
		}
		respJSON = b
	}

	resp := controlResponse{
		Type: "control_response",
		Response: controlResponseBody{
			Subtype:   subtype,
			RequestID: requestID,
			Response:  respJSON,
		},
	}
	return c.writeFn(resp)
}

// sendErrorResponse sends an error control response back to the CLI.
func (c *controller) sendErrorResponse(requestID string, errMsg string) error {
	resp := controlResponse{
		Type: "control_response",
		Response: controlResponseBody{
			Subtype:   "error",
			RequestID: requestID,
			Error:     errMsg,
		},
	}
	return c.writeFn(resp)
}
