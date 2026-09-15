# Backends and execution — who runs the prompt, with which tools

Backend selection and fallbacks, system-prompt composition (the adaptivity
parity that makes an iterion node behave like its native harness), the plugin
/ compression chain, the sandbox, and the prompt-engineering dials.

Full references: [../backends.md](../backends.md), [../plugins.md](../plugins.md),
[../sandbox.md](../sandbox.md), [../supervisors.md](../supervisors.md).

## Backends, tools and isolation

### Backend selection

Six backends are wired:
- `claw` (in-process; automatic or explicit, and the last-resort fallback) — recommended for direct provider calls and Iterion-native declared tools. Use any provider model claw supports, e.g. `model: "openai/gpt-5.4-mini"` or `model: "anthropic/claude-opus-5"`. A `model:` pin alone does not select it; use `backend: "claw"` when the direct path is required.
- `claude_code` — recommended coding-agent CLI for implementation work and rich tool/shell access (implementers, fixers).
- `pi` — the [pi coding agent](https://pi.dev) driven through the generic CLI-agent seam (ADR-065 + **ADR-085**, [pkg/backend/delegate/pi.go](../../pkg/backend/delegate/pi.go)): `backend: "pi"`, prompt on stdin (pi's argv parser drops a message starting with `-`/`@`), `--append-system-prompt` via a file, `--provider`/`--model` emitted together, `--thinking` for effort. Drives a long-lived **`--mode rpc`** session by default (`ITERION_PI_MODE=print` rolls back to one-shot): the only CLI backend where tool events reach the studio timeline, operator chat rides native `steer`, and accounting comes from `get_session_stats`. Reach for it to run a model the other backends cannot: ~36 first-class providers, plus a **provider-computed USD cost** (`cost.AnnotateWithUSD`) instead of an estimate. Its wire types are a pinned port of pi's own exported client surface in [pkg/backend/delegate/pisdk/](../../pkg/backend/delegate/pisdk/) (the `claudesdk/` precedent). **Capability gaps that are easy to miss:** `__ITERION_SECRET_*__` placeholders are not materialised (file secrets work); `ultracode` degrades to `xhigh`. iterion refuses the target repo's `.pi/` extensions by default (they execute as TypeScript inside the agent process). An embedded TypeScript extension ([pi-extension/](../../pi-extension/), bundled into [pkg/backend/delegate/piext/asset/](../../pkg/backend/delegate/piext/asset/)) supplies what pi has no native surface for (RPC transport only): **iterion's permission gate** — resolving through the same `permission.Policy` as claude_code/claw — **`ask_user`** plus the async pair (`ask_user_async`/`await_answers`, ADR-081; answers delivered mid-run via native `steer`), and a hand-rolled **MCP client** on all three transports (streamable http, legacy sse, stdio), since pi ships none. That client is what makes board `capabilities:` and workflow `mcp_server:` blocks reach a pi node; tools are discovered via `tools/list`, never hardcoded. Bridging happens inside pi's `session_start`, which iterion's own RPC handshake waits on, so each server is bounded by `ITERION_PI_MCP_CONNECT_TIMEOUT_MS` (default 10s) and connects in parallel — an unreachable server costs its own tools, never the run. `task pi-ext:check` fails if the committed asset is stale. **Cost gotcha:** pi injects the repo's `AGENTS.md`+`CLAUDE.md` on every call — measured at 26,933 vs 448 input tokens on this repo for a one-word prompt; `ITERION_PI_NO_CONTEXT_FILES=1` turns it off.
- `codex` — supported Codex CLI backend (`pkg/backend/delegate/codex.go`), selected explicitly per node/workflow or by adding it to `ITERION_BACKEND_PREFERENCE`. It uses the Codex CLI login, including ChatGPT OAuth, and supports images, reasoning effort, sessions and structured output. Its native tool set cannot currently be narrowed through Iterion's `tools:` list (`AllowedTools`/`CanUseTool` do not gate the built-in shell), and the pinned SDK cannot run inside Iterion's outer Docker/Kubernetes sandbox; document and test those capability boundaries when extending it.
- `kimi` — Moonshot's kimi-code CLI driven through the generic CLI-agent seam (ADR-065, [pkg/backend/delegate/kimi.go](../../pkg/backend/delegate/kimi.go)): `backend: "kimi"`, prompt via `-p`, stream-json output, model alias passed through verbatim (e.g. `model: "kimi-code/kimi-for-coding"`); credentials are resolved by the CLI itself from its own env/config. Sessions are best-effort — resume/fork are not wired for CLI-agent backends, so each node runs fresh.
- `grok` — xAI Grok Build CLI driven through the same CLI-agent seam (ADR-065, [pkg/backend/delegate/grok.go](../../pkg/backend/delegate/grok.go)): `backend: "grok"`, prompt via `-p`, `--output-format json`, `system:` via `--rules` (append — never override the native agentic baseline), model via `-m` (optional `xai/` prefix stripped), `reasoning_effort` via `--reasoning-effort`; headless tool approval forced with `--permission-mode bypassPermissions --always-approve`. Credentials come from the CLI itself (Grok Build login / `~/.grok`). Distinct from the metered xAI HTTP API path (`backend: claw` + `model: "xai/…"`).

**Cross-backend fallback (`fallbacks:`).** A node may declare ordered,
named alternative **routes** (backend + model + credential hint) taken
when its primary fails — the case `provider:` cannot serve: a CLI
backend whose subscription forfait has shut, continuing on a metered API
through `claw`. `on:` filters which failure routes where (default
`[usage_window, unavailable]`; never `any`, never `auth` by default). A
fall-through is deliberately **loud**: a `model_fallback` event,
`_backend`/`_model` naming what actually *served*, and
`_fallback_used`/`_served_by` so a deterministic gate can fail closed on
a degraded input. THREE crossings are compile-time errors (C176): a route
that cannot enforce the node's `permission:` gate; a claw⇄CLI
crossing on a node with an empty `tools:` list (the list inverts meaning
across that boundary); and a **backend change on a node that keeps its
conversation** (`session: inherit` / `inherit_if_available` / `fork` —
`sessionContinuityCrossingReason`), since session continuity has no
cross-backend meaning. The third is why a node that must survive a
fall-through WITHOUT losing its thread builds its ladder from different
*providers* inside ONE backend rather than from different backends —
Copi's claw ladder is the shipped example. Two route properties extend the chain (ADR-091):
`action: skip` is a TERMINAL degrade — the node completes with a
zero-value output stamped `_skipped` instead of failing the run (the
"continue and ignore" half of an optional-peer policy; "pause and
retry" = don't declare it, the failure stays resumable for the
usage-window retry) — and `when:` gates any route on an expr over vars,
so one node expresses both policies picked per run by a `--var`. C173
guards both. See
[ADR-087](../adr/087-cross-backend-model-fallback-chain.md) +
[ADR-091](../adr/091-fallback-skip-route-and-plan-peer-review.md) +
[docs/backends.md](../backends.md).

**Auto-detection.** When neither the node (`backend:`) nor the workflow (`default_backend:`) names a backend, and `ITERION_DEFAULT_BACKEND` is unset, the resolver in [pkg/backend/model/executor.go:resolveBackendName](../../pkg/backend/model/executor.go) probes the host for credentials (Claude Code OAuth, ANTHROPIC_API_KEY, OPENAI_API_KEY, AWS, GCP) and picks the first match in `ITERION_BACKEND_PREFERENCE` (default `claude_code,claw`; other CLI backends, including codex, are explicit opt-ins). When `model:` is also empty and the resolved backend is `claw`, the runtime substitutes a sensible model spec for the first available provider. The studio surfaces the live detection via the toolbar BackendStatusPill and disables Run when no credential is found. See [docs/backends.md](../backends.md).

**System-prompt composition (adaptivity parity).** A node's `system:`
prompt is the *task*, never the whole operating posture. How it composes
with the agentic baseline differs by backend, and getting this wrong is
exactly what made iterion-via-Claude-Code feel dumber than native Claude
Code:
- **claude_code** — iterion passes the assembled prompt via
  `--append-system-prompt`, **never** `--system-prompt`. Replacing would
  strip Claude Code's native system prompt (TodoWrite/plan-before-act/
  read-before-edit/parallel-tool/`file:line`/refusal posture); appending
  keeps it as the base. iterion also emits `--setting-sources user,project`
  so the target repo's `CLAUDE.md`/settings are honoured (tunable via
  `ITERION_CLAUDE_CODE_SETTING_SOURCES`). MCP is the opposite —
  `--strict-mcp-config` makes the node's resolved MCP set (`mcp_server:`/
  `mcp:` blocks, repo `.mcp.json`, iterion's ask_user/board servers)
  authoritative: the operator's personal `~/.claude.json` servers never
  boot inside a bot node (`ITERION_CLAUDE_CODE_STRICT_MCP=0` restores
  inheritance). Tool restriction: under the
  always-on `--permission-mode bypassPermissions`, `--allowedTools` does
  **not** gate the toolset — claude_code nodes always have the full native
  toolset (a node's lowercase `tools:` list is a no-op here; the real
  hard-restrict flag is `--tools`, deliberately unused to preserve
  adaptivity).
- **claw** — claw-code-go is a bare API client with **no** native system
  prompt, so iterion prepends an authored `agenticOperatingPosture` base
  (the parity substrate) before the node's `system:` text. A node's
  `tools:` list **does** restrict claw (lowercase names are claw-native)
  — and is RESOLVED against the registry, so a name claw does not have
  fails the node at dispatch. `iterion validate` refuses it first (C135,
  claw only — on a CLI backend the list is inert, so an unknown name
  there is dead config, not a failure); the catalog of accepted names is
  [pkg/backend/toolcatalog](../../pkg/backend/toolcatalog/toolcatalog.go), kept
  honest by a conformance test against the real registry. `list_files` /
  `run_command` / `git_diff` / `search_codebase` circulate in older
  examples and have never been registered — use `glob` / `bash` / `grep`.
  Exact Claude spellings `Read`/`Bash`/`Grep` have a manifest-gated Claw alias
  tier after MCP shorthand; see [docs/tool-name-aliases.md](../tool-name-aliases.md)
  for the engine floor, precedence and compatibility probe (unreleased floor
  must be finalized before merge).

The `bypassPermissions` note above describes the default (`permission:
off`). The opt-in **permission gate** (`permission: ask|deny`, see the
DSL section + [docs/permissions.md](../permissions.md)) adds a
deterministic allow/deny/ask boundary on top — without changing
`--permission-mode` (under bypass, PreToolUse hooks still run and a hook
`deny` still blocks the tool, so the gate rides the existing hook
surface). It is the anti-prompt-injection counterpart that keeps a
hypnotized/injected agent from silently performing off-policy actions.

The mechanism is `delegate.SystemPromptMode` (Standalone | AppendToNative
| AuthoredBase), set per-backend by `SystemPromptModeForBackend`
([pkg/backend/delegate/delegate.go](../../pkg/backend/delegate/delegate.go)).
This restores adaptivity **without** touching the convergence machinery —
the `agenticOperatingPosture` "converge and stop / don't re-litigate"
clause reinforces the asymptote, it does not gate it.

**OpenAI ChatGPT-forfait via claw.** When Codex CLI is signed in via "Sign in with ChatGPT" (`auth_mode: "chatgpt"` in `~/.codex/auth.json`), `claw` can reuse that OAuth token + account_id to drive OpenAI calls through `chatgpt.com/backend-api/codex` — billing against the user's ChatGPT Plus/Pro subscription instead of metered API calls. Precedence: `OPENAI_API_KEY` wins when both are present (explicit env var = deliberate); ChatGPT-OAuth activates when no API key is set, or when `ITERION_OPENAI_USE_OAUTH=1` forces it. `ITERION_OPENAI_USE_OAUTH=0` or any `OPENAI_BASE_URL` disables OAuth. The `version:` header (which OpenAI uses to gate model availability — e.g. gpt-5.5 requires codex-cli ≥ 0.130) is sourced from `ITERION_CODEX_VERSION` or `codex --version`. See the "OpenAI via ChatGPT forfait" section in [docs/backends.md](../backends.md). The **Anthropic** subscription equivalent also works on `claw` (and `pi`) — set `ANTHROPIC_AUTH_TOKEN` with no `ANTHROPIC_API_KEY` — but Anthropic bills third-party clients against the subscription's separate **extra-usage** balance, not the plan's limits (verified 2026-07-28; supersedes an older note claiming the path was throttled to zero). iterion warns per node and `ITERION_FORBID_SUBSCRIPTION_OAUTH=1` refuses it — worth setting on a shared/cloud instance, where spending an operator's extra-usage balance is a decision taken for everyone. See **ADR-085**.

### Plugins (rewriters, MCP, skills, lifecycle) + command-output compression

Iterion has a **plugin ecosystem**: declarative, out-of-process packages
(`plugin.yaml`) with typed `contributes:` kinds — `rewriters` (command-output
compressors), `mcp_servers` (e.g. knowledge-graph explorers), `skills` /
`commands` / `agents` (markdown mirrored into `.claude/{skills,commands,agents}/`,
discovered by claude_code via `--setting-sources project`), `hooks` (JSON
fragments idempotently merged into `.claude/settings.json`), and
`lifecycle` (index/refresh). Builtins are embedded
([pkg/plugin/builtin/](../../pkg/plugin/builtin/)); `rtk` ships **enabled**,
`graphify` + `repo-falcon` + `firecrawl` (web search/scrape MCP —
[docs/web-search.md](../web-search.md)) ship **disabled**. Installed plugins live under
`~/.iterion/plugins/<name>/`, enable state in `~/.iterion/plugins.yaml`. Manage
with `iterion plugin list|info|enable|disable|run|install|uninstall`. The plugin
system never injects Go code (static `CGO_ENABLED=0` binaries rule out Go
`plugin`); it wires manifests into existing seams (rewrite chain, MCP catalog,
skill mirroring). Marketplace entries carry a `kind` (`bot`|`plugin`) so both
share one registry. Public skill libraries install ergonomically: `iterion
plugin install <git-url>` of a bare `skills/` repo (no `plugin.yaml`)
synthesizes a skills-only manifest. Full reference + the roadmap toward the full
Claude plugin taxonomy (commands/agents/hooks) with claude_code⇄claw parity
(improve claw in `.works/claw-code-go`): [docs/plugins.md](../plugins.md).

**Command-output compression** is the `rewriter` kind, generalized from the old
hardcoded rtk integration. `rtk` ("Rust Token Killer",
[github](https://github.com/rtk-ai/rtk)) is the default-enabled rewriter: it
rewrites an agent's shell command to its token-compressed equivalent (`git
status` → `rtk git status`), saving 60–90% of command-output tokens, on all
three shell surfaces — the **claude_code** Bash PreToolUse hook, the **claw**
bash builtin, and **tool nodes** (node-level opt-in ONLY, so a review loop's
`git diff` stays full-fidelity). The DSL field is **`compress:`**
(`on|ultra|off`) on the `workflow` block and `agent`/`judge`/`tool` nodes; CLI
flag **`--compress`**; env **`ITERION_COMPRESS`**. Precedence: CLI → node →
workflow → env → **default**. The default is opt-OUT for agent/judge nodes
(**on** when a rewriter plugin is enabled + its binary present, so rtk is used
out of the box) and opt-IN for tool nodes (off unless the node sets
`compress:`). Disable per-run (`--compress off` / studio toggle) or globally
(`iterion plugin disable rtk` → chain empty → off; or `ITERION_COMPRESS=off`).
Enabled rewriter plugins form an ordered **chain** so you can replace rtk or
stack several compressors. iterion uses rewriters strictly as
compressors, never permission gates (failures fall back to the original
command). Sandboxed runs bind-mount each rewriter's host binary at its declared
`sandbox_mount` (rtk → `/usr/local/bin/rtk`). Diagnostic `C102` flags an invalid
`compress:` value.

### Sandbox

Per-run container isolation is **on by default**: at product entry points (`iterion run`/`resume`, studio, dispatcher) a workflow with no `sandbox:` block runs as `sandbox: auto` (reads `.devcontainer/devcontainer.json`, falling back to a published `iterion-sandbox-slim:<version>` image), with graceful degradation when the host can't sandbox (outside a git repo, or no container runtime → visible `sandbox_skipped` event). Workflows can still pin block-form inline configuration (`sandbox:` with `image:` or `build:`) or explicitly opt out via `sandbox: none` — discouraged and flagged by the C128 warning. `ITERION_SANDBOX_DEFAULT=none` restores the historical opt-in behaviour machine-wide; the cloud runner was long assumed to pin `ITERION_SANDBOX_OVERRIDE=none` (the runner pod being the isolation boundary), but the production deployment measured on 2026-08-05 does NOT: its config carries `ITERION_SANDBOX_DEFAULT=auto` with an EMPTY override, so cloud runs DO get the k8s sandbox. Anything that needs a bind-mounted workspace there — auto-memory, for one — must declare `sandbox: none`. When active, claw, claude_code, pi, Kimi, Grok, and tool nodes execute against a long-lived container that bind-mounts the worktree — by default at the host workspace's absolute path so Claude Code project keys match in/out container. Codex is the exception: its pinned SDK refuses Iterion's outer sandbox because it cannot route through the command builder. Network egress is **unrestricted by default** (`network: open`, since 2026-05-22 — no proxy is started). Opting into `network: allowlist` (or `denylist`) starts an HTTP CONNECT proxy on the host that enforces the policy; the built-in `iterion-default` preset covers LLM endpoints + npm/pypi/golang + github/gitlab/bitbucket + Nix cache. Sandboxed `claw` calls are routed through the hidden `iterion __claw-runner` subprocess inside the container, so the `iterion` binary must be present on the container PATH (or bind-mounted by the host when available).

By default the sandbox also auto-mounts `~/.iterion/` (run store) and `~/.claude/` (Claude Code OAuth + per-project sessions) at the same absolute path inside the container so persistent memory survives across runs. On Linux, when the spec doesn't pin a `User`, the docker driver runs the container as the host UID:GID so writes back to those mounted trees stay host-owned. Disable via `sandbox.host_state: none` in the DSL, `--sandbox-host-state=none`, or `ITERION_SANDBOX_HOST_STATE=none` — recommended for multi-tenant cloud runners that must not leak host OAuth credentials. The kubernetes driver hard-errors on `host_state: auto` (cloud pods have no host filesystem to bind). See [docs/sandbox.md](../sandbox.md) for the full reference (incl. the published `iterion-sandbox-slim`/`iterion-sandbox-full` variants, the `--sandbox-default-image` override, and the host-state mount details) and `iterion sandbox doctor` for host diagnostics.

Each **setup phase** between "container up" and "first node" carries its own bound, because an unbounded one does not fail — it waits, holding the run's queue lease with no `sandbox_started` event until `max_duration` fires hours later: pod-Ready (`ITERION_SANDBOX_K8S_POD_READY_TIMEOUT`, 10m), workspace copy + git fixup (`ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT`, 15m), `post_create` (`ITERION_SANDBOX_POST_CREATE_TIMEOUT`, 30m — an install outlasts a copy). An expiry parks the run `failed_resumable` + `SANDBOX_SETUP_TIMEOUT`, checkpoint intact, on a launch AND on a resume (a resume rebuilds the sandbox from scratch), and the cloud runner re-offers it to a fresh pod after 2 min. See [docs/sandbox.md](../sandbox.md#setup-phases-and-their-timeouts) + [docs/resume.md](../resume.md#what-a-resume-rebuilds).

V2-6 wires `sandbox.build:` via `docker buildx build` on the local docker driver — BuildKit lives inside the Docker daemon, so no extra service. The kubernetes driver rejects `sandbox.build:` by design; cloud workflows reference pre-built images via `sandbox.image:` with a CI-built digest (production path). See [docs/sandbox.md](../sandbox.md#buildkit-local-docker-only--v2-6).

### Key Interfaces

- `NodeExecutor` (`pkg/runtime/engine.go`) — `Execute(ctx, node, input) → (output, error)`, abstraction between engine and execution backend
- `ClawExecutor` (`pkg/backend/model/executor.go`) — production `NodeExecutor` impl, dispatches through `delegate.Backend` to `claw`, `claude_code`, `codex`, `pi`, `kimi`, or `grok`; direct generation used by human nodes calls `pkg/backend/model/generation.go` (`GenerateTextDirect` / `GenerateObjectDirect`).
- `Backend` (`pkg/backend/delegate/delegate.go`) — common execution-backend interface. `claude_code`, `pi`, `kimi`, `grok`, and `codex` shell out to their CLIs; `claw` (`pkg/backend/model/claw_backend.go`) calls claw-code-go in-process through the generation engine.
- `RunStore` (`pkg/store/store.go`) — file-backed persistence for runs, events, artifacts, interactions
- `Workflow` (`pkg/dsl/ir/ir.go`) — compiled execution unit with Nodes, Edges, Schemas, Prompts, Vars, Loops, Budget

### Cursors (prompt-engineering dials)

`cursor <name>:` is a top-level declaration alongside `prompt:` /
`schema:`. Each cursor defines either an enum (`values:`) or a
numeric band map (`bands:`) over `[0.0, 1.0]`, with each entry
carrying a prompt fragment. Agent/judge nodes activate cursors via
a `cursors:` block (`enabled` toggle + per-cursor settings), and
the runtime appends the resolved fragments under a `## Calibration`
section in the system prompt. Diagnostics: `C083` (unknown cursor
reference, warning), `C084` (invalid value, error), `C085`
(malformed declaration, error), `C086` (duplicate name, error).
Resolution honours `${VAR}` substitution; the assembled prompt is
sorted alphabetically by cursor name for prompt-cache stability.

Cursors are framing dials, **not gates**. See
[docs/cursors.md](../cursors.md) for the full contract — Goodhart
resistance still lives in judges, scanners, and deterministic
coverage gates. Reference catalogue:
[examples/cursors/cursors.bot](../../examples/cursors/cursors.bot)
ships `ambition` / `depth` / `rigor` / `autonomy`.

### Supervisors (`supervisor <name>:`)

A **supervisor** is an LLM agent that watches another running agent and
enqueues steering messages the supervised agent picks up **at its next
turn** — like a human watching a Claude Code session and typing a
correction. It is **node-scoped**: it watches one or more *agent nodes*
(`watches: [implement]`), is armed only while a watched node is active,
and its injected messages are node-tagged
(`store.QueuedUserMessage.NodeID` + `WithMessageNode`) so a late message
can't leak into the next node. It is a top-level **declaration**, not a
graph node — the engine spawns a concurrent
[pkg/supervise](../../pkg/supervise/coordinator.go) `Coordinator` (shaped like
`watch_coordinator`) at run start and tears it down when the run ends.
The coordinator wakes the bot on debounced turn boundaries (cooldown) and
on **monitor** matches (event patterns the bot registers — Bash failure,
edit to a path, cost threshold); the bot returns a structured `Decision`
(intervene/message/watch/done) via `GenerateObjectDirect`. Injection
reuses `runview.Service.QueueMessage`, so steering shows in the studio
conversation and is delivered by the same inbox-drain hooks as operator
chat. Three surfaces: the in-`.bot` `supervisor <name>:` block (primary,
auto-spawned; diagnostics C190–C193), `iterion supervise --run-id --node
--system --monitor` (attach to an already-running iterion run), and
`iterion supervise --claude-session <cwd>` (attach to a **raw** `claude`
CLI/VSCode session — iterion tails its
`~/.claude/projects/<key>/<sessionId>.jsonl` transcript and steers via a
`Stop`/`PostToolUse` hook, installed by `iterion supervise install-hook`,
that runs the hidden `iterion __claude-hook-drain` to inject from an
inbox under `~/.iterion/claude-sessions/<key>/`). The transcript tailer
is an `Observer` and the inbox an `Injector`, so the same Coordinator/bot
drive both managed and raw targets. The block may pre-seed `monitors:`
(CLI `--monitor` grammar, armed from the first event — the bot-registered
kind only exists after its first eval). Declared supervisors spawn by
default on every launch surface (CLI run/resume, studio/runview, the
dispatcher's direct engine path, cloud runner pods), with the usual
escape hatch: run-level `--supervisors on|off` / launch-API field →
`ITERION_SUPERVISORS` → on (skip always logged; the resolution lives in
`pkg/supervise`, shared by every spawn site) — and like `auto_memory:`
the run-level override travels onto the cloud queue
(`RunMessage.supervisors`, schema v8) so a pod never re-decides an
operator's `off`. The supervisor hub rides BOTH event seams (engine
observer + backend-hook `ExecutorSpec.EventObservers`) — hook events
(`assistant_text`, `tool_*`) never fire the engine seam, and text
monitors are blind without the second wire. Persy (perseverance coach)
is the shipped use — carried by all seven campaign bots on their
`campaign` node, feature-dev's being the reference and
`bots/campaign_supervisor_test.go` the fleet guard. Reference:
[docs/supervisors.md](../supervisors.md),
[examples/supervisor/sample.bot](../../examples/supervisor/sample.bot).

### Ultracode (`reasoning_effort: ultracode`)

`ultracode` is the top of the `reasoning_effort` dial
(`low|medium|high|xhigh|max|ultracode`) but is a **mode, not a wire
value**: Anthropic's API only accepts up to `xhigh`/`max`. It means
**xhigh + a standing prerogative to orchestrate multi-agent
workflows**, delivered via a `## Workflow Orchestration` system-prompt
section, and is **reliable only on `claude-opus-4-8`** (the
orchestration half rides Anthropic mid-conversation system messages,
4.8-only). The runtime remaps `ultracode` to `xhigh` on the wire
([model.wireEffort](../../pkg/backend/model/effort.go)), makes the `agent`
subagent tool available, and emits diagnostic **C089** (warning) when
the node's model isn't 4.8 — degrading to plain `xhigh`. Adaptive
thinking is auto-enabled for 4.8 by the claw backend. The studio
effort picker only offers `ultracode` on 4.8. Full contract:
[docs/ultracode.md](../ultracode.md).

