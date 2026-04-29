# Next-session roadmap — iterion × claw-code-go

Drop-in starting point for a fresh agent picking up the work. Read top
to bottom; pick a track; ship.

---

## Where we are (snapshot 2026-04-29)

- claw-code-go `master` is **13 commits ahead** of origin (deferred-features sprint + simplify pass + security fixes + coverage expansion). All unit + integration tests green.
- iterion `main` is **5 commits ahead** of origin (handoff + e2e coverage doc + computer-use adapter + gitignore fix). `task check` green.
- Live e2e: 9 (existing) + 1 (`TestLive_Lite_ClawReadImage`) = **10 PASS**.
- Permanent docs:
  - [`docs/claw_deferred_handoff.md`](claw_deferred_handoff.md) — deferred features status
  - [`docs/e2e_coverage.md`](e2e_coverage.md) — coverage matrix per feature
  - [`/workspaces/iterion/.works/claw-code-go/docs/review_rationale.md`](../.works/claw-code-go/docs/review_rationale.md) — load-bearing non-bugs to skip on future reviews

**Before you push**: claw-code-go and iterion both need pushing. The
`replace github.com/SocialGouv/claw-code-go => ./.works/claw-code-go`
in [iterion `go.mod`](../go.mod) stays until claw is pushed and you run
`go get @latest && go mod tidy && go mod vendor`.

---

## The big gap I noticed at the end

claw-code-go ships ~50 built-in tools. iterion exposes **7** by default
via `tool.RegisterClawBuiltins` (read_file, write_file, glob, grep,
file_edit, web_fetch, bash) plus `RegisterClawComputerUse` as opt-in.

Every iterion workflow agent today is missing access to: **subagents
(`agent`, `task_*`, `run_task_packet`), workers (`worker_*` 9 tools),
todo (`todo_write`), tool_search, skill, plan_mode, MCP resource
tools, cron, teams, lsp, repl, notebook_edit, structured_output**.

This is the single highest-impact change in the codebase right now.
The infrastructure is there; the iterion-side adapter is missing.

### Track 0 (priority 1): Expand `RegisterClawBuiltins` coverage

Add curated registration helpers in `tool/claw_builtins.go`:

```go
RegisterClawSubagents(reg)      // agent, task_*, run_task_packet
RegisterClawWorkers(reg)         // worker_create/observe/...
RegisterClawTodo(reg)            // todo_write
RegisterClawSearch(reg)          // tool_search, skill, web_search
RegisterClawPlanMode(reg)        // enter/exit_plan_mode
RegisterClawMCPResources(reg)    // list/read_mcp_resource, mcp_auth
RegisterClawCron(reg)            // cron_*
RegisterClawTeams(reg)           // team_*
```

Each helper follows the same pattern as `RegisterClawComputerUse` (see
[tool/claw_builtins.go:51](../tool/claw_builtins.go#L51)). Some tools
need workspace/registry context (workers need a worker registry,
agent needs an agent runtime) — those need a constructor that takes
the dependency or a sensible default.

Per-helper unit tests follow the pattern in
[tool/claw_builtins_test.go](../tool/claw_builtins_test.go).

For the live tests: add a `TestLive_Lite_ClawSubagents` that runs a
workflow where the parent agent spawns 2 child agents via the `agent`
tool, then aggregates their outputs. Estimate ~150 LOC of fixture +
test, most reused from `claw_builtin_tools.iter`.

**Effort**: 1 session (~4h). **Impact**: every iterion workflow
suddenly has full claw tool surface. This is the unlock.

---

## Track 1: Lifecycle hooks plumbed end-to-end (Phase 6 finisher)

**State**: claw-code-go's `lifehooks.Runner` is implemented and
unit-tested (`internal/runtime/conversation_lifecycle_test.go`),
including the new ctx propagation. iterion never installs one on its
ClawExecutor.

**Goal**: `model.WithLifecycleHooks(runner)` option passed to
`NewClawExecutor` plumbs through to the underlying claw client. Then:

- audit hook → log every tool call to OTLP exporter
- safety hook → Block dangerous bash patterns (`rm -rf /`,
  `:(){ :|: & };:`, etc.)
- compaction observability → measure token reduction at every
  compact, persist as event

**Files to touch**:
- [model/executor.go](../model/executor.go) — add option + wire to
  `model.NewClawBackend`
- [model/claw_backend.go](../model/claw_backend.go) — pipe through to
  `runtime.ConversationLoop.LifecycleHooks`
- new fixture `examples/claw_lifecycle_hooks.iter`
- new live test `TestLive_Lite_ClawLifecycleHooks`

**Reference**:
[`.works/claw-code-go/internal/hooks/runner.go`](../.works/claw-code-go/internal/hooks/runner.go)
for the `Runner` API.

**Effort**: 1 session (~3h). **Impact**: production-grade audit/safety.

---

## Track 2: OAuth broker auto-wired into MCP servers

**State**: `oauth.Broker` is fully implemented + tested.
`SSETransport.SetAuthFunc` accepts dynamic auth. `BearerHeaderFunc`
bridges them. **No code path invokes the bridge**.

**Goal**: when `mcp_servers` config in a `.iter` workflow declares
`auth_type: oauth2 + auth_url + token_url + client_id + scopes`,
iterion's MCP init constructs a Broker, calls
`broker.BearerHeaderFunc(cfg)`, and passes the closure to
`SSETransport.SetAuthFunc`.

**Files to touch**:
- [mcp/init.go](../mcp/init.go) (or wherever `PrepareWorkflow` builds
  transports)
- [.works/claw-code-go/internal/runtime/conversation.go::InitMCPFromConfig](../.works/claw-code-go/internal/runtime/conversation.go) — same wiring on the claw side for non-iterion users
- New `MCPServerConfig.OAuth` shape in iterion's IR
- Fixture: a `.iter` workflow that talks to a mock OAuth-protected MCP
  server (httptest-style, even live)

**Risk**: real OAuth providers require a browser callback → live test
needs a programmatic browser opener (already supported by the broker
via `WithAuthOpener`).

**Effort**: 1.5 sessions (~6h). **Impact**: unlocks GitHub/Linear/
Notion-class MCP servers behind OAuth.

---

## Track 3: Plugin marketplace with a real catalog

**State**: marketplace + installer + manager + `/store` slash command
all done and tested (httptest). No real catalog exists, so `/store`
errors out without `CLAW_MARKETPLACE_URL` set.

**Two options** depending on intent:

### Option A — Keep opt-in, add `claw store init`
A scaffolder that generates a local `~/.claw/marketplace/catalog.json`
template + a `serve` subcommand that fronts it via http. For users who
want plugin distribution inside their team without a public registry.

### Option B — Ship a public catalog
Pick a small initial set (e.g. `linter-go`, `tester-pytest`,
`prettier-format`), publish to a static GitHub Pages JSON, default
`CLAW_MARKETPLACE_URL` to point to it. Real plugin ecosystem,
governance becomes the question.

**Effort**: A is 1 session, B is 2-3 sessions + ongoing maintenance.

---

## Track 4: Permission classifier non-trivial (Phase 7 ModeAuto)

**State**: `permissions.RuleClassifier` is a static safe-list (read-only
tools allowed, web_fetch HTTPS allowed, everything else asks).
`ModeAuto` was implemented but uses the static classifier today.

**Goal**: replace `RuleClassifier` with `LLMClassifier` — for each
tool call, ask a small fast model (`anthropic/claude-haiku-*`) to
classify allow/ask/deny based on tool name + input + recent
conversation context. Cache aggressively by `(tool, summary)` hash.

**Files to add**:
- `.works/claw-code-go/internal/permissions/llm_classifier.go`
- LLM client passed in via constructor
- Cache layer with TTL (1 hour?)

**Trade-off**: cost vs autonomy. A 0.5¢/decision LLM call doubles for
every tool call in autonomous workflows. Cache hit rate is the
critical metric.

**Effort**: 2 sessions. **Impact**: real autonomous agents.

---

## Track 5: Recovery recipes per error class

**State**: `runtime/recovery/` exists but only emits generic
`failed_resumable` checkpoints. Resume restarts the failing node
from scratch.

**Goal**: typed recovery strategies in
`runtime/recovery/recipes.go`:

| Error class | Recipe |
|---|---|
| `429` (rate limit) | Backoff with jitter, resume same node |
| `context_length_exceeded` | Force compact, resume same node |
| `BUDGET_EXCEEDED` | Pause for human → operator extends budget → resume |
| `TOOL_FAILED` (transient) | Re-prompt with error in context, retry |
| `TOOL_FAILED` (permanent, e.g. missing file) | Mark node failed_terminal, surface to user |

**Reference**: Rust source at
[`.works/claw-code/rust/crates/runtime/src/recovery/`](../.works/claw-code/rust/crates/runtime/src/recovery/) (~340 LOC).

**Effort**: 2 sessions.

---

## Track 6: Workflow telemetry dashboard

**State**: OTLP exporter ships `TelemetryEvent` to any collector. No
schema published for downstream tools.

**Goal**: a Grafana dashboard JSON committed at
`docs/grafana/iterion-workflow.json` that operators can import. Panels:

- cost per node (sum by `node_id` over `cost_usd`)
- tokens per model (sum by `model` over `total_tokens`)
- retry rate (count `event.type=llm_retry` / count `llm_request`)
- p50/p95/p99 node duration
- parallel branch count over time

Pair with a sample `docker-compose.yml` that boots
otel-collector + Tempo + Prometheus + Grafana for local dev. Doc walks
through the 2-command setup.

**Effort**: 1.5 sessions (mostly Grafana JSON authoring).

---

## Track 7: Plugin signature verification (sigstore)

**State**: SHA-256 verification of tarball contents against catalog
entry. Catalog itself is unsigned.

**Goal**: signed catalog using sigstore's `cosign sign-blob` flow.
Verify with `sigstore-go` library at install time. Plugin entries
gain a `signature_url` field.

**Effort**: 2 sessions. **Impact**: supply-chain hardening for
production plugin ecosystems.

---

## Track 8: Workflow editor — finish Inspector refactor

**State**: commit `0fac41b` captured a partial refactor: monolithic
`PropertiesPanel` deleted, new `Inspector*` components introduced
(`InspectorEdge`, `InspectorNode`, `InspectorMulti`,
`InspectorEditItem`, `InspectorEmpty`). Modals were deleted. UI
primitives library expanded (`Badge`, `Button`, `Chip`, `Dialog`,
`IconButton`, `Input`, `Popover`, `Select`, `Tabs`, `Textarea`,
`Tooltip`).

**Risk**: editor build state is unverified. The deleted Modals
(`EditEdgeModal`, `EditItemModal`, `EditPromptModal`, `EditSchemaModal`,
`EditVarModal`) might still be referenced from somewhere.

**First step**: `cd editor && pnpm install && pnpm typecheck` and
fix everything broken. Then walk every former Modal callsite and
ensure the new Inspector path covers it.

**Effort**: 1 session if just fixing references, 3 if completing the
UX vision (drag-to-edit, inline schema editor, etc.).

---

## Track 9: IR ↔ visual editor bidirectional editing

**Long-term play**. Currently the editor reads `.iter`, lets you
reorganize visually, but writes-back is limited. True bidirectional
editing means: drag a node → IR mutates → unparse → write `.iter`,
preserving comments and formatting.

**Blocker**: `unparse/` is lossy. Comments get dropped. Need a
formatter-aware AST that preserves original byte ranges.

**Effort**: 4-5 sessions. **Impact**: editor becomes the primary
authoring surface, not just a visualizer.

---

## Track 10: Codex sandbox profile + per-tool gating

**State**: memory note `feedback_codex_allowedtools_ineffective.md`
(2026-04) confirms Codex `AllowedTools` doesn't gate the built-in
shell; only `-c sandbox_permissions` works.

**Goal**: iterion-side wrapper that translates a workflow's
`allowed_tools: [...]` declaration into the corresponding
`-c sandbox_permissions=...` invocation when the codex backend is
selected. Per-node, not per-session.

**Risk**: Codex is `discouraged` per `CLAUDE.md`. Investing in this
is a backwards bet unless someone has a hard requirement.

**Effort**: 1 session. **Impact**: low (codex usage is declining).

---

## Order of attack — what I'd ship first

1. **Track 0** (expand RegisterClawBuiltins) — biggest unlock, lowest risk
2. **Track 1** (lifecycle hooks) — closes Phase 6, audit-grade workflows
3. **Track 2** (OAuth into MCP) — unblocks real-world MCP servers
4. **Track 5** (recovery recipes) — production reliability
5. **Track 6** (telemetry dashboard) — make all the OTLP work visible

Tracks 3, 4, 7-10 are nice-to-haves that depend on product direction.

---

## How to start a fresh session

1. `cd /workspaces/iterion && git status` and `cd .works/claw-code-go && git status` — confirm both clean
2. Check unpushed commits: `git log origin/main..HEAD --oneline` (iterion) and `git log origin/master..HEAD --oneline` (claw)
3. Read this doc, [`docs/e2e_coverage.md`](e2e_coverage.md), and [`.works/claw-code-go/docs/review_rationale.md`](../.works/claw-code-go/docs/review_rationale.md)
4. Pick a track. Open todos.
5. Run `devbox run -- task check` after every meaningful change.
6. Run the relevant `task test:live:*` after wiring anything that touches an LLM call.
