package hostmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

// RegisterRunsRoutes exposes runops over Streamable HTTP. The RunStore is
// fixed by the host (project-scoped filesystem locally, tenant-scoped Mongo
// in cloud); the model can select only a run id inside that store. The
// grant's tenant scopes every call (store.WithTenant), so a token minted
// for one tenant reads no other tenant's runs.
func RegisterRunsRoutes(mux *http.ServeMux, prefix string, rs store.RunStore, tokens TokenStore) {
	if mux == nil || rs == nil || tokens == nil {
		return
	}
	h := &runsHandler{store: rs, tokens: tokens}
	register(mux, prefix, h.serve)
}

type runsHandler struct {
	store  store.RunStore
	tokens TokenStore
}

func (h *runsHandler) serve(w http.ResponseWriter, r *http.Request) {
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
	var req Request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, Response{JSONRPC: "2.0", Error: &ResponseError{Code: -32700, Message: "parse error: " + err.Error()}})
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
	writeJSONStatus(w, http.StatusOK, dispatchRuns(ctx, req, h.store, caps))
}

func dispatchRuns(ctx context.Context, req Request, rs store.RunStore, caps runops.Capabilities) Response {
	resp := Response{JSONRPC: "2.0", ID: req.ID}
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
			resp.Error = &ResponseError{Code: -32602, Message: "invalid params: " + err.Error()}
			return resp
		}
		raw, err := runops.Call(ctx, rs, caps, params.Name, params.Arguments)
		if err != nil {
			if errors.Is(err, runops.ErrCapabilityDenied) {
				resp.Error = &ResponseError{Code: -32601, Message: err.Error()}
				return resp
			}
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
			return resp
		}
		resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(raw)}}, "isError": false}
	default:
		resp.Error = &ResponseError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}
