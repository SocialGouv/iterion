# Environment variables

Operational `ITERION_*` environment variables read directly by the engine
and its backends. These are tuning dials and escape hatches — most runs need
none of them. For the four launch knobs (compression, auto-memory,
permission gate, backend) and their five-level precedence chain, see
[settings-precedence.md](settings-precedence.md).

Values are read at the point of use; an unset, empty, or unparseable value
falls back to the listed default.

## Backends and models

| Variable | Effect | Default |
|---|---|---|
| `ITERION_DEFAULT_SUPERVISOR_MODEL` | Fallback `model:` compiled into agents, judges, and LLM routers that do not declare one; also the first fallback for `iterion supervise`. Backend resolution remains independent. An LLM router still empty after this uses `anthropic/claude-sonnet-5`; other nodes can use the detected backend's suggested model. | unset |
| `ITERION_VERIFIED_ACTION_MODEL` | Model spec for the [Verified Action](adr/044-adaptive-recovery-for-deterministic-action-nodes.md) recovery agent. Precedence: a node's `recovery.model` (env-expanded) → this var → package default. | package default |
| `ITERION_CONFLICT_RESOLVER_MODEL` | Model for the merge-conflict-resolver agent ([review-merge-gate.md](review-merge-gate.md)). Overrides the auto-detected pick. | auto-detected claw model |
| `ITERION_CLAUDE_CODE_MAX_TOOL_ERRORS` | Aborts a `claude_code` session after this many **consecutive** tool errors (any success resets the count) — guards against degenerate tool-error loops. `0` disables the guard. | `25` |
| `ITERION_CLAUDE_CODE_THINKING_DISPLAY` | Controls the `claude_code` thinking-block display flag: unset/other → `summarized` (readable summary); `omitted` → the CLI's latency-optimised default; `off` → stop passing the flag (required for `claude` CLIs older than the flag). | `summarized` |
| `ITERION_CLAUDE_CODE_STREAM_COLD_TIMEOUT` | How long a `claude_code` session may produce no SDK message at all — an SDK or process deadlock shows up immediately, so this tier fails fast and lets the recovery dispatcher retry instead of burning minutes on a corpse. `0` disables the tier. | `90s` |
| `ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT` | The **hot** tier: how long a session that has already produced a message may go silent. Generous because a sub-agent run commonly takes 5–10 min between visible messages. The name predates the cold/hot split and is kept for back-compat. `0` disables the tier. | `15m` |
| `ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT` | A tighter budget for one specific deadlock: the model blocked on a blocking orchestration tool (`TaskOutput` / `Monitor`) it reached **without** having spawned a `Task` first — a wait that can never make progress. A blocking call that follows a real spawn keeps the full hot budget. The error still carries "session idle for", so the node auto-re-executes on a fresh subprocess. | `4m` |
| `ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT` | How long a session may keep *talking* without *acting*. The idle tiers only see silence; they are blind to a model streaming text and thinking in circles (observed after a network outage: 20+ min of reasoning, no tool call, no commit). Only a tool_use, a tool result, or a turn's ResultMessage resets this timer. Deliberately longer than the hot tier so one slow build does not trip it. `0` disables it. | `25m` |
| `ITERION_CLAUDE_CODE_CLOSE_GRACE` | How long the `claude_code` subprocess gets to exit on its own after stdin closes, before the shutdown ladder escalates. Bounds `close()` so a hung child (the CLI keeping bash background loops alive past the agent's logical end) cannot deadlock the caller's `defer sess.Close()`. | `3s` |
| `ITERION_CLAUDE_CODE_CLOSE_TERM` | How long the same subprocess gets after `SIGTERM` before the ladder resorts to `SIGKILL`. | `1s` |
| `ITERION_CLAW_COMPACT_THRESHOLD_RATIO` | Context-window fraction (`0 < r ≤ 1`) at which the `claw` router compacts the conversation. Used only when the workflow does not set the field. | engine default |
| `ITERION_CLAW_COMPACT_PRESERVE_RECENT` | Number of most-recent messages kept verbatim when `claw` compacts. Used only when the workflow does not set the field. | engine default |
| `ITERION_PI_BIN` | Absolute path to the `pi` binary — e.g. a `bun --compile` single-file build on a host with no Node runtime. | `pi` on `PATH` |
| `ITERION_PI_MODE` | Selects the `pi` transport. The default is the long-lived `--mode rpc` session — tool events reach the studio timeline, operator chat is delivered by pi's native `steer`, accounting comes from `get_session_stats`, and a pre-flight handshake resolves the model before any token is spent. `print` rolls back to the one-shot `--mode json` path. | `rpc` |
| `ITERION_PI_STREAM_COLD_TIMEOUT` | How long a `pi` RPC session may produce no event at all before the node fails transiently. | `90s` |
| `ITERION_PI_STREAM_IDLE_TIMEOUT` | How long a started `pi` RPC session may go with no event of any kind. | `15m` |
| `ITERION_PI_NO_PROGRESS_TIMEOUT` | How long a `pi` RPC session may go without a *completed* message, tool call or compaction — catches a model looping on streaming deltas, which the idle guard cannot see. | `25m` |
| `ITERION_PI_SETTLE_GRACE` | How long to wait for `agent_settled` after aborting a `pi` RPC turn, so a partial transcript still lands. | `30s` |
| `ITERION_PI_AGENT_DIR` | Pins `PI_CODING_AGENT_DIR` for `pi`. Gives a reproducible pi config (and the only print-mode lever to disable pi's own retry loop), but **hides the operator's `~/.pi/agent/auth.json`** — and with it the OAuth provider breadth that motivates the backend. | unset (pi's own dir) |
| `ITERION_PI_OFFLINE` | `0` re-enables pi's model-catalogue refresh inside a sandbox. Off by default there because a network egress policy would stall startup on the refresh. | off under sandbox |
| `ITERION_PI_TRUST_PROJECT` | `1` trusts the **target repository's** `.pi/` extensions, skills and settings. pi executes project-local extensions as TypeScript inside the agent process, so this turns prompt injection into code execution — only for a repo you control. | refused |
| `ITERION_PI_MCP_CONNECT_TIMEOUT_MS` | How long one MCP server gets to handshake and list its tools before the `pi` extension gives up on it. Servers connect in parallel during pi's session start — which iterion's own RPC handshake is waiting on, bounded by `ITERION_PI_STREAM_COLD_TIMEOUT` (90s) — so this bounds what an unreachable server can cost: its own tools, never the run. | `10000` |
| `ITERION_PI_NO_CONTEXT_FILES` | `1` stops pi injecting the repo's `AGENTS.md` / `CLAUDE.md` into every call. On by default for `claude_code` parity, but it is the dominant per-call cost: measured at **26,933 vs 448 input tokens** on iterion's own tree (103 KB `CLAUDE.md`) for a one-word prompt. | context files loaded |
| `ITERION_FORBID_SUBSCRIPTION_OAUTH` | `1` refuses to spend a Claude Pro/Max subscription OAuth token on the `pi` and `claw` backends, which reach the API directly rather than through the vendor's CLI. Permitted by default — Anthropic accepts it, billing against a **separate extra-usage balance** rather than your plan limits, and iterion warns on each such node. Set this on a shared or cloud instance, where spending an operator's extra-usage balance is a cost decision taken for everyone. `claude_code` / `codex` are unaffected. | permitted, with a warning |
| `ITERION_CLAUDE_CODE_SETTING_SOURCES` | Comma-separated `--setting-sources` for `claude_code` nodes (`user`, `project`, `local`). The default loads the operator's user-level settings **and** the target repo's project `CLAUDE.md` / `.claude/settings.json`, so a node honours the same conventions native Claude Code would. `local` is left out on purpose: `.claude/settings.local.json` is machine-specific and can carry absolute paths that do not resolve in a sandbox. `""` or `none` disables it, restoring the CLI's headless no-settings default. | `user,project` |
| `ITERION_CLAUDE_CODE_STRICT_MCP` | `0`/`false`/`off`/`no` drops `--strict-mcp-config`, letting a `claude_code` node inherit the operator's personal `~/.claude.json` MCP servers. On by default so the node's resolved MCP set (`mcp_server:` / `mcp:` blocks, the repo's `.mcp.json`, iterion's own ask-user and board servers) is authoritative — see [backends.md](backends.md). | strict (on) |

Backend selection and provider routing use `ITERION_DEFAULT_BACKEND`,
`ITERION_BACKEND_PREFERENCE`, `ITERION_OPENAI_USE_OAUTH`, and
`ITERION_CODEX_VERSION` — documented in [backends.md](backends.md).

## Sandbox

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SANDBOX_PULL_TIMEOUT` | Caps `<runtime> pull` for the docker driver (Go duration, e.g. `20m`) so a stalled registry or blocked DNS cannot pend a run indefinitely. | `10m` |

The sandbox on/off default, cloud override, host-state mounts, and the
default image are `ITERION_SANDBOX_DEFAULT`, `ITERION_SANDBOX_OVERRIDE`,
`ITERION_SANDBOX_HOST_STATE`, and `ITERION_SANDBOX_DEFAULT_IMAGE` —
documented in [sandbox.md](sandbox.md).

## Runtime and runner

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SKIP_MCP_HEALTH` | Truthy → do not abort the run when a declared MCP server fails its startup health-check; log a warning and continue. Equivalent to the `iterion run --skip-mcp-health` flag. Useful when an HTTP/OAuth MCP server is unreachable in this environment but the run does not depend on it. | off (abort on failure) |
| `ITERION_BRANCH_CANCEL_GRACE` | Grace period (Go duration, e.g. `30s`) a cancelled fan-out branch is given to unwind before the collector stops waiting on it — raise it for backends that need longer to abort. | `5s` |
| `ITERION_GIT_AUTHOR_NAME` | Commit-author name seeded into a cloud-runner clone's local git config (no `~/.gitconfig` is mounted in the sandbox). The push-token identity is the preferred attributed path; this fires token-less. | `iterion-runner[bot]` |
| `ITERION_GIT_AUTHOR_EMAIL` | Commit-author email for the same cloud-runner clone. The default uses a reserved `.invalid` domain (RFC 2606) so the commit maps to no real account. | `iterion-runner@bot.iterion.invalid` |
| `ITERION_SHUTDOWN_DELAY` | Lame-duck window on SIGTERM: `/readyz` answers 503 for this long while the listener still accepts, so a load balancer can stop routing to the pod before its socket closes. A malformed value is a startup error, never a silent 0. See [probes-and-graceful-shutdown.md](probes-and-graceful-shutdown.md). | `5s` in **cloud** mode; `0` locally (`iterion studio`, and `iterion server` without `ITERION_MODE=cloud`, which routes to the studio) |
| `ITERION_SHUTDOWN_TEARDOWN` | What follows that window: draining in-flight runs, then letting in-flight HTTP requests finish. The ceiling on a long upload or a streamed response during a deploy. Must be > 0. | `30s` in **cloud** mode; `60s` locally |
| `ITERION_WORKTREE_POOL_MAX` | How many per-run worktrees **no live run owns** a store may park under `<store-dir>/worktrees/` before the runtime reclaims the oldest. A worktree is a full checkout of the repository, so the pool is where a long-lived store's disk goes. The bound takes only what a durable ref already holds with nothing uncommitted — never a dirty tree, never a resumable run's checkout — and warns, naming the command, when it cannot get back under. `off` disables it. See [worktree-pool.md](worktree-pool.md). | `8` |
| `ITERION_SCRATCH_RETENTION` | How long an untouched `${PROJECT_SCRATCH_DIR}` entry is kept. A run sweeps the workspace's scratch on its way out, and `iterion clean` sweeps it too; both take only entries nothing has written to for this long — **age is the concurrency guard**, because scratch is deliberately shared between runs (a subbot writes into its parent's, which is how fan-in works). `off` disables the automatic sweep. | `168h` (7 days) |
| `ITERION_RUNNER_DRAIN_MODE` | `complete` (lame-duck: finish the in-flight run before exiting) or `interrupt` (cancel + checkpoint for auto-resume elsewhere). | `complete` |
| `ITERION_RUNNER_DRAIN_TIMEOUT` | Lame-duck ceiling — the longest a runner pod waits for its in-flight run before capping it for a checkpoint-resume. | `8h` |
| `ITERION_RUNNER_SCHEMA_MISMATCH_DELAY` | Redelivery delay a runner applies when it rejects a queue message whose schema version it does not speak (mixed fleet during a rolling schema bump). Set it on server pods too: the orphan sweeper uses `MaxDeliver × max(AckWait, schema delay, epoch delay)`. The Helm chart keeps this older knob in the shared ConfigMap. See [cloud-queue-schema-rollout.md](cloud-queue-schema-rollout.md). | `30s` |
| `ITERION_RUNNER_EPOCH` | Monotonic generation shared by server and runner. Publishers stamp it on every `RunMessage`; runners accept message epochs `≤` their own and delayed-Nak future epochs before taking a lease. A process below the persistent JetStream high-water mark stays live but reports `503 superseded` and cannot publish or consume. In Helm this is a literal PodTemplate env, never a mutable ConfigMap value. | `0` (bootstrap) |
| `ITERION_RUNNER_EPOCH_MISMATCH_DELAY` | Delayed-Nak interval for a future-epoch message. Keep `delay × (MaxDeliver-1)` above the worst cold-readiness time of the replacement fleet. It contributes to the orphan sweeper's redelivery window and is rendered literally in both Helm PodTemplates. | `2m` |
| `ITERION_NODE_MAX_TRANSIENT_RETRIES` | In-executor retry budget for a **transient** backend failure (rate-limit, session-limit, idle watchdog, network/5xx), retried with exponential backoff before the failure becomes a run-level `failed_resumable`. The value is a RETRY count and excludes the initial attempt, so `8` yields 9 attempts; `0` means fail-fast. A negative or non-numeric value keeps the default rather than silently disabling retries. | `5` retries (6 attempts) |
| `ITERION_NODE_MAX_RETRIES` | The same budget for deterministic-but-retryable errors (a signal kill). Same counting rules as above. | `2` retries (3 attempts) |
| `ITERION_WORKSPACE_TRACK` | `off`/`0`/`false`/`no` disables workspace versioning globally — the content-addressed capture that backs `iterion rewind`'s file restore for runs with no isolated worktree ([workspace-versioning.md](workspace-versioning.md)). On by default: without it a rewind cannot undo what a node produced. The escape hatch exists because the cost scales with the workspace, not with the run. | on |
| `ITERION_WORKSPACE_MAX_FILE_MB` | Largest single file workspace versioning will capture. A file over the bound is **reported** (`files.overwritten` / `files.left_in_place`), not silently lost — but reporting is not restoring, so raise it for a media pipeline whose deliverable is the artefact a rewind most needs back. A non-numeric or non-positive value keeps the default. | `32` (MiB) |
| `ITERION_AUTO_MEMORY` | Env level of the `auto_memory:` precedence chain (`--auto-memory` → node → workflow → this → default). Off by default so a run is hermetic — see [memory-and-knowledge.md](memory-and-knowledge.md). | `off` |
| `ITERION_LOOP_BUDGET_GUARD` | Env level of the `loop_budget_guard:` chain (`--loop-budget-guard` → workflow → this → default). Declines a loop back-edge the remaining budget cannot fund, so the run leaves through its own exit path instead of dying mid-pass on `BUDGET_EXCEEDED` — see [dsl.md](dsl.md#budget-and-loop-back-edges). | `on` |
| `ITERION_BUDGET_EXIT_GRACE` | How far past a *spent* cap a run may walk **forward** to reach a terminal node, as a fraction of the declared cap — so work already paid for gets delivered instead of dying on disk. Accepts a ratio in `[0,1]`; `off`/`no`/`false`/`none`/`0` make every declared cap **absolute** (the setting for shared instances and pooled credentials). Fails **closed**: an out-of-range or unparsable value is treated as `0` with a one-time stderr warning, never as the permissive default. The grace is refused outright when the loop budget guard is off, and on a cap clamped by an outside authority (platform ceiling, credential-pool donor allowance). Each graced node emits a `budget_exit_grace` event — see [dsl.md](dsl.md#budget-and-loop-back-edges). | `0.1` (10%) |
| `ITERION_REPO_DEVBOX` | Env level of the `repo_devbox:` chain (`--repo-devbox` → workflow → this → default). `off` skips the **target repo's** `devbox.json`; the bot's own is always installed. Worth turning off for a run that reads a repo without building it — see [dsl.md](dsl.md#the-target-repos-toolchain--repo_devbox). | `on` |

## Run alerts

The run observer (`pkg/alert`) watches runtime events plus a per-run
liveness heartbeat and fires on stall, budget warning/exceeded, and
failure. These variables configure where those alerts go; they are read
by both `iterion studio` and the cloud server. Distinct from the
user-addressed web-push notifications of
[notifications.md](notifications.md), which are per-recipient rather
than per-deployment.

| Variable | Effect | Default |
|---|---|---|
| `ITERION_ALERTS_WEBHOOK_URL` | Generic incoming webhook (Slack / Discord) the alert sink posts to. Empty disables the sink. It is an **operator-set** destination posted to with a plain 15s-timeout client — unlike the operator-*supplied* completion webhooks of `pkg/notify`, it carries no SSRF guard, so point it only at a URL you control. | unset (no webhook sink) |
| `ITERION_ALERTS_STALL_TIMEOUT` | No-activity window after which a non-terminal run is flagged **stalled** (Go duration). An unparseable value keeps the default rather than disabling the check. | `5m` |
| `ITERION_ALERTS_BASE_URL` | Origin used to build clickable `/runs/<id>` deep links in webhook payloads. When unset it is derived from the bind address + port; with an OS-assigned (`0`) port the absolute base is left empty, since a wrong link is worse than none. | derived from bind + port |
| `ITERION_ALERTS_DESKTOP_ENABLED` | `true` turns on the native desktop-notification sink. Parsed strictly — anything unparseable is `false`. | `false` |

## Platform budget ceiling (cloud)

The multitenant safeguard: a **hard, tenant-unraisable** cap on every run a
runner pod executes. Set on the **runner** Deployment. Each variable is
independent — set only the dimensions you want to bound; leaving one unset
means "no platform limit on that axis", and setting none at all is a no-op,
so a self-hosted or single-tenant deployment keeps its `.bot` budgets
verbatim.

The ceiling is applied *after* the launch overrides, and it only ever
**lowers**: a tenant cannot raise it with `--max-cost-usd`, and a bot that
declares no budget at all **inherits** the ceiling rather than running
unlimited — which is what bounds an `as X(unbounded)` loop, whose fuel falls
back to `max_iterations` ([dsl-totality-and-tc.md](dsl-totality-and-tc.md)).
A clamp that actually changes a limit marks the budget **imposed**, so the
runtime's [exit grace](dsl.md#budget-and-loop-back-edges) is refused and the
figure is absolute.

A value that is empty, non-numeric, or ≤ 0 is treated as *unset* (no ceiling
on that dimension), not as zero.

| Variable | Effect | Default |
|---|---|---|
| `ITERION_CLOUD_MAX_ITERATIONS` | Ceiling on the workflow's `max_iterations`. | unset (no ceiling) |
| `ITERION_CLOUD_MAX_TOKENS` | Ceiling on `max_tokens`. | unset |
| `ITERION_CLOUD_MAX_COST_USD` | Ceiling on `max_cost_usd`, in dollars. | unset |
| `ITERION_CLOUD_MAX_DURATION` | Ceiling on `max_duration` (Go duration, e.g. `4h`). Compared by parsed seconds; a workflow value that is unparseable or absent is replaced by the ceiling. Unlike the numeric dials this one is not validated at read time: any non-empty string is accepted, and one that is not a Go duration clamps **nothing** on this axis — silently, with no log line, since the runner only logs a clamp that changed something. | unset |
| `ITERION_CLOUD_MAX_PARALLEL_BRANCHES` | Ceiling on `max_parallel_branches`. | unset |
| `ITERION_CLOUD_RETRY_MAX_ATTEMPTS` | Ceiling on the resolved retry policy's `max_attempts`, applied last so a tenant cannot reserve a pod for a hundred attempts — see [scheduling.md](scheduling.md). | unset |
| `ITERION_CLOUD_RETRY_MAX_WAIT` | Ceiling on the resolved retry policy's `max_wait` (Go duration). | unset |

This is the *per-run* bound. The *per-org* monthly cost cap, run quota,
concurrency and launch rate are a separate admission layer with its own
variables — see [quotas-and-limits.md](quotas-and-limits.md).

## See also

- [probes-and-graceful-shutdown.md](probes-and-graceful-shutdown.md) — what the probe endpoints promise and how the delays compose.
- [settings-precedence.md](settings-precedence.md) — compression / permission / backend precedence.
- [backends.md](backends.md) — backend, provider, and OAuth-forfait variables.
- [sandbox.md](sandbox.md) — sandbox default, override, and host-state variables.
- [notifications.md](notifications.md) — `ITERION_WEBPUSH_VAPID_{PUBLIC,PRIVATE}_KEY`; the user-addressed counterpart to the deployment-wide `ITERION_ALERTS_*` above.
- [worktree-pool.md](worktree-pool.md) — the worktree pool bound and `ITERION_WORKTREE_POOL_MAX`.
- [usage-caps.md](usage-caps.md) — `ITERION_USAGE_CAP`, `ITERION_USAGE_CAP_5H_{MODE,PCT}`, `ITERION_USAGE_CAP_WEEK_{MODE,PCT}`.
- [scheduling.md](scheduling.md#retry--a-provider-quota-window-is-waited-out-not-re-attempted) — the retry policy's machine defaults (`ITERION_RETRY_*`) and the platform ceiling (`ITERION_CLOUD_RETRY_*`).
- [web-search.md](web-search.md) — `ITERION_WEB_SEARCH` and the search-tier resolver.
- [secrets.md](secrets.md) — `ITERION_SECRETS_KEY` and the redaction variables.
- [observability.md](observability.md) — `ITERION_LOG_FORMAT`, `ITERION_LOG_LEVEL`, and the `SENTRY_*` variables.
