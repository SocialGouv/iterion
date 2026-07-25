package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// boardMCPDefaultTTL caps how long a board MCP run token stays alive
// without an explicit Revoke. A run that crashes mid-flight would
// otherwise leak its token until process exit. The window is long
// enough to cover any realistic single-run duration (sandboxed agent
// runs are typically minutes to single-digit hours).
const boardMCPDefaultTTL = 24 * time.Hour

// boardMCPMaxTokens bounds the registry so a misbehaving caller that
// Registers without Revoking can't grow it unbounded.
const boardMCPMaxTokens = 1024

// BoardMCPTokenRegistry is the ephemeral token store used to authorize
// sandbox-running bots that talk to the host's board MCP HTTP endpoint.
// The runtime MUST call Register at the start of every run and Revoke
// at the end. Even with a missed Revoke, entries are reaped after
// boardMCPDefaultTTL and the registry is capped at boardMCPMaxTokens.
type BoardMCPTokenRegistry struct {
	mu     sync.RWMutex
	tokens map[string]boardMCPGrant
	now    func() time.Time // injectable for tests
}

type boardMCPGrant struct {
	Capabilities boardops.Capabilities
	// SourceIssueID is the ticket that owns the granted run, carried so
	// board.create over this transport auto-stamps parent_id /
	// spawned_from the same way the stdio and in-process transports do.
	// Empty for runs with no owning ticket (plain studio/CLI launches).
	SourceIssueID string
	ExpiresAt     time.Time
}

// BoardMCPTokenStore is the per-run board-MCP token registry. The in-memory
// *BoardMCPTokenRegistry is single-replica; the Valkey impl
// (valkey_stores.go) shares tokens across replicas, since the sandboxed bot's
// HTTP call to /api/v1/mcp/board is load-balanced to any server pod while the
// token was registered by the runtime elsewhere. Register/Revoke are called
// by the runtime; lookup by the HTTP handler.
type BoardMCPTokenStore interface {
	Register(token string, caps []string, sourceIssueID string)
	Revoke(token string)
	lookup(token string) (boardMCPGrant, bool)
}

// NewBoardMCPTokenRegistry returns an empty registry.
func NewBoardMCPTokenRegistry() *BoardMCPTokenRegistry {
	return &BoardMCPTokenRegistry{
		tokens: map[string]boardMCPGrant{},
		now:    time.Now,
	}
}

// newBoardMCPToken mints a random opaque X-Iterion-Run token (32 bytes
// hex) for a per-node board MCP grant. Returns "" on the (effectively
// impossible) crypto/rand failure so callers degrade to board-disabled
// rather than panicking.
func newBoardMCPToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// Register stores a token with its grant. A subsequent call with the same
// token replaces the grant.
func (r *BoardMCPTokenRegistry) Register(token string, caps []string, sourceIssueID string) {
	grant := boardMCPGrant{
		Capabilities:  boardops.Capabilities{},
		SourceIssueID: strings.TrimSpace(sourceIssueID),
		ExpiresAt:     r.now().Add(boardMCPDefaultTTL),
	}
	for _, c := range caps {
		grant.Capabilities[c] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Sweep expired entries on every write so a long-running daemon
	// doesn't accumulate dead tokens between explicit Revokes.
	r.sweepLocked()
	if len(r.tokens) >= boardMCPMaxTokens {
		// Hard cap: refuse to grow the registry beyond the limit.
		// Callers see this as a silent no-op on Register; the
		// subsequent CallTool from the affected run will 401 since
		// the lookup misses. Logging is the caller's responsibility.
		return
	}
	r.tokens[token] = grant
}

// Revoke removes the token. A revoked token's subsequent calls fail with 401.
func (r *BoardMCPTokenRegistry) Revoke(token string) {
	r.mu.Lock()
	delete(r.tokens, token)
	r.mu.Unlock()
}

func (r *BoardMCPTokenRegistry) lookup(token string) (boardMCPGrant, bool) {
	r.mu.RLock()
	g, ok := r.tokens[token]
	r.mu.RUnlock()
	if !ok {
		return boardMCPGrant{}, false
	}
	if !g.ExpiresAt.IsZero() && r.now().After(g.ExpiresAt) {
		// TTL elapsed — drop lazily on first failed lookup.
		r.mu.Lock()
		// Re-check under write lock to avoid evicting a freshly-renewed
		// token that another goroutine just Registered.
		if cur, stillThere := r.tokens[token]; stillThere && !cur.ExpiresAt.IsZero() && r.now().After(cur.ExpiresAt) {
			delete(r.tokens, token)
		}
		r.mu.Unlock()
		return boardMCPGrant{}, false
	}
	return g, true
}

// sweepLocked removes every expired entry. Caller must hold r.mu.
func (r *BoardMCPTokenRegistry) sweepLocked() {
	now := r.now()
	for k, v := range r.tokens {
		if !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt) {
			delete(r.tokens, k)
		}
	}
}

// RegisterBoardMCPRoutes wires the board MCP HTTP endpoints onto the given
// mux. The handler authenticates via `X-Iterion-Run` (token issued by the
// runtime at run-start), gates each tool by the granted capability set,
// and dispatches into boardops. The endpoint speaks line-delimited JSON-RPC
// over POST — one request per body, one response.
func RegisterBoardMCPRoutes(mux *http.ServeMux, prefix string, store *native.Store, reg BoardMCPTokenStore) {
	h := &boardMCPHandler{store: store, registry: reg}
	p := strings.TrimRight(prefix, "/")
	mux.HandleFunc("POST "+p, h.serve)
	mux.HandleFunc("POST "+p+"/", h.serve)
	// MCP Streamable HTTP clients (e.g. claude_code) open an optional
	// server→client SSE stream with a GET to the same endpoint. We don't
	// offer SSE (single-response JSON over POST is a valid Streamable HTTP
	// mode), but the spec requires 405 Method Not Allowed for the GET so
	// the client falls back to POST-only — NOT a 404, which a client reads
	// as "endpoint doesn't exist" and aborts the whole connection (this is
	// what kept iterion_board from registering in sandboxed claude_code).
	mux.HandleFunc("GET "+p, boardMCPMethodNotAllowed)
	mux.HandleFunc("GET "+p+"/", boardMCPMethodNotAllowed)
}

// boardMCPMethodNotAllowed signals "no SSE stream here; use POST" per the
// MCP Streamable HTTP transport contract.
func boardMCPMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed: board MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
}

type boardMCPHandler struct {
	store    *native.Store
	registry BoardMCPTokenStore
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

// serve authorizes a run token and dispatches one JSON-RPC board call.
//
// SECURITY INVARIANT: a grant carries capabilities + expiry only, and every
// call dispatches into the SAME single h.store. This is safe ONLY because that
// store is single-tenant (the local/desktop native tracker); cloud multi-tenant
// boards go through the separate per-tenant CloudBoardFor path, not this
// handler. If this handler is ever wired to a shared multi-tenant native store
// (the pending C082 HTTP board-path work), the grant MUST also carry the run's
// tenant/board id and dispatchHTTP MUST select the store by it — otherwise any
// valid run token could operate on another tenant's board by id.
func (h *boardMCPHandler) serve(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Iterion-Run")
	if token == "" {
		http.Error(w, "missing X-Iterion-Run header", http.StatusUnauthorized)
		return
	}
	grant, ok := h.registry.lookup(token)
	if !ok {
		http.Error(w, "unknown run token", http.StatusUnauthorized)
		return
	}

	var req mcpReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB cap on JSON-RPC payloads
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, mcpResp{
			JSONRPC: "2.0",
			Error:   &mcpRespError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	if req.ID == nil {
		// Notification — no response expected, no work to do for now.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resp := dispatchHTTP(req, h.store, grant.Capabilities, boardops.CallEnv{SpawnParentID: grant.SourceIssueID})
	writeJSONStatus(w, http.StatusOK, resp)
}

func dispatchHTTP(req mcpReq, store *native.Store, caps boardops.Capabilities, env boardops.CallEnv) mcpResp {
	resp := mcpResp{JSONRPC: "2.0", ID: req.ID}

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
			resp.Error = &mcpRespError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		raw, err := boardops.CallWithEnv(store, caps, params.Name, params.Arguments, env)
		if err != nil {
			if errors.Is(err, boardops.ErrCapabilityDenied) {
				resp.Error = &mcpRespError{Code: -32601, Message: err.Error()}
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
		resp.Error = &mcpRespError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
