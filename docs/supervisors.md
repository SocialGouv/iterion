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
  model: "anthropic/claude-opus-5"  # default: auto-detect / ITERION_DEFAULT_SUPERVISOR_MODEL
  system: watchdog_policy              # a prompt: ref — the supervision policy
  cooldown: "45s"                      # min between turn-boundary evals (default 30s)
  max_evals: 12                        # hard eval cap (default 20)

prompt watchdog_policy:
  Intervene only if the implementer edits files outside src/, or a Bash
  test fails twice in a row. Keep messages short and actionable.
```

Launch the workflow normally (`iterion run`); the supervisor is
auto-spawned, observes the run, and is torn down when the run ends. The
`watches:` ids must name agent nodes (a warning `C190` fires otherwise),
and `system:` must reference a declared prompt (`C193`). Monitors aren't
declared in the DSL — the supervisor bot registers the patterns it cares
about at runtime; use the CLI `--monitor` flag to pre-seed them when
attaching externally.

## Attaching a supervisor to a running run (CLI)

```sh
iterion supervise --run-id <id> \
  --node implement \
  --system @policies/watchdog.md \
  --monitor event_type=tool_error,tool_name=Bash \
  --model anthropic/claude-opus-4-8
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
- **Local store / broker mode.** `ObserveRun` is wired for the local
  broker path; cloud event-source mode is a follow-up.

## Roadmap

- **Inline `monitors:` in the DSL block** — today monitors are registered
  by the bot at runtime or pre-seeded via the CLI `--monitor` flag.
- **Session-scoped raw inbox by default** — disambiguate concurrent
  `claude` sessions in the same repo (currently project-keyed with a
  session-id refinement).
- **Cloud event-source mode** for `ObserveRun` (today local broker mode).
