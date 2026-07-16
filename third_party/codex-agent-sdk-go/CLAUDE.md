# CLAUDE.md

Shared repository instructions for coding agents.

## Universal Summary (Codex + Claude)

### Repository Facts

- Module: `github.com/ethpandaops/codex-agent-sdk-go`
- Primary package: `codexsdk`
- Go: `1.26+`
- Minimum Codex CLI: `0.103.0+` (see `version.go`)

### Core APIs

- One-shot: `Query(ctx, content, opts...)`
- Streaming input: `QueryStream(ctx, messages, opts...)`
- Stateful sessions: `NewClient()` + `Client`
- Lifecycle helper: `WithClient(ctx, fn, opts...)`
- Session metadata: `StatSession(ctx, sessionID, opts...)`
- Session listing: `ListSessions(ctx, opts...)`
- Session rollouts: `GetSessionMessages(ctx, sessionID, opts...)`

### Canonical Commands

```bash
go build ./...
go test ./...
go test -race ./...
go test -v -run TestQuery ./...
go test -tags=integration ./integration/...
golangci-lint run
```

### Architecture Facts

- `Query(...)` auto-selects backend (`exec` vs `app-server`) based on enabled option support.
- `QueryStream(...)` uses app-server semantics unless a custom transport is injected with `WithTransport(...)`.
- `Client.Start(...)` uses app-server transport semantics.
- Backend option capability policy lives in `internal/config/capability.go`.

### Boundaries

Always:

- Follow nearby code patterns before introducing new patterns.
- Keep behavior changes covered by tests in the same PR.
- Keep docs aligned when public behavior changes (`README.md`, `doc.go`, and agent guidance docs).

Ask first:

- Adding exported API surface.
- Changing transport/protocol interfaces.
- Adding new third-party dependencies.

Never:

- Ignore returned errors.
- Store `context.Context` in structs.
- Bypass backend capability checks for options.

## Claude Modules

For Claude Code, load and follow these detailed modules:

@.claude/rules/project-overview.md
@.claude/rules/build-and-test.md
@.claude/rules/architecture.md
@.claude/rules/coding-conventions.md
@.claude/rules/boundaries.md

If guidance appears to conflict, prioritize:
1. `boundaries.md`
2. `coding-conventions.md`
3. the remaining modules
