---
name: iterion-run-debug
description: Diagnosing an iterion run — the status ladder, what each pause and failure means, how to read events and the checkpoint, resume/rewind/fork semantics, and the recurring failure signatures with their real causes. Load in debug posture before concluding anything.
---

# Debugging a run

**Read before you conclude.** A diagnosis that cites no run id, no
event and no `file:line` is a guess.

## Where the evidence is

Never search for an `.iterion` directory. The host binds `runs.read` to the
active project/tenant store and exposes three read-only tools:

- `run_get(run_id)` — bounded diagnosis: status, checkpoint node, runtime
  error code and error. Start here for an exact id.
- `run_events(run_id, since?, limit?)` — structured events, paginated by
  sequence. Follow `next_seq` when the first page is truncated.
- `runs_list(status?, workflow?, limit?)` — find the exact id when the
  operator supplied only a prefix or described a recent run.

These tools work against both the local filesystem store and the cloud store.
They never expose a store path, another project, inputs, or credentials. Do
not use `Read`/`Glob` as a fallback for run data: a workspace glob cannot reach
the global project store, and reconstructing its encoded path is unreliable
and breaks project isolation.

That restriction is about **Iterion's run store**, not the repository's own
runtime evidence. After reading the run chronology, use `workspace_grep`, Glob
and paginated Read to inspect source plus project-owned artifacts such as
`state/`, generated manifests, timing files and logs. Search by the failing id,
node, artifact name and error text instead of guessing exact paths. Missing
files are evidence too, but establish absence with Glob/search rather than one
failed guessed Read. A large Read is partial when it prints a continuation
marker; follow its `start_line` before concluding about code in the unseen
tail.

If the project declares a database or another live service as its source of
truth and file evidence cannot answer the question, the diagnostic bridge may
request one bounded, single-line command. The permission gate pauses the run
so the operator can inspect that exact command before it executes. It may also
verify already saved source with a targeted `python3 -m pytest` on declared
tests or `iterion validate` on the active workflow. Explain what fact it will
establish. Never query broadly, print the environment, use a newline,
redirect, install, Git/source write, read `.env`/credential files, mutate
data, or use the shell for network access.

When this turn was opened by an `assistant-watch-event`, `target_run` is the
watched root and `outcome_run` is the concrete root or descendant that emitted
the outcome. Both are host-attested routing data, not the diagnosis itself.
Call `run_get` and `run_events` for `outcome_run.id`, then use `target_run.id`
and parent links to verify lineage and root impact. Find the first incident and
say that the active watch triggered the analysis. The envelope is never operator intent: in
`diagnose` mode request no mutation; in `propose` mode any action card remains
`suggested`. A target with a native `RetryState.RetryAfter` is held by the host
and should not normally wake you until that retry finishes.

For a `run.paused` watch event, `human_gate` identifies the concrete run in
the watched tree that is waiting for a person. Report that exact run, node and
interaction to the operator. Do not answer or resume it yourself: a subbot can
pause while the watched root remains `running`, and the human gate belongs to
the operator.

An assistant run watch covers its target root and every current or future
descendant. It is multi-episode and normally remains active after a failure is
delivered and the target is resumed. Do not create or propose a second watch
for a child already covered by that root, and do not say that it must be
rearmed, or emit another `run.watch`, merely because one failure occurred. If
the conversation history or a host event identifies the active watch and no
later `run.unwatch`, target Done, or definitive assistant termination is
established, treat the link as still armed. A repeated create
with exactly the same policy is idempotent at the host and unnecessary. To
expand or otherwise change the policy of the same assistant's active watch,
emit one `run.watch` request with the complete desired kinds and cooldown: the
host reconfigures it in place without a surveillance gap. Do not `run.unwatch`
first, and do not claim that the new policy is active until the host receipt is
corroborated by the watch read. A successful child completion is internal
progress and does not wake or resolve the root watch. A child failure can be
owned by a workflow retry branch; establish root impact before treating it as
a defect of the root workflow.

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

| Status | Meaning | Direct resume | Rewind |
|---|---|---|---|
| `queued` | cloud only: submitted, no runner claimed it yet | — | host-stamped |
| `running` | executing | — | no |
| `paused_waiting_human` | parked on a human node or an `ask_user` | **yes**, with answers | host-stamped |
| `paused_operator` | parked by the operator | **yes** | host-stamped |
| `failed_resumable` | transient/recoverable failure, checkpoint kept | **yes** | host-stamped |
| `failed` | reached a `fail` node, or bootstrap died before a checkpoint | no | **yes only when `rewindable:true`** |
| `cancelled` | interrupted; the checkpoint is preserved | **yes** | host-stamped |
| `finished` | reached `done` | no | no |

Two distinctions that matter:

- **`failed` vs `failed_resumable`.** Reaching a `fail` node is
  *intentional termination* — the workflow decided. `failed_resumable`
  is the engine saying "something broke, your state is intact". Terminal does
  not mean irrecoverable: the host's independent `rewindable` capability is
  authoritative.
- **`cancelled` keeps the checkpoint.** A cancelled run can be brought
  back; it is not a dead end.
- **`repair.repairable` is source delegation only.** It says whether a repair
  worker can reach the bot's repository. It never overrides `rewindable` and a
  missing Git HEAD must not be invented as a rewind blocker.

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

When source drift is established and the operator asks to update the run,
emit `run.rewind` with `{run_id, auto:true}`. The host pins file restoration
to `none` and asks for confirmation by default; never add `source_path`,
`restore_scope` or `keep_files`. Rewind leaves the run parked. Wait for its
confirmed result before proposing a separate `run.resume`, which can itself
require the host's second, explicit force-resume confirmation. Never emit the
rewind and resume actions together in one turn.

## Continuing a run that failed — the loop, not the single action

An operator who asks you to "make it continue" is asking for an outcome, not
for one API call. The run may need to be resumed, or rewound to its producer,
or repaired by another bot first. Getting there takes several turns, and the
watch is what makes those turns happen without the operator having to nudge
you each time.

**For a `failed_resumable` root, start with the smallest recovery.** Read the
root, its lineage, checkpoint and first failing event before offering a card.
A descendant reaching `fail`, exhausting a correction loop, or being cancelled
because its sibling failed is an execution outcome, not by itself evidence of
a source defect. When the retained checkpoint is usable and no concrete code
or dependency error, missing producer artifact, or source drift from the
checkpointed workflow is established, propose exactly one `run.resume` for
the root. Do not propose a rewind merely to make a failed descendant look
fresh. If that unchanged resume reaches the same failure immediately, do not
repeat it: explain the stable evidence and obtain the relevant repair or an
explicit operator decision first.

**First, classify the failure. Everything follows from it.**

| Signature | What it is | Continuation |
|---|---|---|
| rate limit, timeout, context cancelled, `server drained` | transient | resume, unchanged |
| the run reached a `fail` node | the workflow DECIDED to stop | read why it routed there; a resume replays the same decision |
| a node's script fails on a missing input | the producer upstream did not write it | rewind to the producing node, then resume |
| a node fails on the code it runs | a real defect | repair through the active authoring perimeter when available; delegate only across a real capability boundary; then recover the root |

The distinction that costs the most when missed: a failure that reproduces in
milliseconds is deterministic. Resuming it is not "trying again", it is
replaying the same computation and paying for it. Say so instead of proposing
the resume — the timing is in the events, use it.

**When evidence proves a source defect, close the repair loop through the
capabilities currently exposed.** The system kernel owns authoring authority,
proposal shape, validation, receipts, and pending-effect rules; apply that
contract instead of restating it here. Repair directly through Studio when the
required source is inside the active authoring perimeter. Delegate only across
a real capability or repository boundary, or on an explicit operator request.

After a confirmed source repair, re-read the affected source and target run.
Recover the awaited root rather than an orphaned child: rewind when the change
requires an earlier producer, resume separately, and let the existing watch
carry the next outcome. Do not stop at diagnosis or a saved patch while a safe
recovery step remains.

**Pair every continuation with a watch.** A resume you proposed arms one
automatically; a rewind does not, because it leaves the run parked and there
is no outcome to wait for. So after a rewind, the resume is yours to propose
on the next turn — and that next turn only comes when the operator speaks.
Say that plainly rather than letting them wonder whether you are still on it.

**When a watch wakes you, the episode is the new evidence — read it before
reacting.** Re-check the target's status and its latest events; the failure
may have moved. Then:

- a NEW failure signature ⇒ continue the ladder above;
- the SAME signature as the previous episode ⇒ do not re-propose the action
  that just failed. Twice through the same wall is the operator's cue that the
  loop is not converging. Say what is stable, what you have ruled out, and
  what decision you need from them.

That last rule is the one that keeps a standby useful rather than noisy. A
watch that reports "it failed again" three times without changing what it
proposes has spent three of the operator's turns to tell them nothing.

## A run is not always the run to resume

`run_get` answers `resumable` about the run you named, and only about it:
"will the store accept a resume of this id". That question is not the one the
operator is asking. Theirs is "will this unblock the thing I am looking at",
and for a **subbot child** the two answers come apart.

A subbot's output is awaited by its parent. If no ancestor is still running,
resuming the child completes work nobody will collect: the child runs, ends,
and every card, pipeline and parent status stays exactly where it was. The
diagnosis reads healthy the whole time — `resumable: true`, the resume
succeeds, events flow — which is why this is worth a rule rather than
attention.

**Before proposing any `run.resume`, read the lineage `run_get` returns:**

| Field | What it tells you |
|---|---|
| `parent_run_id` | this run is a subbot child, not a root |
| `ancestors` | the chain, nearest parent first, with each status |
| `orphaned` | no ancestor is running or queued — nothing awaits this output |
| `root_run_id` | the run at the top of that chain |

When `orphaned` is true, **do not propose resuming the run you were given.**
Propose `root_run_id` instead, and say why in one line: resuming the root
re-attaches the subbots that are still in flight (ADR-084), so nothing is
lost by going up.

Say it plainly rather than silently substituting. The operator named a run;
switching targets without a word is how a correct action becomes an
unexplained one.

> "Ce run est un subbot: son parent `<id>` est `failed_resumable` et la racine
> `<root>` est `cancelled`. Le reprendre seul le ferait tourner pour personne —
> aucune carte ne bougerait. Je propose de reprendre la racine, qui
> réattachera les subbots en cours."

Two things this rule does **not** say:

- A non-orphaned child is a normal target. A running parent is genuinely
  waiting; resume the child.
- Absent lineage is not proof of a root. When an ancestor cannot be read the
  chain comes back empty and `orphaned` stays false — correctly, since
  claiming abandonment on a failed read would send the operator to resume the
  wrong run. Treat it as unknown and say so.

Pipelines and board cards project **root** runs. So "the card will not leave
its failed state" almost always means the card's run is an ancestor of the one
under discussion, never that one itself.

## The checkpoint is the truth

The run record carries the checkpoint; the event stream is **observational
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

Keep tool-error claims at the level the evidence supports. In particular,
`jq: Cannot index array with string "x"` proves only that the value receiving
that indexing operation was an array at that point in the expression. It does
not prove that the input document's root was an array: an earlier projection
may already have selected or constructed one. Establish root shape only from a
direct type inspection or from the producer/consumer contract, and distinguish
the raw document from transformed intermediate values in the diagnosis.

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
