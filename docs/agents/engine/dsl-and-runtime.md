# DSL and runtime — the compiled graph and how it executes

Node types, edge and reference syntax, the workflow-level fields
(`compress:`, `auto_memory:`, `permission:`, budget), and what the engine does
with them at run time: error handling, persistence, resume, concurrency,
worktree finalization.

Full language reference: [../dsl.md](../../dsl.md). This page is the working
summary plus the gotchas that cost a session each.

## Architecture

`.bot` files are parsed into an **AST**, compiled into an **IR** (directed graph of nodes and edges), validated, then executed by the **runtime** engine. Nodes include Agent (LLM), Judge, Router, Human (pause/resume), Tool, Compute, Subbot (nested child `.bot` run), and terminal nodes (Done/Fail). Parallel branches converge on downstream nodes via `await: wait_all` or `await: best_effort`; there is no top-level Join node. The runtime supports parallel branch scheduling, loop detection, budget enforcement, and resumable execution.

### Compilation Pipeline

```
.bot source → Lexer (indent-sensitive tokens) → Parser (recursive-descent) → AST
  → ir.Compile() → IR Workflow (nodes + edges + schemas + prompts + budget)
  → Diagnostics from ir.Compile() / ir.Validate() (sparse codes C001–C199 + C2xx: compile errors, reachability, routing, cycles, attachments, presets, capability checks (C080–C082), cursor declarations (C083–C086), etc.)
  → runtime.Engine.Run() → execution with events, budget, and persistence
```

### Node Types

| Type | Description |
|------|-------------|
| **Agent** | LLM node with tools, structured I/O, and any selected backend (`claw`, `claude_code`, `codex`, `pi`, `kimi`, `grok`, or `opencode`) |
| **Judge** | LLM node producing verdicts (typically no tools) |
| **Router** | Routing node with 5 modes: `fan_out_all`, `fan_out_each`, `condition`, `round_robin`, `llm` (see `docs/routers.md`) |
| **Human** | Pause/resume via `interaction: human` (default for human nodes); optional `interaction: llm` or `llm_or_human` can auto-answer or escalate. Agent/judge nodes can instead declare **`interaction: async`** (**ADR-081**): the agent posts questions via `ask_user_async` and KEEPS WORKING — answers arrive in its message queue (node-scoped inbox) whenever the operator replies; the `await_answers` tool is the LLM-discretion sync point (pauses only if something is still pending). See [docs/async-interaction.md](../../async-interaction.md). |
| **Tool** | Direct shell command execution (no LLM). ACTION tool nodes may opt into the **Verified Action** quad (`goal`+`postcondition`+`policy`+`recovery`) so a brittle recipe self-heals (idempotent-skip → recipe → self-repair → agent → policy) instead of hard-blocking; the postcondition is the deterministic truth oracle at every rung. **Gates stay deterministic** — never attach recovery to a `recipe == postcondition` gate (enforced by C103–C106). See [docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md](../../adr/044-adaptive-recovery-for-deterministic-action-nodes.md). |
| **Compute** | Deterministic expression node for derived structured output (no LLM, no shell) |
| **Subbot** | Runs another `.bot` as a nested child run (`subbot <name>:` with `source:`, var mapping, resource leases, and an `isolated:` workspace-safety assertion); child outputs read back as `outputs.<subbot>.<field>`. Diagnostic C119. |
| **Emit** / **Wait** | In-bot event-driven primitives (**ADR-051**): `emit` publishes a named run-scoped event with an immutable payload; `wait` blocks a branch until that event fires (mandatory `timeout:` — the bornage). A reactive coordination pair between parallel branches (actor/CSP model, **not** the JS event loop — payloads are immutable, no shared mutable heap), backed by a run-local *reliable* registry, distinct from the lossy cross-run `pkg/eventbus`. Diagnostics C196–C198. See [docs/adr/051-in-bot-event-driven-primitives.md](../../adr/051-in-bot-event-driven-primitives.md) + [examples/events/pingpong.bot](../../../examples/events/pingpong.bot). |
| **Await answers** | `await_answers` (**ADR-081**): the deterministic sync point for async human questions — parks its branch (only its branch) until every pending `ask_user_async` question of the `from:` node (or the whole run) is answered; mandatory `timeout:`; output `{answers: [...]}`. Level-triggered against the interaction store (doorbell + 5s poll), so cross-process answers and resume both work. Diagnostics C240–C242. See [docs/async-interaction.md](../../async-interaction.md) + [examples/async-questions/main.bot](../../../examples/async-questions/main.bot). |
| **Done** | Terminal: workflow success |
| **Fail** | Terminal: workflow failure |


## Where the rest went

[DSL quick reference](dsl-quick-reference.md) · [errors, persistence and resume](dsl-errors-resume.md) · [worktree finalization](worktree-finalization.md).
