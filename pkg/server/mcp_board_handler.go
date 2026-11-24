package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/hostmcp"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The sandboxed host-MCP transport (C082) lives in pkg/hostmcp so the two
// serving processes — this server AND the cloud runner pod (#2632) — share
// one implementation. This file keeps the server's historical names as thin
// wrappers: the dedicated host-MCP mux of boardMCPServiceOption, the main
// mux's /api/v1/mcp/board + /api/v1/mcp/runs mounts (server_routes.go), the
// connector facade's grant reads (mcp_connector_routes.go) and the Valkey
// cross-replica token store (valkey_stores.go) all hang off them.

// boardMCPDefaultTTL caps how long a board MCP run token stays alive without
// an explicit Revoke; a run that crashes mid-flight would otherwise leak its
// token until process exit.
const boardMCPDefaultTTL = hostmcp.DefaultGrantTTL

// boardMCPMaxTokens bounds the in-memory registry so a misbehaving caller
// that Registers without Revoking can't grow it unbounded.
const boardMCPMaxTokens = hostmcp.MaxTokens

// BoardMCPTokenRegistry is the ephemeral in-memory token store used to
// authorize sandbox-running bots that talk to the host's board/runs MCP HTTP
// endpoints. Single-process; the Valkey store (valkey_stores.go) is the
// cross-replica twin.
type BoardMCPTokenRegistry = hostmcp.Registry

// boardMCPGrant is one token's grant: capabilities, the tenant the token is
// pinned to, and the run facts the serving side dispatches under.
type boardMCPGrant = hostmcp.Grant

// BoardMCPTokenStore is the per-run board-MCP token registry. Register is
// called by the server's per-node token-minter closure
// (Server.boardMCPServiceOption) — a Register failure means the minted token
// would never authorize, so the minter must not hand it out; Lookup is
// called by the HTTP handlers. The contract is hostmcp.TokenStore: the
// in-memory *BoardMCPTokenRegistry is single-process, the Valkey impl
// (valkey_stores.go) shares tokens across replicas, since a token-authenticated
// call to the load-balanced mux may land on a pod that did not mint it.
type BoardMCPTokenStore interface {
	hostmcp.TokenStore
}

// The JSON-RPC protocol shapes the connector facade shares with the
// board/runs handlers.
type (
	mcpReq       = hostmcp.Request
	mcpResp      = hostmcp.Response
	mcpRespError = hostmcp.ResponseError
)

// NewBoardMCPTokenRegistry returns an empty registry.
func NewBoardMCPTokenRegistry() *BoardMCPTokenRegistry {
	return hostmcp.NewRegistry()
}

// newBoardMCPToken mints a random opaque X-Iterion-Run token (32 bytes hex)
// for a per-node board MCP grant. Returns "" on the (effectively impossible)
// crypto/rand failure so callers degrade to board-disabled rather than
// panicking.
func newBoardMCPToken() string {
	return hostmcp.MintToken()
}

// boardMCPMethodNotAllowed signals "no SSE stream here; use POST" per the
// MCP Streamable HTTP transport contract.
var boardMCPMethodNotAllowed = hostmcp.MethodNotAllowed

// RegisterBoardMCPRoutes wires the board MCP HTTP endpoints onto the given
// mux against the server's ONE native store: the local single-tenant mount,
// where the grant's (empty) tenant resolves the same store every time. Cloud
// per-tenant boards are served by the runner-side mount of the same routes
// (pkg/runner, #2632). The handler authenticates via `X-Iterion-Run` (token
// issued by the runtime at run-start), gates each tool by the granted
// capability set, and dispatches into boardops.
func RegisterBoardMCPRoutes(mux *http.ServeMux, prefix string, store *native.Store, reg BoardMCPTokenStore) {
	hostmcp.RegisterBoardRoutes(mux, prefix, func(string) native.BoardStore { return store }, reg)
}

// RegisterRunsMCPRoutes exposes runops over Streamable HTTP. The RunStore is
// fixed by the host (project-scoped filesystem locally, tenant-scoped Mongo
// in cloud); the model can select only a run id inside that store.
func RegisterRunsMCPRoutes(mux *http.ServeMux, prefix string, rs store.RunStore, reg BoardMCPTokenStore) {
	hostmcp.RegisterRunsRoutes(mux, prefix, rs, reg)
}

// writeJSONStatus is the package's JSON responder — it predates the MCP
// handlers' move to pkg/hostmcp and stays here for the forge/launch routes
// that share it.
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	httpx.WriteErrorJSON(w, status, body)
}

// Compile-time sentinel: the registry alias must keep satisfying the store
// interface the handlers authorize against.
var _ BoardMCPTokenStore = (*BoardMCPTokenRegistry)(nil)
