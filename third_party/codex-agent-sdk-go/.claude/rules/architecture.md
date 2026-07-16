# Architecture

## Transport and Backend Model

The SDK supports two built-in execution backends:

1. `exec` backend (`codex exec`)
2. `app-server` backend (`codex app-server`)

## Backend Routing Rules

- `Query(...)` auto-selects backend from enabled options.
  - Uses `exec` when all selected options are supported there.
  - Routes to `app-server` when required by selected options.
- `QueryStream(...)` uses app-server semantics unless `WithTransport(...)` injects custom transport.
- `Client.Start(...)` uses app-server transport semantics.

Capability selection/validation logic lives in:

- `internal/config/capability.go`

## High-Level Components

- `query.go`: one-shot and query-stream orchestration
- `client.go` + `client_impl.go`: stateful client interface + implementation
- `with_client.go`: lifecycle helper wrapper
- `mcp.go`: `Tool` interface and `NewTool` constructor for SDK-defined tools
- `internal/subprocess/`: process and app-server adapters
- `internal/protocol/`: JSON-RPC/session controller, dynamic tool dispatch
- `internal/message/`: parsing + public message mapping
- `internal/config/`: options, `DynamicTool` type, and backend capability policy
- `internal/session/`: SQLite-based session metadata lookup
- `internal/mcp/`: MCP server integration and status

## Change Impact Guidance

When changing options, transport, or session behavior:

- verify backend capability mapping in `internal/config/capability.go`
- update related tests (capability, query/client behavior)
- keep docs aligned (`README.md`, package docs, CLAUDE rules where needed)
