# ADR-065: Dedicated CLI-agent backend for non-claude-code argument protocols

- **Status**: Accepted
- **Date**: 2026-07-09
- **Code**: [pkg/backend/delegate/cliagent.go](../../pkg/backend/delegate/cliagent.go) (`CLIAgentBackend`, `CLIAgentProtocol`), [pkg/backend/delegate/kimi.go](../../pkg/backend/delegate/kimi.go) (`BackendKimi`, `kimiProtocol`), [pkg/backend/delegate/grok.go](../../pkg/backend/delegate/grok.go) (`BackendGrok`, `grokProtocol`)

## Context

iterion had no way to run a third-party agent CLI whose argument protocol
differs from claude-code / codex. The `claude_code` backend drives its CLI in
**Session mode**: `--print`, `--input-format`/`--output-format stream-json`
with the prompt on **stdin**, `--permission-mode`, `--append-system-prompt`,
`--model`, … A per-node `command:` override (#76) only swaps the *binary*
while keeping that exact argv, so it can only drive a claude-code-**compatible**
CLI (a pinned build, a compatible proxy/wrapper).

Moonshot's **`kimi-code`** is not compatible. Its flag surface is disjoint:

```
kimi -p <prompt> --output-format {text,stream-json} [-m <alias>]
```

`kimi --print …` fails with `error: unknown option '--print' (Did you mean
--prompt?)` — the prompt is a `-p <prompt>` *value*, not stdin under
`--print`. #76 cannot run it. This is the separate, larger need #75/#76
deferred: running an agent CLI with a **different argument protocol**.

## Decision

Add a **generic, protocol-driven CLI-agent backend** rather than a bespoke
one-off per CLI. `CLIAgentProtocol` declares, as data, everything that varies
between agent CLIs:

- **argv shape** — prompt delivery (`-p <prompt>` arg vs. bare-switch + stdin),
  the output-format flag/value, the model flag, an optional system-prompt flag,
  static extra args;
- **model mapping** — `MapModel` translates iterion's `provider/model` spec
  into the CLI's native alias (kimi: strip the provider segment → `-m kimi-k2`);
- **effort mapping** — `MapEffort` maps a reasoning-effort level onto extra
  argv, or is `nil` when the CLI has no effort dial (kimi);
- **output parsing** — `ParseOutput` turns raw stdout into the assistant's
  final text (+ session id, tokens); the shared, schema-aware `parseSDKOutput`
  fallback then handles `output:` schemas uniformly with the other backends;
- **credential/endpoint resolution** — `ResolveEnv` layers overrides sourced
  from the CLI's own config/env conventions, or is `nil` to let the CLI resolve
  its own credentials from the inherited host environment (kimi reads e.g.
  `$MOONSHOT_API_KEY` itself).

`CLIAgentBackend` wraps a protocol and mirrors the **codex backend's shape**:
build native argv → run with a wall-clock timeout (host or, when sandboxed,
inside the run's container via `sandbox.Run.Command`) → parse stdout → retry on
a no-output/network transient. Two concrete instances ship today, both
registered in `DefaultRegistry`:

- **kimi** (`backend: "kimi"`, `kimiProtocol`) — Moonshot kimi-code. No
  system-prompt flag: the node's composed `system:` is folded in as a preamble
  to the `-p` prompt (`SystemPromptStandalone`).
- **grok** (`backend: "grok"`, `grokProtocol`) — xAI Grok Build CLI. System
  prompt is delivered via `--rules` (append to the CLI's native agentic
  baseline — `SystemPromptAppendToNative`); non-interactive tool approval is
  forced with `--permission-mode bypassPermissions --always-approve`; model
  and effort map to `-m` / `--reasoning-effort`.

When a protocol's `SystemPromptFlag` is empty (kimi), the node's task still
reaches the agent via the preamble fold; when it is set (grok `--rules`), the
flag carries the author text and the CLI keeps its native posture.

## Trade-offs / Consequences

- **Tools are not host-gated.** Like codex, kimi runs with its *own* built-in
  toolset; iterion's node `tools:`/`AllowedTools` list is advisory only — the
  protocol has no lever to restrict the target CLI's shell. Nodes needing a
  hard permission boundary should use `claude_code` (the permission gate) or
  `claw` (native tool restriction). Documented in docs/backends.md.
- **Sessions/continuity are best-effort.** We capture a `session_id` from the
  stream when present, but resume/fork is not wired for CLI-agent backends yet
  (the CLI's resume protocol is per-vendor). Kimi runs fresh each node.
- **Auto-detection excludes them.** kimi and grok are explicit opt-in
  (`backend: "kimi"` / `backend: "grok"`); the credential detector never
  auto-selects them, so no host with a Moonshot key or a Grok Build install
  silently re-targets away from claude_code.

## Alternatives considered

### 1. A hardcoded, per-CLI backend (a `KimiBackend` from scratch)
Straightforward but non-reusable: the next CLI (Gemini CLI, aider, …) repeats
all the subprocess/timeout/retry/parse plumbing. **Rejected** — the varying
parts are pure data; a declarative protocol captures them without new Go per
CLI (the next backend is a new `CLIAgentProtocol` value, not a new type).

### 2. Extend the `command:` override (#76) with argv templating
Fold "different protocol" into the same field. **Rejected** — #76 is precisely
scoped to *claude-code-compatible* binaries (pinned build / compatible proxy),
where keeping the Session-mode argv is the point. Overloading it with a full
argv-templating mini-language would blur a clean boundary and reimplement, in
config, what a typed protocol expresses in code. #76 stays the binary swap;
this ADR is the protocol switch.

### 3. A per-node `claw` endpoint override (the original #75)
Point claw's OpenAI-compatible client at Moonshot's API. **Rejected upstream**
already: it drives the *model API*, not the *agent CLI* — it loses kimi-code's
own tool loop / agent behaviour, which is the reason to run kimi-code at all.
