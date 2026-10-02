# DSL, backends and execution — package invariants

One entry per package, verbatim from the engine map this tree replaces.
The generated [map-packages.md](../../references/map-packages.md) lists
every package with its purpose and interfaces; these leaves carry the
invariants. `iterion map find|impact|path` answer the exact questions.

- `pkg/dsl/` — DSL pipeline (parser → AST → IR) The compiled `Workflow` — nodes, edges, schemas, prompts, vars, loops, budget — is `ir/ir.go`.

  - `parser/` — Lexer, parser, tokens, diagnostics for the .bot DSL

  - `ast/` — AST definitions and `MarshalFile`/`UnmarshalFile` (JSON encoder for AST)

  - `ir/` — Intermediate Representation compilation and validation

  - `unparse/` — IR back to .bot serialization

  - `types/` — Shared enums (transports, field types, session/router/await/interaction modes)

  - `expr/` — Expression evaluator for `compute` nodes and `when` conditions

  - `workflowfile/` — Workflow source-file loading + hash computation (used by `iterion resume` change detection)

  - `spec/` — The declarative **property registry** (a leaf): every kind's accepted properties with value shape and one-line doc. Held to the parser by a black-box conformance test in BOTH directions (a property added to one side without the other fails CI) and to the EBNF's `*_prop` productions. Feeds E012's remedy (closest name, the block a name belongs to, the kind's list — `parser.unknownProperty`, the one choke point), and renders `docs/references/dsl-properties.md`, the grammar's tables and the skills' property section (`iterion dsl spec --write` / `task dsl:gen`; `task dsl:check` fails on a stale rendering). The parser's `isKeywordToken` is derived from the lexer's keyword table for the same reason: the hand-kept copy drifted twice

- `pkg/backend/` — Execution stack (LLM + tools)

  - `model/` — Executor registry (`ClawExecutor`), schema validation, event hooks

  - `delegate/` — Backend interface and CLI delegates (`claude_code`, `codex`, `pi`, `kimi`, `grok`, `opencode`); `claw` implements the same interface in-process under `model/`

  - `tool/` — Tool registry, policies, adapters

  - `mcp/` — MCP server lifecycle, configuration, health checks

  - `recipe/` — Recipe handling for tool adapters and execution policies

  - `cost/` — Cost estimation and budgeting. Prices a call from three sources in order: claw's live registry, the spec aggregator's published pair (`modelspecs`, taken only when BOTH rates are positive — a half-published pair would price the other half at zero), then the committed static table. Zero is always *unknown*, never *free*: `Annotate` omits `_cost_usd` rather than emit a 0

  - `modelspecs/` — The dynamic model-spec registry of **ADR-042**, extracted to a LEAF package (only iterion dep: `pkg/store`) so `cost/` can read published pricing without inverting the import graph — `cost/` is a leaf precisely *because* `model/` imports it (**ADR-093**). Serves a consensus-filtered `Spec` (context window, max output, prices, three flags) per `provider/model` and per bare `model`; a field the publishers disagree on is zeroed, i.e. UNKNOWN, so the caller keeps its curated value. Supplies but does not decide — merging over the curated table stays in `model/` (`mergeSpec`). `Default()` is built lazily from the env (not at init, which would make a test's `ITERION_MODEL_SPECS_CACHE` too late); `SetDefault`/`NewSeeded` are the cross-package test seam that keeps a price assertion off the host's `~/.iterion` cache

  - `llmtypes/` — LLM SDK abstraction (`LLMTool`, `FatalToolError`, `ModelCapabilities` — carrying `ContextWindow` / `MaxOutputTokens` / `InputCostPerM` / `OutputCostPerM`, every one zero-means-unknown)

  - `detect/` — Backend credential auto-detection (OAuth, API keys, AWS/GCP) consumed by `model/executor.go`'s resolver and the studio toolbar BackendStatusPill

  - `tooldisplay/` — Human-readable rendering of tool calls for the run console / report

- `pkg/runtime/` — Workflow execution engine (branch scheduling, events, budget, recovery dispatch)

- `pkg/store/` — Run persistence (JSON-based, versioned artifacts, events.jsonl)

- `pkg/server/` — HTTP server for studio backend (embedded static UI)

- `pkg/runner/` — Cloud runner pod logic: claim a queued run, execute, report status back

- `pkg/runview/` — Read-only run console API (REST + WS) consumed by the studio SPA

- `pkg/supervise/` — LLM-driven **supervisor** agents (the `Coordinator`) that watch a running agent node from a separate goroutine/process and enqueue steering messages the run picks up at its next turn. Backs the in-`.bot` `supervisor` block and `iterion supervise` (managed runs + raw `claude` sessions). See [docs/supervisors.md](../../supervisors.md)

- `pkg/forge/` — Outbound forge-integration layer (github/gitlab/forgejo): `Connection`/`RepoIntegration`/`OAuthApp` stores (team-scoped), per-provider `Admin` clients (repos/hooks), GitHub App manifest flow + installation-token minting (least-priv, `InstallationInfo` health probe), the optional `RepoCreator` capability (create-only; GitHub Apps mint a per-call `administration:write` token, an opt-in grant at App creation), `Orchestrator` (Provision/Deprovision: webhook + hook + managed secret + bot bindings + repo-bound schedules), and the token refresh worker. The studio's **repo-first** shell (RepoSwitcher, connect wizard `/integrations/connect`, launch "Target repository" attach-or-create from a bot's manifest `repo:` block) sits on it — see [docs/repo-scope.md](../../repo-scope.md)

- `pkg/cli/` — CLI command implementations (validate, import, run, inspect, runs, resume, fork, diagram, report, studio, server, dispatch, schedule, issue, bots, skill, marketplace, memory, models, openapi, bench, bundle, sandbox, migrate, secret, plugin, remote, supervise, version)

- `pkg/benchmark/` — Metrics collection and reporting

- `pkg/botreplay/` — Record/replay golden-test framework for bots: freezes one representative LLM node interaction (the input + output maps) as a committed fixture and re-validates it against the current schema + invariants with no API calls (`task test:goldens`). See [docs/adr/008-bot-golden-replay-framework.md](../../adr/008-bot-golden-replay-framework.md)

- `pkg/log/` — Leveled logger (error, warn, info, debug, trace) — public so e2e tests can construct it

- `pkg/secure/httpdial/` — Shared SSRF guard resolving operator-supplied hosts to safe public-unicast IPs with pinned DNS; backs webhooks, OIDC issuer fetch, and the preview proxy

- `pkg/internal/` — Internal utilities (not importable outside `pkg/`)

  - `appinfo/` — Build-time version/commit injection (LDFLAGS targets)

  - `mongoutil/` — MongoDB helpers used by `pkg/cloud/` for the cloud-mode Mongo store

  - `proc/` — Process/subprocess helpers (PID management, signal handling)

- `pkg/config/` — Config-file loader (`iterion dispatch` YAML + cloud config)
