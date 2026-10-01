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
| `ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT` | The **hot** tier: how long a session that has already produced a message may go silent. Generous because a sub-agent run commonly takes 5–10 min between visible messages. It also bounds the Bash timeouts every `claude_code` spawn pins (`BASH_MAX_TIMEOUT_MS` sits under the tighter of this tier and the no-progress tier, by a margin; see [backends.md](backends.md#claude_code)): a foreground command is silent while it runs. The name predates the cold/hot split and is kept for back-compat. `0` disables the tier. | `15m` |
| `ITERION_CLAUDE_CODE_ORCH_STALL_TIMEOUT` | A tighter budget for one specific deadlock: the model blocked on a blocking orchestration tool (`TaskOutput` / `Monitor`) it reached with **no background work to wait on** — none running or being launched, per the CLI's own task list (with the background lifecycle off, on a CLI older than 2.1.280, or before the CLI reported any background task: no subagent ever spawned — `Agent`, or `Task` on older CLIs — and no `run_in_background` command ever started). A blocking call that follows real background work keeps the full hot budget. Once classified, the stall is recovered **in place** first (next row); only a failed recovery aborts the session, with an error that still carries "session idle for" so the node auto-re-executes on a fresh subprocess. Every classification lands as a `delegate_stall` event (outcome `recovered` or `aborted`) and on the runner's `iterion_delegate_idle_deadlock_total{backend,model,outcome}` counter. | `4m` |
| `ITERION_CLAUDE_CODE_ORCH_RECOVERY_TIMEOUT` | The in-place recovery of that deadlock: the session is interrupted (the CLI's control-protocol interrupt aborts the tool call it is blocked on and closes the turn), then told which wait could never return, and continues on the same session — one tier cheaper than the node restart a kill costs (re-clone, re-provision, replay). This bounds how long the turn may take to close after the interrupt; past it, or when the model blocks the same way again, the session is aborted for retry. `0` disables the recovery (the stall aborts immediately). | `30s` |
| `ITERION_CLAUDE_CODE_DISALLOW_ORCHESTRATION_TOOLS` | Opt-in: withhold the single-subagent surface (`Agent`, `Task`, `TaskOutput`, `Monitor`, via `--disallowedTools`) from `claude_code` nodes that are not in `ultracode` mode, for a deployment whose served model family hallucinates task ids and deadlocks on `TaskOutput`. The tools no headless session can use — `Workflow`, `ScheduleWakeup`, `CronCreate`/`CronDelete`/`CronList`, `RemoteTrigger` — are withheld from every node regardless, and subagents run in the foreground (every spawn pins `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, unless `ITERION_CLAUDE_CODE_BACKGROUND_TASKS=on`; see [backends.md](backends.md#claude_code)). Off by default: a claude_code node keeps its full native toolset, subagents included. `ultracode` nodes keep the surface regardless — on every spawn, so this knob decides nothing for them. For a non-`ultracode` node it decides the main spawn; what it never decides is the structured-output pass of a node with `permission: ask`/`deny`, which withholds that surface and `Workflow` whatever this is set to, because that spawn carries no permission hook and nothing there can run the policy. | unset (off) |
| `ITERION_CLAUDE_CODE_BACKGROUND_TASKS` | Opt-in to background work on `claude_code` nodes: `on` pins the CLI's `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS` empty (in the process environment and the `--settings` flag layer) instead of `1`, so subagents and shells may run while the main agent works, and replaces the "subagents run in the foreground" section of the system prompt with one saying what the session waits for. The background lifecycle (next rows) keeps the session open until background subagents come back (and workflows, a task type no spawn can start: `Workflow` stays withheld); a background shell is not waited for, and the section says to run one in the foreground. With the lifecycle off, background work dies with the session at its first result — warned, and the section then tells the model to pass `run_in_background: false` on every Agent call. Whatever the lifecycle, every spawn pins the CLI's own wind-down ceiling (`CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS`) at the CLI's own default — past it the CLI terminates the work it holds, so no iterion knob shortens it and the repository under review cannot move it. | unset (off: foreground) |
| `ITERION_CLAUDE_CODE_BACKGROUND_LIFECYCLE` | Kill switch of the lifecycle that keeps a `claude_code` session open for the background subagents and workflows it launched instead of killing them at the first result — see [backends.md](backends.md). `off` restores the previous behaviour (end at the first result; the orchestration-stall guard's historical rule; the CLI is asked for no session state events nor message replays); lost work is reported, and recorded in the run's session ledger, either way, and the tool-error breaker counts per agent either way. The lifecycle engages only when the CLI reports background work, and only on a CLI ≥ 2.1.280. | `on` |
| `ITERION_CLAUDE_CODE_BACKGROUND_WAIT` | Budget of one wave of background work — from the first result that left a subagent or workflow running, or from held work coming back without a turn — before iterion asks the agent to report with what it has (at the next idle point, never mid-turn); only a report written after a turn took that request is kept. A wave ends once all of it came back: the next launch starts a budget of its own. Also the ceiling on the time a turn source runs (a `Monitor` call's watch, an MCP or WebSocket monitor, a teammate) past the node's first turn, summed over the session: past it, a close where one runs, or where one ended since the previous deciding close without an agent stopping it, asks for the report. The silence and no-progress watchdogs rest while the agent waits by design. `0` = unbounded (the run's `max_duration` still applies). | `30m` |
| `ITERION_CLAUDE_CODE_BACKGROUND_AUTOTURN_GRACE` | How long a CLI that is neither in a turn nor at rest (no idle, or one iterion cannot take as rest yet) may stay so before iterion acts: one nudge if a background result may still be owed, then the session ends with the last turn's report — or, once a turn ran while a turn source (a monitor, a teammate) was running, asks for the report first — the tasks that kept the CLI from idle (work iterion does not wait for: an MCP task, a remote agent, a teammate, a timed monitor) named and reported lost. A nudge no turn ever takes ends the call on a retryable error. It also bounds the turns the CLI keeps running on its own once no subagent or workflow runs (a monitor's events): counted from the first close with nothing held running — those turns do not restart it; held work starting or coming back, one of the agent's background tasks ending, or a nudge, does — at the close of the first one past it, unless a held result is still on its way, iterion asks for the report (or ends on that turn's when no turn source ever prompted one and no held result's delivery is unproven). `0` = no grace: the silence watchdog (`ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT`) bounds the first case; the second, the ceiling while a turn source runs (`ITERION_CLAUDE_CODE_BACKGROUND_WAIT`) and the node's `tool_max_steps`. | `90s` |
| `ITERION_CLAUDE_CODE_BACKGROUND_IDLE_SETTLE` | How long the CLI's idle (`session_state_changed`) must hold before the session ends on it: the CLI reports idle an instant before a turn it re-kicks for a command already queued. `0` ends on the first idle — a node may then keep a report that predates a delivery; warned once per session. | `1s` |
| `ITERION_CLAUDE_CODE_BACKGROUND_ANSWER_WAIT` | How long the CLI may take to ANSWER a message iterion wrote (the delivery nudge above) before the delivery is given up on and the call ends on a retryable error. A different wait from the grace, which decides WHEN to nudge: one grace has to cover the CLI's replay of the message and the first assistant message of the turn that took it, and a provider backoff in between spends it — the session then answers with the text that predates the nudge, losing work the CLI had delivered. The budget is read on a grace tick, so its resolution is one grace, and the silence watchdogs are suspended while a nudge is outstanding — this is how long a CLI that took the message and went quiet holds the node. | 5 × `…_AUTOTURN_GRACE` (`7m30s`) |
| `ITERION_CLAUDE_CODE_BACKGROUND_RESULT_WAIT` | How long a held result on its way to the CLI's queue — its end reported with no idle since (the CLI queues a subagent's result only once its worktree is finalised), or its task gone from the CLI's set before its notification — is still waited for by the grace before it is recorded lost: the CLI waits that long for it itself. No nudge meanwhile — none could deliver it. A bound spent at rest (`tool_max_steps`, the ceiling) does not wait: it asks for the report. `0` = the grace alone. | `5m` |
| `ITERION_CLAUDE_CODE_BACKGROUND_FINALIZE_TIMEOUT` | Bound of the wrap-up turn iterion asked for when a budget was spent, from the moment a turn took it (the CLI's replay of the message); past it the call ends on a retryable error, and the unfinished tasks are reported and recorded in the session ledger. `0` = no bound of its own: the silence watchdog (`ITERION_CLAUDE_CODE_STREAM_IDLE_TIMEOUT`) governs the wrap-up turn. | `10m` |
| `ITERION_CLAUDE_CODE_NO_PROGRESS_TIMEOUT` | How long a session may keep *talking* without *acting*. The idle tiers only see silence; they are blind to a model streaming text and thinking in circles (observed after a network outage: 20+ min of reasoning, no tool call, no commit). Only a tool_use, a tool result, or a turn's ResultMessage resets this timer. Deliberately longer than the hot tier so one slow build does not trip it. With the hot tier it bounds the Bash timeouts every `claude_code` spawn pins. `0` disables it. | `25m` |
| `ITERION_CLAUDE_CODE_CLOSE_GRACE` | How long the `claude_code` subprocess gets to exit on its own after stdin closes, before the shutdown ladder escalates. Bounds `close()` so a hung child (the CLI keeping bash background loops alive past the agent's logical end) cannot deadlock the caller's `defer sess.Close()`. | `3s` |
| `ITERION_CLAUDE_CODE_CLOSE_TERM` | How long the same subprocess gets after `SIGTERM` before the ladder resorts to `SIGKILL`. | `1s` |
| `ITERION_CLAW_COMPACT_THRESHOLD_RATIO` | Context-window fraction (`0 < r ≤ 1`) at which the `claw` router compacts the conversation. Used only when the workflow does not set the field. | engine default |
| `ITERION_CLAW_COMPACT_PRESERVE_RECENT` | Number of most-recent messages kept verbatim when `claw` compacts. Used only when the workflow does not set the field. | engine default |
| `ITERION_PI_BIN` | The `pi` binary on the host — e.g. a `bun --compile` single-file build on a host with no Node runtime. An **absolute path** or a **bare name** on PATH; a relative path with a separator is **refused with a typed error** (it would resolve against the workspace and run a binary out of the checkout). Both pi transports apply the same rule. | `pi` on `PATH` |
| `ITERION_PI_MODE` | Selects the `pi` transport. The default is the long-lived `--mode rpc` session — tool events reach the studio timeline, operator chat is delivered by pi's native `steer`, accounting comes from `get_session_stats`, and a pre-flight handshake resolves the model before any token is spent. `print` rolls back to the one-shot `--mode json` path. | `rpc` |
| `ITERION_PI_STREAM_COLD_TIMEOUT` | How long a `pi` RPC session may produce no event at all before the node fails transiently. | `90s` |
| `ITERION_PI_STREAM_IDLE_TIMEOUT` | How long a started `pi` RPC session may go with no event of any kind. | `15m` |
| `ITERION_PI_NO_PROGRESS_TIMEOUT` | How long a `pi` RPC session may go without a *completed* message, tool call or compaction — catches a model looping on streaming deltas, which the idle guard cannot see. | `25m` |
| `ITERION_PI_SETTLE_GRACE` | How long to wait for `agent_settled` after aborting a `pi` RPC turn, so a partial transcript still lands. | `30s` |
| `ITERION_PI_AGENT_DIR` | Pins `PI_CODING_AGENT_DIR` for `pi`. Gives a reproducible pi config (and the only print-mode lever to disable pi's own retry loop), but **hides the operator's `~/.pi/agent/auth.json`** — and with it the OAuth provider breadth that motivates the backend. | unset (pi's own dir) |
| `ITERION_PI_OFFLINE` | `0` re-enables pi's model-catalogue refresh inside a sandbox. Off by default there because a network egress policy would stall startup on the refresh. | off under sandbox |
| `ITERION_OPENCODE_BIN` | The opencode CLI on the **host**, for a host whose PATH the iterion process does not share. An **absolute path** or a **bare name** on PATH; a relative path with a separator is **refused with a typed error** (it would resolve against the workspace and run a binary out of the checkout). Detection and execution apply the same rule. Ignored inside a sandbox, where a host path means nothing. | PATH lookup |
| `ITERION_OPENCODE_TRUST_PROJECT` | `1` trusts the **target repository's** `.opencode/` resources (and an `opencode.json` at any level up to the git root). opencode loads and executes what it finds there inside the agent process, so this turns prompt injection into code execution — only for a repo you control. Process-wide: on a shared server it lifts the refusal for every concurrent run. | refused |
| `ITERION_PI_TRUST_PROJECT` | `1` trusts the **target repository's** `.pi/` extensions, skills and settings. pi executes project-local extensions as TypeScript inside the agent process, so this turns prompt injection into code execution — only for a repo you control. | refused |
| `ITERION_PI_MCP_CONNECT_TIMEOUT_MS` | How long one MCP server gets to handshake and list its tools before the `pi` extension gives up on it. Servers connect in parallel during pi's session start — which iterion's own RPC handshake is waiting on, bounded by `ITERION_PI_STREAM_COLD_TIMEOUT` (90s) — so this bounds what an unreachable server can cost: its own tools, never the run. | `10000` |
| `ITERION_PI_NO_CONTEXT_FILES` | `1` is the raw off switch for pi's context files (`AGENTS.md` / `CLAUDE.md`, one per directory up to `/` plus the agent dir's), whatever the node's `ambient_context:` says — a cost lever: measured at **26,933 vs 448 input tokens** on iterion's own tree (103 KB `CLAUDE.md`) for a one-word prompt. Without it, the ambient-context policy decides ([ADR-119](adr/119-ambient-context-policy.md)): every policy but `all` already turns pi's own loading off, and iterion supplies the files the policy allows. | unset (the policy decides) |>>>>>>> 025ea9449 (wip(ambient): S7 docs, studio select, maps (local checkpoint))
| `ITERION_FORBID_SUBSCRIPTION_OAUTH` | `1` refuses to spend a Claude Pro/Max subscription OAuth token on the `pi` and `claw` backends, which reach the API directly rather than through the vendor's CLI. Permitted by default — Anthropic accepts it, billing against a **separate extra-usage balance** rather than your plan limits, and iterion warns on each such node. Set this on a shared or cloud instance, where spending an operator's extra-usage balance is a cost decision taken for everyone. `claude_code` / `codex` are unaffected. | permitted, with a warning |
| `ITERION_CLAUDE_CODE_SETTING_SOURCES` | Comma-separated `--setting-sources` for `claude_code` nodes (`user`, `project`, `local`). **Raw override**: when set it replaces the node's `ambient_context:` translation (ADR-119) for every claude_code spawn, logged once; the routing pin holds whatever the scopes. `none`/`""` now loads NO source at all (an explicit `--setting-sources ""`): omitting the flag would make the CLI load every source, `local` included. Unknown tokens are logged and dropped — a list made only of typos loads nothing. Unset, the policy decides: `workspace` by default, which keeps the project scope that carries the engine's mirrored skills and plugin contributions, adds the repository's memory, and keeps the operator's user scope out. | unset (the policy decides) |
| `ITERION_CLAW_SLASH_COMMANDS` | `off`/`0`/`false` stops `claw` nodes resolving a user prompt that opens with `/<name>` against `<workspace>/.claude/commands/<name>.md`, sending the raw text instead. On by default, so a command a plugin contributes (`contributes: commands`) reaches a `claw` node the way `--setting-sources project` gives it to `claude_code` — see [backends.md](backends.md#workspace-slash-commands). Read on the HOST, in the executor, so it holds for a sandboxed run too. Other backends are unaffected: `claude_code` resolves these natively, the CLI agents have no such convention. | on |
| `ITERION_CLAW_SLASH_COMMAND_MAX_BYTES` | Ceiling, in bytes, checked on a workspace command's body **and again on the text it expands to** — a body under the ceiling can still amplify past it, since `$ARGUMENTS` repeated N times multiplies the arguments N-fold. On a review run the workspace is a checkout the run does not control, so that file is untrusted input that becomes a **billed** request; over the ceiling the node sends its prompt unchanged and logs the file, its size and this variable — an abstention, never a truncation, because half a command body is an instruction nobody wrote. `0` removes the ceiling; a value that does not parse is ignored, so a typo cannot silently remove the bound. | `262144` (256 KiB) |
| `ITERION_CLAUDE_CODE_STRICT_MCP` | `0`/`false`/`off`/`no` drops `--strict-mcp-config`, letting a `claude_code` node inherit the operator's personal `~/.claude.json` MCP servers. On by default so the node's resolved MCP set (`mcp_server:` / `mcp:` blocks, the repo's `.mcp.json`, iterion's own ask-user and board servers) is authoritative — see [backends.md](backends.md). | strict (on) |

Backend selection and provider routing use `ITERION_DEFAULT_BACKEND`,
`ITERION_BACKEND_PREFERENCE`, `ITERION_OPENAI_USE_OAUTH`, and
`ITERION_CODEX_VERSION` — documented in [backends.md](backends.md).

## Sandbox

| Variable | Effect | Default |
|---|---|---|
| `ITERION_SANDBOX_PULL_TIMEOUT` | Caps `<runtime> pull` for the docker driver (Go duration, e.g. `20m`) so a stalled registry or blocked DNS cannot pend a run indefinitely. | `10m` |
| `ITERION_SANDBOX_POST_CREATE_TIMEOUT` | Budget of the `post_create` snippet, on **both** drivers — the budget belongs to the phase, so raising it for a slow toolchain install raises it wherever the phase runs. Fails closed: a value that is not a positive Go duration is refused, the default applies, and one stderr line names the variable, the value and the default. | `30m` |
| `ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT` | Budget of the kubernetes driver's workspace copy AND of the git fixup that follows, each end-to-end. Same fail-closed parsing. | `15m` |
| `ITERION_SANDBOX_K8S_APPLY_TIMEOUT` | Budget of ONE `kubectl` control call on the kubernetes driver — the per-run Secret, the CA Secret, the pod, the NetworkPolicy, the mid-run secret refresh, and every `delete` (stale-pod eviction, rollbacks, cleanup). Enforced by killing the process; never passed as kubectl's own `--request-timeout`, which makes kubectl v1.36 discard the in-cluster configuration and dial `localhost:8080`. Same fail-closed parsing. | `2m` |

Every setup phase and how its expiry is classified: [sandbox.md § setup
phases](sandbox.md#setup-phases-and-their-timeouts). The pod-Ready wait
has its own knob, `ITERION_SANDBOX_K8S_POD_READY_TIMEOUT`, documented
there with the rest of the kubernetes scheduling family.

The sandbox on/off default, cloud override, host-state mounts, and the
default image are `ITERION_SANDBOX_DEFAULT`, `ITERION_SANDBOX_OVERRIDE`,
`ITERION_SANDBOX_HOST_STATE`, and `ITERION_SANDBOX_DEFAULT_IMAGE` —
documented in [sandbox.md](sandbox.md).

## MCP catalog and the launcher

| Variable | Effect | Default |
|---|---|---|
| `ITERION_MCP_AUTOLOAD` | `0`/`false` stops iterion reading a `.mcp.json` next to the `.bot` (CLI, studio) or at the root of the cloned repository (cloud runner). Those entries are `project`-origin: under an active sandbox the launcher does not start them anyway — this removes them from the catalog entirely. | on |
| `ITERION_MCP_HEALTHCHECK` | `0`/`false` skips the pre-run health check of the workflow's active MCP servers. The check connects — for a stdio server, spawns — so it only ever probes servers the launcher may start; the rest are skipped and logged. | on |
| `ITERION_MCP_CACHE_TTL` | Lifetime of the on-disk tool-discovery cache (`0` disables it). The cache stores tool names and schemas only, keyed by a hash of the server's configuration. | `1h` |
| `ITERION_MCP_EXPAND_UNTRUSTED_ENV` | `true` restores the pre-#1945 behaviour: a **workflow-controlled** server's `command`/`args`/`url` expand `${VAR}` against the launcher's own environment. Off by default — that expansion reads the operator's shell (or the cloud runner pod's credentials) on behalf of a definition the target repository controls, and the expanded value travels into the container as the CLI backends' MCP config. `plugin`-origin servers are unaffected. | off |

Which servers the launcher starts at all is decided by their ORIGIN — see
[sandbox.md § MCP servers under a sandbox](sandbox.md#mcp-servers-under-a-sandbox).
Note that `ITERION_PLUGINS_ENABLE`, `ITERION_HOME` and
`ITERION_PLUGIN_<NAME>_<KEY>` are honoured wherever they come from, but only
speak for the OPERATOR when the process inherited them: a value a project
`.env` filled in still applies and still does not confer the operator's
authority.

`ITERION_PLUGINS_DISABLE` is read live and applied whatever its source, with
no provenance distinction — deliberately: disabling only ever removes a
capability, so there is no authority to lose. A project `.env` can therefore
silence a plugin the operator enabled in their own `plugins.yaml`.

`ITERION_MCP_EXPAND_UNTRUSTED_ENV` is read **only** from the inherited
environment, so a value a project `.env` planted does not turn the hatch on
**through this variable**: it is an operator's consent to read the launcher's
environment on behalf of a definition the repository controls, and a
repository cannot consent on the operator's behalf. Setting it in a `.env` is
reported as such in the diagnostic that names the missing variables. It is
not the only door, though — the expansion also follows the SANDBOX (an
unsandboxed run expands, because nothing crosses into a container), and
`ITERION_SANDBOX_DEFAULT` / `ITERION_SANDBOX_OVERRIDE` are read live, so a
`.env` setting either to `none` makes the run unsandboxed and the expansion
happens. Closing that family is tracked separately.
Note that an operator-installed plugin's stdio server inherits the whole
environment of the launcher process: that is the operator's own binary and
the operator's choice, but it is worth knowing before enabling one in a pod
that holds platform credentials (`ITERION_PLUGINS_ENABLE`).

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
| `ITERION_AMBIENT_CONTEXT` | Env level of the `ambient_context:` precedence chain (`--ambient-context` / launch `ambient_context` → node → workflow → this → `workspace`). Which instruction files agent/judge nodes inherit besides their prompt: `workspace` (the repository's, the default), `operator` (the operator's setup), `all`, `none` — ADR-119. An invalid value is ignored, logged once, and the run falls through to the workflow or the default. Enforced on `claude_code`, `claw`, `codex` and `pi`. | `workspace` |
| `ITERION_LOOP_BUDGET_GUARD` | Env level of the `loop_budget_guard:` chain (`--loop-budget-guard` → workflow → this → default). Declines a loop back-edge the remaining budget cannot fund, so the run leaves through its own exit path instead of dying mid-pass on `BUDGET_EXCEEDED` — see [dsl.md](dsl.md#budget-and-loop-back-edges). | `on` |
| `ITERION_BUDGET_EXIT_GRACE` | How far past a *spent* cap a run may walk **forward** to reach a terminal node, as a fraction of the declared cap — so work already paid for gets delivered instead of dying on disk. Accepts a ratio in `[0,1]`; `off`/`no`/`false`/`none`/`0` make every declared cap **absolute** (the setting for shared instances and pooled credentials). Fails **closed**: an out-of-range or unparsable value is treated as `0` with a one-time stderr warning, never as the permissive default. The grace is refused outright when the loop budget guard is off, and on a cap clamped by an outside authority (platform ceiling, credential-pool donor allowance). Each graced node emits a `budget_exit_grace` event — see [dsl.md](dsl.md#budget-and-loop-back-edges). | `0.1` (10%) |
| `ITERION_REPO_DEVBOX` | Env level of the `repo_devbox:` chain (`--repo-devbox` → workflow → this → default). `off` skips the **target repo's** `devbox.json`; the bot's own is always installed. Worth turning off for a run that reads a repo without building it — see [dsl.md](dsl.md#the-target-repos-toolchain--repo_devbox). | `on` |
| `ITERION_WEBHOOK_SYNC_DEBOUNCE` | Quiet window a **synchronize** (push-to-PR) review launch waits out, so a push volley costs one review of the final head instead of N−1 runs cancelled mid-flight. A Go duration; `0` disables the debounce and every push launches immediately — the kill switch for the whole deferred lane. An unparsable value keeps the default with a stderr warning (failing open would silently restore the waste it exists to cut). PR open, `/revi` and a re-request click are never debounced. See [webhooks.md](webhooks.md). | `3m` |

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
| `ITERION_CLOUD_MAX_DURATION` | Ceiling on `max_duration` (Go duration, e.g. `4h`). Compared by parsed seconds; a workflow value that is unparseable, absent, zero or negative (all unlimited to the runtime) is replaced by the ceiling, and a value kept under it is frozen as judged — a stored bot var changed later does not reach the run. A zero or negative ceiling clamps nothing. Unlike the numeric dials this one is not validated at read time: any non-empty string is accepted, and one that is not a Go duration clamps **nothing** on this axis — silently, with no log line, since the runner only logs a clamp that changed something. | unset |
| `ITERION_CLOUD_MAX_PARALLEL_BRANCHES` | Ceiling on `max_parallel_branches`. | unset |
| `ITERION_CLOUD_RETRY_MAX_ATTEMPTS` | Ceiling on the resolved retry policy's `max_attempts`, applied last so a tenant cannot reserve a pod for a hundred attempts — see [scheduling.md](scheduling.md). | unset |
| `ITERION_CLOUD_RETRY_MAX_WAIT` | Ceiling on the resolved retry policy's `max_wait` (Go duration). | unset |

This is the *per-run* bound. The *per-org* monthly cost cap, run quota,
concurrency and launch rate are a separate admission layer with its own
variables — see [quotas-and-limits.md](quotas-and-limits.md).

## Merge gate (server)

The cadence of the [merge-gate sweep](merge-gate.md#two-triggers-because-one-event-is-not-a-guarantee),
the net that re-offers dead gating runs to the reconciler. It runs on one
elected server replica, and every offer it makes spends the forge's request
budget, so these are the levers that slow it down without a release. Set on
the **server** Deployment; read at start. A value that breaks the net keeps its
default and warns in the server log, naming the variable:

- a value that does not parse, or is not positive;
- an interval under 1 s, or of half the 8-day horizon (96 h) or more;
- a lookback that does not exceed the interval plus the sweep's 3-minute grace
  (a run could end between two windows and never be examined), or that reaches
  the horizon;
- deep passes half the horizon apart or more (interval × deep-every of 96 h or
  more).

| Variable | Effect | Default |
|---|---|---|
| `ITERION_GATE_SWEEP_INTERVAL` | Time between two passes (Go duration). The sweeper's lease is paced by it, capped at the default: its TTL is three intervals, three minutes at most, so a long interval does not delay the failover. A new term's first pass runs at once. | `1m` |
| `ITERION_GATE_SWEEP_LOOKBACK` | How far back an ordinary pass reaches (Go duration). The ordinary publish grant outlives it by 30 minutes. | `1h` |
| `ITERION_GATE_SWEEP_DEEP_EVERY` | Passes between two deep ones, which reach the whole 8-day horizon. | `30` |
## Platform environment isolation (cloud)

A cloud server or runner keeps its own credentials out of what the workflows
it executes can read. Once booted, it removes them from its process
environment — `ITERION_SECRETS_KEY`, `ITERION_JWT_SECRET`, the Mongo, NATS,
Redis and S3 credentials, the SMTP password, the forge GitHub App key and
client secret, the OIDC client secrets, the VAPID private key, the bootstrap
admin password, the completion webhook secret, the alerts webhook URL, the
SMTP username, the bootstrap admin address, the error-tracker DSN and the OTLP
header blocks — and marks itself non-dumpable, so a child process cannot read
its boot environment from `/proc` either. The tracker and the OTLP exporter
read their configuration while booting, well before the scrub. Provider keys
stay: runs spend them.

Workflow text — a `${NAME}` in a `.bot`, a launch value, a tool command, a
node's `backend:`/`model:`/`provider:` — reads no credential-shaped name from
a cloud process's environment (the definition the bot-vars settings use:
TOKEN, KEY, SECRET, PASSWORD, CREDENTIAL, AUTH, PRIVATE, a `HEADER(S)` or
`PROXY` segment and the like), nor any name the scrub removes: in the DSL such
a reference reads as unset, and a tool command's `${NAME}` is left as written,
for the command's own shell. Local runs are unaffected.

| Variable | Effect | Default |
|---|---|---|
| `ITERION_CLOUD_ENV_PASSTHROUGH` | Comma-separated credential-shaped names that workflow text may still read from a cloud process's environment — for a deployment whose teams are its operators. | unset |
| `ITERION_CLOUD_SCHEDULE_GUARDS` | `allow` lets a cloud schedule or trigger carry a `guard:`, which the server runs as a shell command in its own pod. Otherwise the field is refused at write (422) and a guard stored earlier does not run: its tick is recorded `guard_error` and the reason is raised on the schedule's own `last_error`. | unset |

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
