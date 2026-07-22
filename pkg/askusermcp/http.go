package askusermcp

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/internal/httpx"
)

// DefaultPath is the URL path the engine binds the per-run ask-user
// MCP listener on (the ask-user sibling of /api/v1/mcp/board).
const DefaultPath = "/api/v1/mcp/ask-user"

// NewRunToken mints a random opaque X-Iterion-Run token (32 bytes hex)
// authorizing exactly one run's ask-user MCP listener. Unlike the
// board's per-node capability grants there is nothing to scope — the
// listener itself is per-run and dies with the sandbox — so a single
// bearer token is the whole grant.
func NewRunToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("askusermcp: mint run token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Handler returns an http.Handler serving the ask-user MCP endpoint at
// prefix, authorizing requests whose `X-Iterion-Run` header equals
// token (constant-time compare). The endpoint speaks single-response
// JSON-RPC over POST — the same Streamable-HTTP subset as the board
// MCP handler; GET answers 405 so the claude-code client falls back to
// POST-only instead of treating the endpoint as missing.
//
// An empty token would make every request fail closed with 401 — the
// caller must mint one via [NewRunToken] before starting the listener.
func Handler(prefix, token string) http.Handler {
	h := &httpHandler{token: token}
	mux := http.NewServeMux()
	p := strings.TrimRight(prefix, "/")
	mux.HandleFunc("POST "+p, h.serve)
	mux.HandleFunc("POST "+p+"/", h.serve)
	mux.HandleFunc("GET "+p, methodNotAllowed)
	mux.HandleFunc("GET "+p+"/", methodNotAllowed)
	return mux
}

// methodNotAllowed signals "no SSE stream here; use POST" per the MCP
// Streamable HTTP transport contract (a 404 would make the client
// abort the whole connection — the C082 lesson from the board path).
func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed: ask-user MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
}

type httpHandler struct {
	token string
}

type mcpReq struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type mcpResp struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *mcpRespError    `json:"error,omitempty"`
}

type mcpRespError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serve authorizes the run token and dispatches one JSON-RPC call.
func (h *httpHandler) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Iterion-Run")
	if token == "" {
		http.Error(w, "missing X-Iterion-Run header", http.StatusUnauthorized)
		return
	}
	if h.token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) != 1 {
		http.Error(w, "unknown run token", http.StatusUnauthorized)
		return
	}

	var req mcpReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB cap on JSON-RPC payloads
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteJSON(w, http.StatusBadRequest, mcpResp{
			JSONRPC: "2.0",
			Error:   &mcpRespError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	if req.ID == nil {
		// Notification — no response expected, no work to do.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, dispatch(req))
}

func dispatch(req mcpReq) mcpResp {
	resp := mcpResp{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		// serverInfo.version is REQUIRED by the MCP spec: the claude-code
		// client validates the initialize response with a Zod schema and
		// rejects the whole connection when it's missing (the C082 board
		// lesson). Always include it.
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "iterion-ask-user-http", "version": "1.0.0"},
		}
	case "tools/list":
		tools := Tools()
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
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &mcpRespError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		resp.Result = CallResult(params.Name, params.Arguments)
	default:
		resp.Error = &mcpRespError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}
