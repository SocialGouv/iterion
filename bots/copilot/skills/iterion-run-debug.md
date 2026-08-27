---
name: iterion-run-debug
description: Diagnosing an iterion run — the status ladder, what each pause and failure means, how to read events and the checkpoint, resume/rewind/fork semantics, and the recurring failure signatures with their real causes. Load in debug posture before concluding anything.
---

# Debugging a run

**Read before you conclude.** A diagnosis that cites no run id, no
event and no `file:line` is a guess.

## Where the evidence is

You have **no shell**. You do not need one: everything a run persists is
a file, and you have `Read` and `Glob`.

```
<store-dir>/runs/<run-id>/
  run.json          # status, error, inputs, and THE CHECKPOINT
  events.jsonl      # one event per line, monotonic seq
  artifacts/<node>/<version>.json
  interactions/<id>.json
  report.md         # present only if someone generated it
```

The store dir is anchored on the working directory: `<workspace>/.iterion`
when the project has a managed store, otherwise
`~/.iterion/projects/<encoded-workdir>/`. Glob for the run when you only
have an id prefix:

```
Glob: **/.iterion/runs/019f8384*/run.json
```

For the commands themselves — which the **operator** runs, not you:

```
iterion inspect                           # list all runs
iterion inspect --run-id <id>             # run-level summary
iterion inspect --run-id <id> --events    # the event stream
iterion report --run-id <id> --output /tmp/<id>.md   # full chronology
```

Note `iterion runs` is a *management group* (`prune` / `questions` /
`answer`) — invoked bare it prints its help and lists nothing. The
command that lists runs is `iterion inspect` with no `--run-id`.

## Status ladder

```
queued → running → paused_waiting_human | paused_operator
                 → finished | failed | failed_resumable | cancelled
```

| Status | Meaning | Resumable |
|---|---|---|
| `queued` | cloud only: submitted, no runner claimed it yet | — |
| `running` | executing | — |
| `paused_waiting_human` | parked on a human node or an `ask_user` | **yes**, with answers |
| `paused_operator` | parked by the operator | **yes** |
| `failed_resumable` | transient/recoverable failure, checkpoint kept | **yes** |
| `failed` | reached a `fail` node, or bootstrap died before a checkpoint | no |
| `cancelled` | interrupted; the checkpoint is preserved | **yes** |
| `finished` | reached `done` | no |

Two distinctions that matter:

- **`failed` vs `failed_resumable`.** Reaching a `fail` node is
  *intentional termination* — the workflow decided. `failed_resumable`
  is the engine saying "something broke, your state is intact".
- **`cancelled` keeps the checkpoint.** A cancelled run can be brought
  back; it is not a dead end.

## Resume, rewind, fork

```
iterion resume --run-id <id> --file <f> [--answers-file <a>] [--force]
iterion rewind --run-id <id> [--auto | --node <n>]
iterion fork   --run-id <id> --node <n> [--turn N]
```

- **resume** re-enters from the checkpoint. From `paused_waiting_human`
  it *injects the answers and moves past* the human node; from
  `failed_resumable`/`cancelled` it **re-executes** the checkpointed
  node.
- **`--force`** is needed when the `.bot` source changed since the run
  started (the engine hashes it). Without it you get a hash-mismatch
  refusal. Editing a bot mid-session is exactly when you need it.
- **rewind** re-anchors *this* run on an earlier node and invalidates
  everything downstream. `--auto` diffs the edited `.bot` against what
  the run executed and targets the change — the bot-development loop.
- **fork** branches a *new* run from a prior turn of an existing one.

## The checkpoint is the truth

`run.json` carries the checkpoint; `events.jsonl` is **observational
only**. When the two disagree, the checkpoint wins — resume reads it,
not the events. The checkpoint holds the current node, node outputs,
loop counters, vars, budget accounting, and the backend session anchor.

Budget accounting is restored from it on **every** resume, which is why
budgets are cumulative across a run's whole life.

## Reading the event stream

Useful event types, roughly in the order they tell a story:

`run_started` · `node_started` · `llm_request` · `llm_retry` ·
`tool_called` · `assistant_text` · `artifact_written` ·
`human_input_requested` · `run_paused` · `run_resumed` ·
`edge_selected` · `join_ready` · `budget_warning` · `budget_exceeded` ·
`run_finished` · `run_failed`

Technique: find the **first** anomaly, not the loudest one. A single bad
node output produces a cascade of downstream complaints, and the last
error in the log is usually a symptom.

- Per-turn latency = Δ between two `node_started` on the same node.
- Per-turn cost = Δ of the checkpoint's cumulative cost between turns.
- `edge_selected` tells you *why* the run went where it went — this is
  the event people forget to look at when a router "misbehaves".

## Runtime error codes

`NODE_NOT_FOUND` · `NO_OUTGOING_EDGE` · `LOOP_EXHAUSTED` ·
`BUDGET_EXCEEDED` · `EXECUTION_FAILED` · `WORKSPACE_SAFETY` · `TIMEOUT` ·
`CANCELLED` · `JOIN_FAILED` · `RESUME_INVALID`

Each carries a `NodeID` and usually a `Hint` — read the hint before
theorising.

## Recurring signatures

| Symptom | Usual cause |
|---|---|
| `BUDGET_EXCEEDED` on a long-lived looping bot | budgets are **cumulative**; the caps were sized per-turn |
| A conversational bot "forgets" everything each turn | the loop edge lost `_session_id` (a typo there is silent), or the backend session died — in cloud the CLI transcript lives in a per-delivery temp dir |
| `LOOP_EXHAUSTED` | the loop's exit condition never became true; check the `when` field's actual value in the node output |
| `NO_OUTGOING_EDGE` | every `when` was false and there is no `else`/default edge |
| Agent "has no tools" | on claw: `tools:` empty ⇒ zero tools; or a declared `mcp_server` with no `mcp: servers:` selecting it |
| Node dies immediately on start | an unresolvable tool name in `tools:` — it fails at runtime, `validate` does not catch it |
| Run pauses at a surprising point | `permission: ask` — an unlisted tool call triggers an approval pause |
| "All files deleted" in git | a repo-root `workspace_dir` override under sandbox: `.git` is mounted but the working tree is not. Omit the override and let it default |
| `run not found … run.json: no such file` in the studio | the run went to a different store dir than the studio reads |
| Run drained mid-flight in dev | a dev backend under a file watcher restarted and cancelled it |
| Cloud run stuck in `queued` | no runner claimed it — check the queue, and whether a second submission for the same run was deduplicated |

## Sandbox and workspace gotchas

- `sandbox: auto` is the default; `sandbox: none` raises `C128` and
  means every tool runs on the host with its filesystem and credentials.
- Under sandbox, the workspace is bind-mounted; a `workspace_dir`
  pointing at the repo root instead of the worktree makes git report a
  phantom "everything deleted".
- `worktree: auto` runs land their commits on a storage branch
  (`iterion/run/<name>`) and best-effort fast-forward the checked-out
  branch. `run.json` records `final_commit` / `final_branch` /
  `merged_into` — that is where to look for "where did my commits go".

## What to hand back

State it in this order, always:

1. **What happened** — the run id, the node, the first anomalous event.
2. **Why** — the mechanism, with the evidence you read.
3. **The fix** — the command or the edit, concretely.

If you could not determine the cause, say which evidence was missing
and how to capture it on the next run. That is a useful answer; a
confident wrong mechanism is not.
