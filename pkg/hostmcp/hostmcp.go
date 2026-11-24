// Package hostmcp serves the host-owned MCP HTTP transports a SANDBOXED
// run reaches over the per-run gateway listener (C082): the board tools
// (/api/v1/mcp/board) and the read-only run tools (/api/v1/mcp/runs).
// The runtime starts the listener beside the sandbox (docker:
// host.docker.internal; kubernetes: the runner pod IP) serving the mux a
// host wires with RegisterBoardRoutes / RegisterRunsRoutes; every route
// is gated by a per-node run token ([TokenStore]) and each call dispatches
// under exactly the grant that token was minted with.
//
// The package is the neutral home both serving processes import: the
// studio/server (single native store, or the cross-replica Valkey token
// store when the endpoint rides the load-balanced mux) and the cloud
// runner pod (#2632), where the board is the tenant's Mongo board resolved
// PER CALL from the tenant the grant is pinned to.
package hostmcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/internal/httpx"
)

// Request is one line-delimited JSON-RPC request the MCP Streamable HTTP
// transport carries: one POST per body, one response.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Response is the JSON-RPC answer to a Request. A nil ID marks a
// notification; the handlers answer those with 204 and no body.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *ResponseError   `json:"error,omitempty"`
}

// ResponseError is the JSON-RPC error object.
type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	httpx.WriteErrorJSON(w, status, body)
}

// MethodNotAllowed signals "no SSE stream here; use POST" per the MCP
// Streamable HTTP transport contract: clients (e.g. claude_code) probe the
// optional server→client SSE stream with a GET, and the spec requires 405 —
// NOT a 404, which a client reads as "endpoint doesn't exist" and aborts the
// whole connection (this is what kept iterion_board from registering in
// sandboxed claude_code, C082).
func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed: MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
}

// register mounts the POST + GET pair for one token-gated MCP endpoint.
func register(mux *http.ServeMux, prefix string, serve http.HandlerFunc) {
	p := strings.TrimRight(prefix, "/")
	mux.HandleFunc("POST "+p, serve)
	mux.HandleFunc("POST "+p+"/", serve)
	mux.HandleFunc("GET "+p, MethodNotAllowed)
	mux.HandleFunc("GET "+p+"/", MethodNotAllowed)
}
