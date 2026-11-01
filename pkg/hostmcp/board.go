package hostmcp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// BoardResolver resolves the board store one token-authenticated call
// dispatches to, from the TENANT THE GRANT IS PINNED TO — never from
// anything the caller sends. Local mode resolves the single native store
// and ignores the (empty) tenant; cloud mode resolves the tenant's
// Mongo-backed board (the server's CloudBoardFor twin on the runner, #2632).
// A nil answer means the grant names a board this process does not serve;
// the handler renders that as 404 rather than dispatching to nothing.
type BoardResolver func(tenantID string) native.BoardStore

// RegisterBoardRoutes wires the board MCP HTTP endpoints onto the given mux.
// The handler authenticates via `X-Iterion-Run` (a token the host minted at
// run/node start), gates each tool by the granted capability set, and
// dispatches into boardops against the store the grant's tenant resolves to.
// The endpoint speaks line-delimited JSON-RPC over POST — one request per
// body, one response.
func RegisterBoardRoutes(mux *http.ServeMux, prefix string, resolve BoardResolver, tokens TokenStore) {
	h := &boardHandler{resolve: resolve, tokens: tokens}
	register(mux, prefix, h.serve)
}

type boardHandler struct {
	resolve BoardResolver
	tokens  TokenStore
}

// serve authorizes a run token and dispatches one JSON-RPC board call.
//
// SECURITY INVARIANT: the board store is selected by the GRANT's tenant,
// which the host pinned at mint time from the authoritative run record —
// never from the request. This is what makes the handler safe to mount
// against a multi-tenant resolver (the cloud runner's per-tenant Mongo
// boards, #2632): a valid run token can only ever operate on its own
// tenant's board. A single-tenant deployment resolves the same store
// whatever the grant says, which degenerates to the original single-store
// behaviour.
func (h *boardHandler) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Iterion-Run")
	if token == "" {
		http.Error(w, "missing X-Iterion-Run header", http.StatusUnauthorized)
		return
	}
	grant, ok := h.tokens.Lookup(token)
	if !ok {
		http.Error(w, "unknown run token", http.StatusUnauthorized)
		return
	}
	board := h.resolve(grant.TenantID)
	if board == nil {
		http.Error(w, "board not available for this run", http.StatusNotFound)
		return
	}

	var req Request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB cap on JSON-RPC payloads
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, Response{
			JSONRPC: "2.0",
			Error:   &ResponseError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	if req.ID == nil {
		// Notification — no response expected, no work to do for now.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resp := dispatchBoard(req, board, grant.Capabilities, boardops.CallEnv{SpawnParentID: grant.SourceIssueID})
	writeJSONStatus(w, http.StatusOK, resp)
}

func dispatchBoard(req Request, board native.BoardStore, caps boardops.Capabilities, env boardops.CallEnv) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		// serverInfo.version is REQUIRED by the MCP spec (a string); the
		// claude-code client validates it with a Zod schema and rejects the
		// whole connection with a ZodError ("serverInfo.version: expected
		// string, received undefined") if it's missing — which silently
		// kept the board MCP from ever registering in sandboxed claude_code
		// (C082). Always include it.
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "iterion-board-http", "version": "1.0.0"},
		}
	case "tools/list":
		tools := boardops.ToolsFor(caps)
		entries := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			entries = append(entries, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		resp.Result = map[string]any{"tools": entries}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &ResponseError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		raw, err := boardops.CallWithEnv(board, caps, params.Name, params.Arguments, env)
		if err != nil {
			if errors.Is(err, boardops.ErrCapabilityDenied) {
				resp.Error = &ResponseError{Code: -32601, Message: err.Error()}
				return resp
			}
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(raw)}},
			"isError": false,
		}
	default:
		resp.Error = &ResponseError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}
