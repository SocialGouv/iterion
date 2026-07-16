# Project Overview

## Identity

- Repository: `github.com/ethpandaops/codex-agent-sdk-go`
- Primary package: `codexsdk`
- Go version: `1.26+`
- Minimum Codex CLI version: `0.103.0+` (see `version.go`)

## What This SDK Exposes

- One-shot query API: `Query(ctx, prompt, opts...)`
- Streaming input query API: `QueryStream(ctx, messages, opts...)`
- Stateful client API: `NewClient()` + `Client` methods
- Lifecycle helper: `WithClient(ctx, fn, opts...)`
- Session metadata API: `StatSession(ctx, sessionID, opts...)`

## Primary Public Surface Areas

- `options.go`: all `WithXxx(...)` constructors
- `mcp.go`: `Tool` interface, `NewTool` constructor, and `WithSDKTools` tool registration
- `client.go`: `Client` interface
- `query.go`: top-level query functions
- `session_stat.go`: `SessionStat` struct and `StatSession()` function
- `types.go`: re-exported message/content/config types
- `errors.go`: typed and sentinel errors

## Documentation Sync Expectations

When public APIs or options change, update in the same PR:

- `README.md` (user-facing)
- `doc.go` (package docs)
- `CLAUDE.md` / `.claude/rules/*` if agent guidance changed
