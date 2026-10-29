package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/facade"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The connector MCP facade endpoint (ADR-098 §2, #2758): how an AGENT reaches
// a connector's operations with the credential never leaving the server.
//
// It rides the board MCP transport's whole shape, on purpose — one pattern
// for "a host-owned MCP surface a sandboxed agent calls over HTTP": the same
// X-Iterion-Run per-run token, the same registry (whose grant now carries the
// run's tenant AND its trust), the same line-delimited JSON-RPC, the same
// per-run gateway listener. What differs is what a call DOES: the grant
// authorizes the facade only when it carries the `connectors` capability, the
// alias resolves through the tenant-scoped connection store, and the
// connection must itself declare `agent` — the workflow declares the use, the
// operator grants it, and both halves meet at the one chokepoint
// (Resolver.ResolveAgent).

// ConnectorMCPDeps is what the facade endpoint needs wired. All four are
// required: a facade with no store resolves nothing, one with no sealer must
// not open credentials, one with no catalog has no operations to serve, and
// the client is the guarded one (connection.CloudHTTPClient in cloud, the
// local tier's otherwise) — never a default http.Client, which the executor
// itself refuses.
type ConnectorMCPDeps struct {
	Store   connection.Store
	Catalog connection.Catalog
	Sealer  secrets.Sealer
	Client  *http.Client
	Logger  *iterlog.Logger
}

// RegisterConnectorMCPRoutes wires the facade endpoint onto mux — the
// DEDICATED host-MCP mux of boardMCPServiceOption, never the authenticated
// server mux: this handler's whole authorization is the run token.
func RegisterConnectorMCPRoutes(mux *http.ServeMux, prefix string, deps ConnectorMCPDeps, reg BoardMCPTokenStore) {
	h := &connectorMCPHandler{deps: deps, registry: reg}
	p := strings.TrimRight(prefix, "/")
	mux.HandleFunc("POST "+p, h.serve)
	mux.HandleFunc("POST "+p+"/", h.serve)
	// Same transport contract as the board endpoint: no SSE stream, and a
	// 405 (not 404) so a Streamable-HTTP client falls back to POST-only
	// instead of aborting the connection.
	mux.HandleFunc("GET "+p, boardMCPMethodNotAllowed)
	mux.HandleFunc("GET "+p+"/", boardMCPMethodNotAllowed)
}

type connectorMCPHandler struct {
	deps     ConnectorMCPDeps
	registry BoardMCPTokenStore

	// Built once: the packages are process-stable (the baked floor is
	// compiled in; the FSCatalog above it caches), so the tool set does not
	// change between two calls of one server.
	facadeOnce sync.Once
	facade     *facade.Facade
	facadeErr  error
}

// theFacade loads every connector the catalog serves into ONE facade. A
// package the catalog lists but cannot load is skipped with a loud log
// rather than taking the whole facade down — the same defect already refuses
// the node path at its own resolution, so nobody is told a quieter lie here.
func (h *connectorMCPHandler) theFacade() (*facade.Facade, error) {
	h.facadeOnce.Do(func() {
		var pkgs []*spec.Package
		for _, id := range h.deps.Catalog.Connectors() {
			pkg, _, err := h.deps.Catalog.Package(id)
			if err != nil {
				if h.deps.Logger != nil {
					h.deps.Logger.Error("connector MCP: the catalog lists %q but it cannot load; excluded from the facade: %v", id, err)
				}
				continue
			}
			pkgs = append(pkgs, pkg)
		}
		h.facade, h.facadeErr = facade.New(pkgs...)
	})
	return h.facade, h.facadeErr
}

func (h *connectorMCPHandler) serve(w http.ResponseWriter, r *http.Request) {
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
	// The capability half of the grant: the workflow (through the node the
	// token was minted for) must have declared `connectors`. Without the
	// check, any run token — minted for board caps alone — would spend the
	// tenant's `agent` connections.
	if !grant.Capabilities[connection.CapConnectors] {
		http.Error(w, "this run token holds no connectors capability", http.StatusForbidden)
		return
	}

	var req mcpReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // same 1 MB cap as the board endpoint
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, mcpResp{
			JSONRPC: "2.0",
			Error:   &mcpRespError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return
	}
	if req.ID == nil {
		// Notification — no response expected.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSONStatus(w, http.StatusOK, h.dispatch(r.Context(), req, grant))
}

func (h *connectorMCPHandler) dispatch(ctx context.Context, req mcpReq, grant boardMCPGrant) mcpResp {
	resp := mcpResp{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "iterion-connectors-http", "version": "1.0.0"},
		}
	case "tools/list":
		f, err := h.theFacade()
		if err != nil {
			resp.Error = &mcpRespError{Code: -32603, Message: "facade unavailable: " + err.Error()}
			return resp
		}
		tools := f.Tools()
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
		out, err := h.call(ctx, params.Name, params.Arguments, grant)
		if err != nil {
			// A refusal — the grant, the capability, the trust, the alias —
			// is the tool's ANSWER, as for the board endpoint: the model
			// reads it and can act on it (ask for a grant, pick another
			// alias), which a protocol error would not let it do.
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}
			return resp
		}
		resp.Result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": out}},
			"isError": false,
		}
	default:
		resp.Error = &mcpRespError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp
}

// call runs one tool through the facade, with the execution half wired to the
// grant: the tenant it pins, the trust it carries.
func (h *connectorMCPHandler) call(ctx context.Context, tool string, args json.RawMessage, grant boardMCPGrant) (string, error) {
	f, err := h.theFacade()
	if err != nil {
		return "", err
	}
	resolver := h.resolverFor(grant)
	out, err := f.Call(ctx, tool, args, h.execWith(resolver))
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// resolverFor builds the per-call resolver from the GRANT, so the tenant and
// the trust of a facade call are the run's — read at mint time from the run
// document — and never anything the MCP request itself claims.
//
// The tenant falls back to connection.LocalTenant when the grant carries
// none: local mode mints grants without one (there is exactly one tenant on a
// laptop), and the FileStore's records live under it.
func (h *connectorMCPHandler) resolverFor(grant boardMCPGrant) *connection.Resolver {
	tenantID := grant.TenantID
	if tenantID == "" {
		tenantID = connection.LocalTenant
	}
	trust := connection.TrustUnknown()
	if grant.TrustStated {
		trust = connection.TrustOfRun(store.RunTrust(grant.Trust))
	}
	return &connection.Resolver{
		Catalog:  h.deps.Catalog,
		Store:    h.deps.Store,
		Sealer:   h.deps.Sealer,
		TenantID: tenantID,
		Trust:    trust,
	}
}

// execWith is the facade's ExecFunc: resolve the agent-chosen operation
// against the run's grant, then perform the call — HERE, in the server
// process, on the guarded client. The token never leaves it.
func (h *connectorMCPHandler) execWith(resolver *connection.Resolver) facade.ExecFunc {
	return func(ctx context.Context, opID, alias string, params map[string]any) (any, error) {
		if strings.TrimSpace(alias) == "" {
			return nil, errors.New("the connection argument is required — it names which of the tenant's connections authenticates the call")
		}
		pkg, op, cred, baseURL, err := resolver.ResolveAgent(ctx, opID, alias)
		if err != nil {
			return nil, err
		}
		ex := &exec.Executor{Client: h.deps.Client, BaseURL: baseURL, UserAgent: connectorFacadeUserAgent}
		res, err := ex.Call(ctx, pkg, op, params, cred)
		if err != nil {
			return nil, err
		}
		if res.Err != nil {
			// The vendor's own failure is an answer the model can read and
			// branch on — the same shape the deterministic node surfaces.
			return map[string]any{
				"error":  res.Err.Error(),
				"status": res.Status,
			}, nil
		}
		return map[string]any{
			"status":   res.Status,
			"data":     res.Data,
			"pending":  res.Pending,
			"requests": res.Requests,
		}, nil
	}
}

// connectorFacadeUserAgent identifies the facade's calls to the vendor,
// distinct from the node path's: an operator reading the vendor's audit log
// can tell "an agent chose this call" from "a workflow declared it".
const connectorFacadeUserAgent = "iterion-connector-facade"
