package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RegisterRunsMCPRoutes exposes runops over Streamable HTTP. The RunStore is
// fixed by the host (project-scoped filesystem locally, tenant-scoped Mongo in
// cloud); the model can select only a run id inside that store.
func RegisterRunsMCPRoutes(mux *http.ServeMux, prefix string, rs store.RunStore, reg BoardMCPTokenStore) {
	if mux == nil || rs == nil || reg == nil {
		return
	}
	h := &runsMCPHandler{store: rs, registry: reg}
	p := strings.TrimRight(prefix, "/")
	mux.HandleFunc("POST "+p, h.serve)
	mux.HandleFunc("POST "+p+"/", h.serve)
	mux.HandleFunc("GET "+p, runsMCPMethodNotAllowed)
	mux.HandleFunc("GET "+p+"/", runsMCPMethodNotAllowed)
}

func runsMCPMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed: runs MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
}

type runsMCPHandler struct {
	store    store.RunStore
	registry BoardMCPTokenStore
}

func (h *runsMCPHandler) serve(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, mcpResp{JSONRPC: "2.0", Error: &mcpRespError{Code: -32700, Message: "parse error: " + err.Error()}})
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	caps := runops.Capabilities{}
	for capability, granted := range grant.Capabilities {
		if granted {
			caps[capability] = true
		}
	}
	ctx := r.Context()
	if grant.TenantID != "" {
		ctx = store.WithTenant(ctx, grant.TenantID)
	}
	writeJSONStatus(w, http.StatusOK, dispatchRunsHTTP(ctx, req, h.store, caps))
}

func dispatchRunsHTTP(ctx context.Context, req mcpReq, rs store.RunStore, caps runops.Capabilities) mcpResp {
	resp := mcpResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "iterion-runs-http", "version": "1.0.0"},
		}
	case "tools/list":
		definitions := runops.ToolsFor(caps)
		entries := make([]map[string]any, 0, len(definitions))
		for _, definition := range definitions {
			entries = append(entries, map[string]any{
				"name": definition.Name, "description": definition.Description, "inputSchema": definition.InputSchema,
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
		raw, err := runops.Call(ctx, rs, caps, params.Name, params.Arguments)
		if err != nil {
			if errors.Is(err, runops.ErrCapabilityDenied) {
				resp.Error = &mcpRespError{Code: -32601, Message: err.Error()}
				return resp
			}
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": false}
	default:
		resp.Error = &mcpRespError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}
