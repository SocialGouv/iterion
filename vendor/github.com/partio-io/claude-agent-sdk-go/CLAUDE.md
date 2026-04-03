# CLAUDE.md — claude-agent-sdk-go

## Project Overview

Go SDK for the Claude Code CLI. Spawns `claude` as a subprocess and communicates via NDJSON over stdin/stdout. Targets the v2 session-based API pattern.

**Module:** `github.com/partio-io/claude-agent-sdk-go`
**Go version:** 1.26
**Dependencies:** Zero (stdlib only)

## Project Structure

```
claude.go                  Prompt() one-shot function
session.go                 Session: NewSession, ResumeSession, Send, Stream, Close
message.go                 Message sealed interface + 5 concrete types + JSON dispatch
content.go                 ContentBlock sealed interface + 4 concrete types
option.go                  config struct + all With* functional options
error.go                   Sentinel errors + typed errors (ProcessError, ParseError, etc.)
hook.go                    HookEvent constants, HookMatcher, HookCallback, input/output types
mcp.go                     MCPServerConfig sealed interface (Stdio, SSE, HTTP)
agent.go                   AgentDefinition for subagents
channel.go                 ToChan() adapter: iter.Seq2 → chan
uuid.go                    stdlib-only UUID v4 via crypto/rand
doc.go                     Package-level godoc
internal/
  process/
    process.go             Subprocess lifecycle (spawn, pipes, signal, cleanup)
    cli.go                 CLI binary discovery (PATH, ~/.claude/local/, explicit)
    args.go                Config → CLI flag builder
  protocol/
    parser.go              Raw JSONL → typed Message dispatch
    control.go             Control protocol: request/response multiplexing
  testutil/
    mock_process.go        Mock subprocess (shell scripts feeding NDJSON)
    fixtures.go            Captured NDJSON from real CLI sessions
examples/
  prompt/main.go           One-shot prompt
  session/main.go          Multi-turn session
  resume/main.go           Session resume
  streaming/main.go        Token-level streaming
  hooks/main.go            Pre/Post tool use hooks
  mcp/main.go              MCP server integration
```

## Build & Test

```bash
go build ./...             # Build all packages
go test ./...              # Run unit tests (uses mock subprocess)
go test -tags=integration  # Run integration tests (requires claude CLI)
go vet ./...               # Static analysis
```

## Architecture

- **Sealed interfaces** for Message and ContentBlock: exhaustive type switches, no external implementations.
- **Functional options** (Rob Pike pattern) for all configuration.
- **Flat package layout**: single import `claude.Prompt()`, no sub-package verbosity.
- **iter.Seq2[Message, error]** for streaming: no goroutine leaks, composable, natural for-range.
- **Process boundary**: all CLI interaction goes through internal/process; tests mock at NDJSON level.

## Key Patterns

- One primary concern per file.
- Table-driven tests with `t.TempDir()` and `t.Setenv()`.
- Standard `testing` package only — no testify, no gomock.
- Zero external dependencies.
- `go vet` clean.

## Control Protocol

The SDK communicates with the CLI via a bidirectional control protocol:
- **SDK → CLI**: initialize, interrupt, set_model, etc.
- **CLI → SDK**: can_use_tool (permissions), hook_callback, mcp_message.
- Requests use `{"type":"control_request","request_id":"req_N_hex","request":{...}}`.
- Responses use `{"type":"control_response","response":{"subtype":"success|error",...}}`.
