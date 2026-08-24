# Environment variables

Operational `ITERION_*` environment variables read directly by the engine
and its backends. These are tuning dials and escape hatches — most runs need
none of them. For the three launch knobs (compression, permission gate,
backend) and their five-level precedence chain, see
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
| `ITERION_PI_MCP_CONNECT_TIMEOUT_MS` | How long one MCP server gets to handshake and list its tools before the `pi` extension gives up on it. Servers connect in parallel during pi's session start — which iterion's own 30s RPC handshake is waiting on — so this bounds what an unreachable server can cost: its own tools, never the run. | `10000` |
| `ITERION_PI_NO_CONTEXT_FILES` | `1` stops pi injecting the repo's `AGENTS.md` / `CLAUDE.md` into every call. On by default for `claude_code` parity, but it is the dominant per-call cost: measured at **26,933 vs 448 input tokens** on iterion's own tree (103 KB `CLAUDE.md`) for a one-word prompt. | context files loaded |
| `ITERION_FORBID_SUBSCRIPTION_OAUTH` | `1` refuses to spend a Claude Pro/Max subscription OAuth token on the `pi` and `claw` backends, which reach the API directly rather than through the vendor's CLI. Permitted by default — Anthropic accepts it, billing against a **separate extra-usage balance** rather than your plan limits, and iterion warns on each such node. Set this on a shared or cloud instance, where spending an operator's extra-usage balance is a cost decision taken for everyone. `claude_code` / `codex` are unaffected. | permitted, with a warning |

Backend selection and provider routing use `ITERION_DEFAULT_BACKEND`,
`ITERION_BACKEND_PREFERENCE`, `ITERION_OPENAI_USE_OAUTH`, and
`ITERION_CODEX_VERSION` — documented in [backends.md](backends.md).

## Sandbox

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SANDBOX_PULL_TIMEOUT` | Caps `<runtime> pull` for the docker driver (Go duration, e.g. `20m`) so a stalled registry or blocked DNS cannot pend a run indefinitely. | `10m` |

The sandbox on/off default, cloud override, host-state mounts, and the
default image are `ITERION_SANDBOX_DEFAULT`, `ITERION_SANDBOX_OVERRIDE`, and
`ITERION_SANDBOX_HOST_STATE` — documented in [sandbox.md](sandbox.md).

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
| `ITERION_RUNNER_SCHEMA_MISMATCH_DELAY` | Redelivery delay a runner applies when it rejects a queue message whose schema version it does not speak (mixed fleet during a rolling schema bump). Must cover a rolling restart of the runner Deployment — raise it on fleets with a slow lame-duck turnover. See [cloud-queue-schema-rollout.md](cloud-queue-schema-rollout.md). | `30s` |

## See also

- [probes-and-graceful-shutdown.md](probes-and-graceful-shutdown.md) — what the probe endpoints promise and how the delays compose.
- [settings-precedence.md](settings-precedence.md) — compression / permission / backend precedence.
- [backends.md](backends.md) — backend, provider, and OAuth-forfait variables.
- [sandbox.md](sandbox.md) — sandbox default, override, and host-state variables.
- [notifications.md](notifications.md) — `ITERION_WEBPUSH_VAPID_{PUBLIC,PRIVATE}_KEY`.
- [worktree-pool.md](worktree-pool.md) — the worktree pool bound and `ITERION_WORKTREE_POOL_MAX`.
