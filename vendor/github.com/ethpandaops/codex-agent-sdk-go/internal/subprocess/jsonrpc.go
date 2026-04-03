package subprocess

import "encoding/json"

// JSON-RPC 2.0 message types for the Codex App Server protocol.

// RPCRequest is a JSON-RPC 2.0 request message.
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	ID      int64  `json:"id"`
	Params  any    `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response message.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCNotification is a JSON-RPC 2.0 notification (no id field).
type RPCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError contains error information from a JSON-RPC response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	return e.Message
}

// rpcMessage is used to determine the type of an incoming JSON-RPC message.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method,omitempty"`
	ID      *int64          `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// isResponse reports whether the message is a JSON-RPC response.
func (m *rpcMessage) isResponse() bool {
	return m.ID != nil && m.Method == ""
}

// isRequest reports whether the message is a JSON-RPC request (has both ID and method).
func (m *rpcMessage) isRequest() bool {
	return m.ID != nil && m.Method != ""
}

// RPCIncomingRequest is a JSON-RPC 2.0 request received from the server.
type RPCIncomingRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      int64           `json:"id"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// toIncomingRequest converts the message to an RPCIncomingRequest.
func (m *rpcMessage) toIncomingRequest() *RPCIncomingRequest {
	return &RPCIncomingRequest{
		JSONRPC: m.JSONRPC,
		Method:  m.Method,
		ID:      *m.ID,
		Params:  m.Params,
	}
}

// isNotification reports whether the message is a JSON-RPC notification.
func (m *rpcMessage) isNotification() bool {
	return m.ID == nil && m.Method != ""
}

// toResponse converts the message to an RPCResponse.
func (m *rpcMessage) toResponse() *RPCResponse {
	return &RPCResponse{
		JSONRPC: m.JSONRPC,
		ID:      *m.ID,
		Result:  m.Result,
		Error:   m.Error,
	}
}

// toNotification converts the message to an RPCNotification.
func (m *rpcMessage) toNotification() *RPCNotification {
	return &RPCNotification{
		JSONRPC: m.JSONRPC,
		Method:  m.Method,
		Params:  m.Params,
	}
}
