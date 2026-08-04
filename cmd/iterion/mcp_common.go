package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// Shared JSON-RPC 2.0 message types for the internal MCP stdio servers
// (__mcp-ask-user, __mcp-board, __mcp-control). All three speak the same
// line-delimited JSON-RPC wire format, so they share these structs.

type mcpRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpError        `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpInitializeResult builds the "initialize" response result shared by
// every internal MCP stdio server; only the advertised server name varies.
func mcpInitializeResult(serverName string) any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": cli.Version(),
		},
	}
}

// mcpMethodNotFoundError builds the -32601 error for an unhandled method.
func mcpMethodNotFoundError(method string) *mcpError {
	return &mcpError{Code: -32601, Message: fmt.Sprintf("method not found: %s", method)}
}

// mcpInvalidParamsError builds the -32602 error for a params-unmarshal failure.
func mcpInvalidParamsError(err error) *mcpError {
	return &mcpError{Code: -32602, Message: fmt.Sprintf("invalid params: %s", err)}
}

// runMCPLoop drives a line-delimited JSON-RPC server over the given streams.
// For each non-empty input line it:
//   - replies with a -32700 parse error (id=null) on json.Unmarshal failure;
//   - drops notifications (req.ID == nil) silently;
//   - otherwise invokes dispatch(req) and writes the returned mcpResponse.
//
// Returns nil on clean EOF. bufMax sizes the scanner's read buffer (MCP
// messages can exceed bufio's 64KB default; callers pick the cap that fits
// their largest expected payload).
func runMCPLoop(in io.Reader, out io.Writer, bufMax int, dispatch func(req mcpRequest) mcpResponse) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), bufMax)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(mcpResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &mcpError{Code: -32700, Message: fmt.Sprintf("parse error: %s", err)},
			})
			continue
		}
		if req.ID == nil {
			continue // notification — no response
		}
		resp := dispatch(req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		// A single line beyond bufMax poisons the scanner (it cannot
		// resync mid-line), so the server has to exit — but the client
		// deserves to know WHY instead of watching the process vanish.
		if errors.Is(err, bufio.ErrTooLong) {
			_ = enc.Encode(mcpResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &mcpError{Code: -32700, Message: fmt.Sprintf("request line exceeds the %d-byte limit; the server cannot recover and will exit", bufMax)},
			})
		}
		return err
	}
	return nil
}
