# ADR-029: Sandboxed claw via hidden claw-runner NDJSON IPC

- **Status**: Accepted · **amended 2026-09-29** (see [Amendment](#amendment-2026-09-29--three-placements-and-a-boundary-held-by-the-launcher))
- **Date**: 2026-06-22
- **Authors**: Adry
- **Code**: [cmd/iterion/claw_runner.go](../../cmd/iterion/claw_runner.go), [pkg/backend/delegate/io.go](../../pkg/backend/delegate/io.go), [pkg/backend/delegate/multiplexer.go](../../pkg/backend/delegate/multiplexer.go)

## Context

Sandboxed claw calls need two properties that pull in opposite directions. Filesystem-affecting built-in tools must run inside the sandbox container so reads, writes, shell commands, and edits see the isolated run worktree rather than the launcher's host working directory. At the same time, MCP servers, custom engine-side tools, user prompts, and observability hooks often exist only in the launcher process.

A single local/remote placement for all tools cannot satisfy both constraints. The code therefore treats the sandbox boundary as a process boundary with an explicit typed protocol rather than trying to smuggle launcher closures into the container.

## Decision

Iterion runs sandboxed claw work through a hidden `iterion __claw-runner` subcommand inside the container. The runner is declared in [`cmd/iterion/claw_runner.go`](../../cmd/iterion/claw_runner.go) and exchanges bidirectional NDJSON envelopes over stdin/stdout.

The launcher sends one task envelope described by [`pkg/backend/delegate/io.go`](../../pkg/backend/delegate/io.go). The runner can emit intermediate envelopes for tool calls, ask-user requests, session capture, and events, and it finishes with one result envelope.

Inside the runner, built-in tools such as `bash`, `read_file`, `glob`, `grep`, `file_edit`, `web_fetch`, and `write_file` are rebuilt to execute locally in the container workdir. MCP tools, custom engine-side tools, and other launcher-only capabilities are represented as proxy tool definitions; their executions become envelope tool calls that the launcher-side multiplexer in [`pkg/backend/delegate/multiplexer.go`](../../pkg/backend/delegate/multiplexer.go) dispatches and correlates with tool-result envelopes.

## Trade-offs

| Dimension | Chosen hybrid runner | Pure launcher-side IPC | Pure in-container execution |
|---|---|---|---|
| Filesystem isolation | Built-ins mutate only the sandbox worktree. | Built-ins would run from the launcher process and can touch host state. | Built-ins are isolated. |
| Host-only tools | MCP/custom tools proxy to the launcher. | Host-only tools work naturally. | Host-only MCP servers and engine closures are unreachable. |
| Protocol complexity | Requires NDJSON envelopes, correlation IDs, and a multiplexer. | Simpler process placement. | Simpler tool placement. |

The honest concession is that the hybrid split makes tool placement an architectural category, not a mere implementation detail.

## Alternatives considered

### 1. Run every tool through launcher-side IPC

The runner could have proxied every tool call to the launcher and kept tool execution in one process.

**Rejected because**: that breaks sandbox filesystem isolation; `bash`, file readers, and editors would operate in the launcher's host cwd rather than the container worktree.

### 2. Run every tool locally inside the container

The runner could have attempted to instantiate all tools inside the container and avoid host callbacks.

**Rejected because**: MCP managers, custom engine tool closures, and user-interaction plumbing are launcher-side state and cannot be reconstructed inside the sandbox process.

## Consequences

- **Sandboxed built-ins are genuinely isolated.** File and shell operations execute where the run worktree is mounted, so tool side effects do not escape to the operator's host cwd.
- **Launcher-only capabilities remain available.** MCP/custom tools continue to work because the multiplexer bridges their calls back to the process that owns them.
- **The IPC protocol is now a compatibility seam.** Envelope types and correlation semantics must remain stable across runner and launcher code.
- **Observability crosses the same channel.** Session capture and events are modelled as envelopes rather than Go callbacks, which keeps closure state out of the sandbox.
- **Rechallenge if a third placement category appears.** Tools requiring simultaneous trusted host state and container-local filesystem state may need a third category beyond local built-ins and launcher-proxied tools.

## Amendment (2026-09-29) — three placements, and a boundary held by the launcher

The trigger above fired. A third category exists, and two sentences of the original decision no longer describe the code.

**1. Placement is an exhaustive, typed classification with three values.** `tool.SandboxPlacementOf` answers `Sandbox`, `Launcher` or `Refused`, and **`Refused` is the zero value**: a name nobody classified is refused rather than proxied. `lsp`, `screenshot`, `computer_use` and the `worker_*` family land there — they need trusted host state AND container-local state, exactly the case this ADR anticipated. A sandboxed node declaring one is refused when it EXECUTES, not when it is built, so the node's `fallbacks:` still get their turn.

**2. The launcher holds the boundary; the runner's routing is a request.** The original text reads as though the runner decides what to proxy. It cannot: the runner is the contained process, it may be an older binary baked into an image, and its stdout is writable from inside the container. The launcher therefore executes a forwarded call only when the tool was advertised to that node under that exact name, is `Launcher`-placed, and passes the run's `permission:` policy — re-evaluated on the tool's IDENTITY (its advertised name and its MCP FQN), never on the spelling the runner sent. Every refusal is logged and emitted by the launcher.

**3. "MCP tools … are represented as proxy tool definitions" (above) is now conditional.** An MCP *tool* is still launcher-placed. An MCP *server* is a process, and where it runs is the question this ADR exists to answer. `claude_code` and pi start their own servers, so a sandboxed node's servers run in the container with it; claw connects them in the launcher process. Under an active sandbox the launcher therefore starts only the OPERATOR's servers (`plugin` origin: a builtin, or a plugin installed under the iterion home the operator's own environment names — enabled and configured by the operator in both cases; builtins are the normal case). A server the node inherited is dropped with an `mcp_server_degraded` event; a server it names refuses the node at execution, so a route that starts the server in the container can serve it. Origins and consequences: [sandbox.md](../sandbox.md#mcp-servers-under-a-sandbox).

**What this does not change.** The hybrid split, the NDJSON protocol and the multiplexer stand as decided; the compatibility seam is unchanged. What the amendment records is that placement is now *enforced*, exhaustively, on the trusted side of the boundary — and that the honest concession above ("tool placement is an architectural category") extends to processes, not only to tools.

**The end state, still open.** An MCP manager inside the claw runner, so claw's servers run in the container like `claude_code`'s — parity, and the removal of the last launcher-side execution a sandboxed claw node can reach. It needs an IOTask field, secret/env crossing, plugin binaries in the sandbox images and a version handshake with the runner binary; it is tracked as a follow-up, and until it lands the refusal above is the typed alternative the backend-parity doctrine requires.
