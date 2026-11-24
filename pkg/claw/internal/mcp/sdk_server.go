package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// MCPServerProtocolVersion is the protocol version the server advertises during initialize.
const MCPServerProtocolVersion = "2025-03-26"

// ToolCallHandler is invoked for every tools/call request.
// Returning (text, nil) yields a text content block with isError: false.
// Returning ("", err) yields the error message with isError: true.
type ToolCallHandler func(name string, arguments json.RawMessage) (string, error)

// McpServerSpec configures an McpSdkServer instance.
type McpServerSpec struct {
	// ServerName advertised in the serverInfo field of the initialize response.
	ServerName string
	// ServerVersion advertised in the serverInfo field.
	ServerVersion string
	// Tools are the tool descriptors returned for tools/list.
	Tools []sdkTool
	// ToolHandler is invoked for tools/call.
	ToolHandler ToolCallHandler
}

// sdkTool describes a tool exposed by the SDK server.
type sdkTool struct {
	Name        string           `json:"name"`
	Description *string          `json:"description,omitempty"`
	InputSchema *json.RawMessage `json:"inputSchema,omitempty"`
}

// sdkInitializeResult is the server's response to initialize.
type sdkInitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      sdkServerInfo  `json:"serverInfo"`
}

type sdkServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// sdkListToolsResult is the server's response to tools/list.
type sdkListToolsResult struct {
	Tools      []sdkTool `json:"tools"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// sdkToolCallParams are the parameters for tools/call.
type sdkToolCallParams struct {
	Name      string           `json:"name"`
	Arguments *json.RawMessage `json:"arguments,omitempty"`
}

// sdkToolCallResult is the server's response to tools/call.
type sdkToolCallResult struct {
	Content []sdkToolCallContent `json:"content"`
	IsError *bool                `json:"isError,omitempty"`
}

type sdkToolCallContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// McpSdkServer is a minimal MCP stdio server.
// It runs a blocking read/dispatch/write loop over the provided reader/writer,
// terminating cleanly when the reader is closed.
type McpSdkServer struct {
	spec McpServerSpec
}

// NewMcpSdkServer creates a new MCP SDK server with the given specification.
func NewMcpSdkServer(spec McpServerSpec) *McpSdkServer {
	return &McpSdkServer{spec: spec}
}

// Run runs the server until the reader is closed or the context is cancelled.
// Returns nil on clean EOF.
func (s *McpSdkServer) Run(ctx context.Context, reader io.Reader, writer io.Writer) error {
	br := bufio.NewReader(reader)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		payload, err := readLSPFrame(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("mcp sdk: read frame: %w", err)
		}
		if payload == nil {
			return nil // clean EOF before header
		}

		// Parse the raw JSON to check for id field.
		var raw json.RawMessage
		if err := json.Unmarshal(payload, &raw); err != nil {
			resp := errorResponse(nil, -32700, fmt.Sprintf("parse error: %s", err))
			if werr := writeLSPFrame(writer, resp); werr != nil {
				return werr
			}
			continue
		}

		// Check if this is a notification (no id).
		var envelope struct {
			ID     *json.RawMessage `json:"id"`
			Method string           `json:"method"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			resp := errorResponse(nil, -32700, fmt.Sprintf("parse error: %s", err))
			if werr := writeLSPFrame(writer, resp); werr != nil {
				return werr
			}
			continue
		}

		if envelope.ID == nil {
			// Notification — no reply.
			continue
		}

		// Parse as full request.
		var req sdkRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			resp := errorResponse(nil, -32600, fmt.Sprintf("invalid request: %s", err))
			if werr := writeLSPFrame(writer, resp); werr != nil {
				return werr
			}
			continue
		}

		result := s.dispatch(req)
		if werr := writeLSPFrame(writer, result); werr != nil {
			return werr
		}
	}
}

// Dispatch dispatches a parsed request to the appropriate handler.
// Exported for testing without I/O.
func (s *McpSdkServer) Dispatch(method string, id any, params json.RawMessage) sdkResponse {
	return s.dispatch(sdkRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  ptrRawMessage(params),
	})
}

type sdkRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      any              `json:"id"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

type sdkResponse struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Result  any          `json:"result,omitempty"`
	Error   *sdkRPCError `json:"error,omitempty"`
}

type sdkRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *McpSdkServer) dispatch(req sdkRequest) sdkResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	default:
		return sdkResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &sdkRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		}
	}
}

func (s *McpSdkServer) handleInitialize(id any) sdkResponse {
	result := sdkInitializeResult{
		ProtocolVersion: MCPServerProtocolVersion,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo: sdkServerInfo{
			Name:    s.spec.ServerName,
			Version: s.spec.ServerVersion,
		},
	}
	return sdkResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

func (s *McpSdkServer) handleToolsList(id any) sdkResponse {
	result := sdkListToolsResult{
		Tools: s.spec.Tools,
	}
	if result.Tools == nil {
		result.Tools = []sdkTool{}
	}
	return sdkResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

func (s *McpSdkServer) handleToolsCall(id any, params *json.RawMessage) sdkResponse {
	if params == nil {
		return invalidParamsResponse(id, "missing params for tools/call")
	}

	var call sdkToolCallParams
	if err := json.Unmarshal(*params, &call); err != nil {
		return invalidParamsResponse(id, fmt.Sprintf("invalid tools/call params: %s", err))
	}

	args := json.RawMessage("{}")
	if call.Arguments != nil {
		args = *call.Arguments
	}

	text, err := s.spec.ToolHandler(call.Name, args)
	isError := err != nil
	if err != nil {
		text = err.Error()
	}

	result := sdkToolCallResult{
		Content: []sdkToolCallContent{
			{Type: "text", Text: text},
		},
		IsError: &isError,
	}

	return sdkResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
}

func invalidParamsResponse(id any, message string) sdkResponse {
	return sdkResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &sdkRPCError{
			Code:    -32602,
			Message: message,
		},
	}
}

func errorResponse(id any, code int, message string) sdkResponse {
	return sdkResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &sdkRPCError{
			Code:    code,
			Message: message,
		},
	}
}

// readLSPFrame reads a single Content-Length framed payload.
// Returns (nil, nil) on clean EOF before any header.
func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	return ReadLSPFrameFrom(r)
}

// writeLSPFrame writes a JSON-RPC response with Content-Length framing.
func writeLSPFrame(w io.Writer, resp sdkResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	return WriteLSPFrameTo(w, body)
}

func ptrRawMessage(data json.RawMessage) *json.RawMessage {
	if data == nil {
		return nil
	}
	return &data
}
