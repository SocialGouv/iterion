# Supervisor agents

A **supervisor** is an LLM agent that watches another running agent and
enqueues steering messages the supervised agent picks up **at its next
turn** — exactly like a human operator watching a Claude Code session in
the studio and typing a quick correction mid-work. The supervisor runs
*concurrently* and reacts to the supervised run's live activity.

Supervisors are **node-scoped**: one supervisor watches one or more
*agent nodes* (e.g. `implement`, `fix`), not necessarily the whole run.
It is *armed* only while one of its watched nodes is the active node, and
every message it injects is tagged with that node so a late message can't
leak into the next node. Watching the whole run is the degenerate case
(no node filter).

## How it works

```
supervised run ──events──▶ Coordinator ──wake──▶ Supervisor bot (LLM)
                               │                        │
                     monitors + cooldown          decision: enqueue
                               │                        │
                               └──── QueueMessage ◀──────┘
                                        │
                          drained at the supervised node's NEXT turn
```

The coordinator (`pkg/supervise`) subscribes to the run's event stream
(`runview.Service.ObserveRun`), tracks the active node, and wakes the
supervisor bot on:

- **turn boundaries** (`llm_step_finished` / `node_finished` /
  `node_started` / `run_paused`), debounced and rate-limited by a
  cooldown, and
- **monitor matches** — event patterns the bot registers interest in
  (a Bash failure, an edit to a path, a cost threshold), which fire
  immediately, bypassing the cooldown.

On each wake the bot returns a structured **decision**: whether to
`intervene` now (and with what `message`), which event patterns to keep
`watch`-ing, and whether it is `done`. When it intervenes, the message is
enqueued via the same `runview.Service.QueueMessage` path operator chat
uses — so it is delivered at the next turn boundary (the claude_code
inbox-drain hooks / the claw `InboxDrain` closure), shows in the studio
run conversation, and is **node-scoped** (`store.QueuedUserMessage.NodeID`).

A hard `max_evals` budget and the cooldown keep token cost bounded;
supervision degrades to a silent no-op when the budget is exhausted —
it never eats the supervised run's budget.

## Declaring a supervisor in a `.bot` (primary path)

Declare the supervisor inline in the workflow it watches — a top-level
`supervisor <name>:` block, alongside `cursor`/`schema`/`prompt`. It is
**not a graph node**: the engine spawns it concurrently at run start and
arms it only while a watched node is active. Multiple supervisors are
allowed (each watching a different node set).

```
supervisor watchdog:
  watches: [implement, fix]            # agent node(s) to steer (omit = whole run)
  model: "anthropic/claude-opus-5"  # optional pin; resolution below
  system: watchdog_policy              # a prompt: ref — the supervision policy
  cooldown: "45s"                      # min between turn-boundary evals (default 30s)
  max_evals: 12                        # hard eval cap (default 20)
  monitors: ["event_type=tool_error,tool_name=Bash"]  # pre-seeded patterns (CLI --monitor grammar)

prompt watchdog_policy:
  Intervene only if the implementer edits files outside src/, or a Bash
  test fails twice in a row. Keep messages short and actionable.
```

**Model resolution** (unpinned supervisors): `model:` pin →
`ITERION_DEFAULT_SUPERVISOR_MODEL` → **the provider family the watched
nodes themselves run on** (their `provider:` routing, a `provider/`
model prefix, or the backend's family — claude_code → anthropic,
codex → openai), honoured when that provider is detected in the env OR
funded by the run's own ctx credentials (a per-provider API key, or the
codex ChatGPT forfait for openai; the anthropic OAuth forfait does NOT
count — it is usable only by the claude_code CLI, never by claw) → the
detector's first available provider. The family
preference is what keeps the coach on the credential the run already
proved working: without it, a dead key sitting first in the host
environment (a platform OPENAI key with no credits, on the prod runner
pods) 429-ed every eval while the supervised campaign ran fine on
Anthropic.

`monitors:` pre-seeds event patterns at coordinator construction, armed
as soon as a watched node becomes active (immediately, for a supervisor
with no `watches:`) — the supervisor bot can still register more at
runtime, but anything it registers only exists after its first eval (a
measured blind window when the marker you care about appears early).
Each entry uses the CLI `--monitor` grammar (`key=val,key=val`); a
malformed entry gets a `C191` warning at validate and is dropped at
spawn. A value cannot contain a comma — split into two monitors.

Semantics that matter when choosing the set:
- **Pin `text_contains` to an event type** (usually
  `event_type=assistant_text`, the agent's own words). An unpinned
  substring is matched against EVERY rendered event — tool inputs and
  outputs included — so a `grep` hit in the repo or a Read of a file
  mentioning the word wakes an eval.
- **Pre-seeded matches honour the `cooldown`** — a match inside the
  window is DEFERRED to the cooldown's expiry (the trigger event rides
  the wake reason, so it survives even if evicted from the recent
  ring), never dropped; if the watched node finishes first, the
  deferred wake re-arms when the node comes back (the next pass of a
  loop). One deferral slot: further suppressed matches in the same
  window coalesce into it. Only monitors the bot registers itself (via
  its decision's `watch`) bypass the cooldown, and re-listing a seeded
  pattern does not promote it. A seeded match also never resurrects a
  supervisor whose bot declared `done` — re-arming is the job of a
  bot-registered monitor or a fresh watched `node_started`.
- **The coordinator is one goroutine**: an eval in flight blocks its
  own wake processing (never the run — the hub drops rather than
  stalls), so under a very short `cooldown` the supervisor lags the
  run by up to one eval. Invisible at the shipped cooldowns.
- **A failed evaluation does not consume `max_evals`** — but three
  consecutive failures (no reachable model, auth) park supervision with
  a logged warning instead of burning the budget on transport errors.
- **Budget events are node-scoped like everything else**: a supervisor
  watching `campaign` never sees a `budget_warning` raised while
  another node (a verifier, a reviewer) is the active one.
- The supervisor's own steering messages (the `user_message_*` event
  family) are never matched — an intervention cannot re-trigger its
  author.
- On **resume**, the catch-up replay of pre-attach history is
  observational: it reconstructs state but never fires monitors or
  evals; supervision acts on activity after the attach.

Launch the workflow normally (`iterion run`); the supervisor is
auto-spawned, observes the run, and is torn down when the run ends. The
`watches:` ids must name agent or judge nodes (a warning `C190` fires
otherwise — both kinds execute through the same model executor and pick
up steering at their next turn),
and `system:` must reference a declared prompt (`C193`). Monitors aren't
declared in the DSL — the supervisor bot registers the patterns it cares
about at runtime; use the CLI `--monitor` flag to pre-seed them when
attaching externally.

### Disabling declared supervisors (kill switch)

Declared supervisors spawn by default. The escape hatch, resolved
run-level override → env → on:

- `iterion run --supervisors off` (and `iterion resume --supervisors
  off` — like the other run-level overrides it is NOT persisted on the
  run, so a launch-time `off` must be re-stated on resume);
- the launch API field `supervisors` /
  `runview.LaunchSpec.Supervisors` / `ResumeSpec.Supervisors` for
  programmatic launches — forwarded to detached runners as
  `--supervisors` AND onto the cloud queue (`RunMessage.supervisors`,
  schema v8), so a pod never re-decides an operator's `off` from its
  own env. The studio Launch modal does not expose it yet;
- `ITERION_SUPERVISORS=off` machine-wide (the layer below the run-level
  override).

The skip is loud (a `supervisors: N declared supervisor(s) disabled by
…` warning) — a declared capability never disappears in silence. Use it
for cost control, or to isolate a supervisor suspected of steering a
run astray.

## The perseverance-coach pattern

The first shipped use of the DSL block is **Persy**, feature-dev's
perseverance coach ([bots/feature-dev/main.bot](../bots/feature-dev/main.bot)
— `supervisor persy:` + `prompt persy_policy:`): a supervisor that
reproduces, agent-side, the push a good operator supplies when watching
a coding session live. Its policy is a reference for authoring similar
coaches:

- **monitors-first, pre-seeded, pinned** — the give-up markers
  (`impossible`, `unsolvable`, `infeasib…`, `giving/gave/give up`,
  `workaround`) are declared in the DSL `monitors:` list, each pinned
  `event_type=assistant_text` so they fire on the agent's own words,
  never on tool output or prompt echoes (unpinned, 5 of 10 evals were
  measured burned on echoes); `budget_warning` + `budget_exceeded`
  cover the pressure lane. The 5m `cooldown` debounces both the seeded
  matches and turn-boundary evals so the budget survives to the late,
  hard stretch of a campaign. Deliberately NO `tool_error` monitor: a
  red test is routine in a build loop — the FAILURE LOOP class needs
  repetition, which turn-boundary wakes surface via recent events;
- **four intervention classes** — premature impossibility (demand one
  more *instrumented* attempt: debug output, smaller repro, bisect),
  the expedient path (name the durable alternative), a failure loop
  (change of approach, not another retry), and budget/context pressure
  (bank now: commit what is green + a precise remaining-work note);
- **an asymptote guard** — never contest an honest termination report,
  never enlarge the scope, never re-raise an answered point. The coach
  pushes toward *completion*, not perpetual motion, so it composes with
  the convergence contract (ADR-058) instead of fighting it.

This matches what the 2025–26 steering literature converged on
(deterministic monitors + an advisor LLM consulted on detection;
explicit anti-give-up prompting in production harnesses), and the
failure mode it targets — an agent declaring a solvable problem
impossible until pushed — is well documented in the wild.

## Attaching a supervisor to a running run (CLI)

```sh
iterion supervise --run-id <id> \
  --node implement \
  --system @policies/watchdog.md \
  --monitor event_type=tool_error,tool_name=Bash \
  --model anthropic/claude-opus-5
```

Flags:

| flag | meaning |
|------|---------|
| `--run-id` | the iterion run to supervise (this OR `--claude-session`) |
| `--claude-session` | supervise a raw Claude Code session — its cwd (directory) or session id |
| `--node` | agent node id(s) to watch (repeatable; empty = whole run) |
| `--name` | supervisor name, shown in injected messages (`[supervisor <name>]`) and logs |
| `--system` | supervision policy text, or `@path` to read it from a file |
| `--model` | supervisor model spec; default auto-detect or `ITERION_DEFAULT_SUPERVISOR_MODEL` |
| `--monitor` | pre-declared monitor `key=val,key=val` (repeatable). Keys: `event_type`, `node_id`, `tool_name`, `text_contains`, `cost_gt` |
| `--cooldown` | min time between LLM evals on turn boundaries (default 30s) |
| `--max-evals` | hard cap on LLM evaluations for the run (default 20) |
| `--store-dir` | override the iterion store directory |

The supervisor blocks until the run terminates or you Ctrl-C to detach.
Because it observes via the shared store, it works against a run launched
by any other process (a `iterion run`, the studio, the dispatcher).

## Supervising a raw Claude Code session

A supervisor can also watch a **raw `claude` CLI / VSCode session** that
iterion did not launch — no `.bot`, no run. iterion observes the session
by tailing its transcript and steers it through Claude Code's own hook
mechanism (the same one the `claude_code` delegate uses internally).

One-time setup per repo — install the drain hook into the target repo's
Claude Code settings:

```sh
iterion supervise install-hook --cwd /path/to/repo   # writes .claude/settings.local.json
```

This adds a `Stop` + `PostToolUse` command hook that runs
`iterion __claude-hook-drain`. It is non-destructive (existing hooks and
keys are preserved) and idempotent; remove it with `uninstall-hook`. The
hook must be present **before** the `claude` session starts (Claude Code
reads hooks at session start).

Then attach a supervisor to a running session:

```sh
iterion supervise --claude-session /path/to/repo \
  --system @policies/watchdog.md \
  --monitor event_type=tool_error,tool_name=Bash
```

The argument is either the session's cwd (a repo directory, resolved to
its active session) or a session id directly. iterion finds the
transcript (`~/.claude/projects/<key>/<sessionId>.jsonl`), tails it, and
when the
supervisor decides to intervene it writes to an iterion-owned inbox
(`~/.iterion/claude-sessions/<key>/`); the installed hook drains it and
injects the message at the session's next tool/stop boundary. Raw
sessions have no nodes, so `--node` is ignored (always session-scoped).

How it maps to the managed path: the same `Coordinator`/bot drive both —
the transcript tailer is an `Observer` (it synthesizes `tool_called` /
`tool_error` / turn-boundary events from transcript records), and the
inbox is an `Injector`. Honest limits: injection lands at the next
boundary (no mid-LLM-call interruption); the hook must be pre-installed;
and concurrent sessions in one repo share the project inbox unless keyed
by session id.

## Monitors

A monitor is an event pattern; every set field must match (unset fields
are wildcards):

- `event_type` — matches the event type verbatim (`tool_error`,
  `node_finished`, `budget_warning`, …)
- `node_id` — matches the event's node
- `tool_name` — matches a tool event's tool (`Bash`, `Edit`, …)
- `text_contains` — case-insensitive substring against the rendered event
- `cost_gt` — fires on a `budget_warning` whose `used` exceeds the value

The supervisor bot registers the few signals it cares about and is then
woken only when they fire, instead of re-reading every turn — this is the
main token-saver.

## Limits

- **Next turn, not now.** A message lands at the next tool/stop boundary
  of the supervised node; an in-flight LLM call is never interrupted.
- **Local store / broker mode for `iterion supervise` attach.**
  `ObserveRun` is wired for the local broker path; cloud event-source
  mode is a follow-up. Declared (`supervisor NAME:`) supervisors are
  unaffected: every launch surface — CLI run/resume, studio/runview,
  the dispatcher's direct engine path, and the cloud runner pod —
  spawns them in-process next to the engine.
- **Subbot children are not supervised.** A child `.bot` declaring
  `supervisor NAME:` is silently unsupervised on every surface today;
  declare the supervisor in the parent (watching the subbot node's
  activity is not equivalent, but the parent's own agent nodes are
  coverable).

## Roadmap

- **Session-scoped raw inbox by default** — disambiguate concurrent
  `claude` sessions in the same repo (currently project-keyed with a
  session-id refinement).
- **Cloud event-source mode** for `ObserveRun` (today local broker mode).
- **Studio Launch-modal toggle** for the per-run kill switch (the
  `LaunchSpec.Supervisors` seam is wired; only the UI control is
  missing).
