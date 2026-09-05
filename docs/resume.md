# ⏯️ Resume paused, failed, and cancelled runs

A run in iterion is durable: it can pause for a human, fail, or be cancelled,
and later continue under the same run ID from an authoritative checkpoint — no
replay, no lost work — whether that pause lasted seconds or days. The
checkpoint, not `events.jsonl`, is the authoritative execution state.

## Resumable states

| Status | Meaning | Public resume behavior |
|---|---|---|
| `paused_waiting_human` | A human node or mid-agent interaction is waiting for answers. | CLI and `POST /api/runs/{id}/resume`; answers are required. |
| `failed_resumable` | Execution failed after resumable state was captured. | CLI and HTTP resume; the restart node is re-executed. |
| `cancelled` | The operator interrupted an active run and the checkpoint was preserved. | CLI and HTTP resume; no answers are required. |
| `paused_operator` | A local Studio soft pause or daily-spend-cap pause. | CLI and HTTP resume through the failure-style path; no answers are required. |
| `running` | Execution should still have an owner. | `--force-stale` can promote a provably stale local run after 60 seconds without an event flush. Server startup also reconciles orphans. |
| `queued` | Cloud queue state. | Internal publisher/runner resume transport; not a user-selected restart state. |
| `finished`, `failed` | Successful or non-resumable terminal state. | Cannot resume. |

Reaching the reserved `fail` node intentionally produces `failed`. Compile or
bootstrap errors can happen before a resumable run exists, and a store failure
that prevents checkpoint persistence may also fall back to `failed`.

A **named** fail node (`fail <name>:`, see [the DSL reference](dsl.md#typed-terminal-failure--fail-name))
may opt out of that with `resumable: true`, which parks the run
`failed_resumable` — the ordinary resumable status above, resumable by the
CLI and by HTTP. Declare it only when continuing is genuinely the cure. The
reference case is a phase-budget guard whose remedy is "raise the cap and
carry on": leaving that terminal makes the operator re-pay a phase the run
already completed, which is the very cost the guard exists to avoid. A
refusal that a resume could only repeat — "this lot is not actionable" —
stays terminal, the default.

Either way the node's `code:` lands on the run's `failure_code` and its
rendered `message:` on `error`, so what the resume is recovering FROM is
legible without opening the run's artifacts.

**The checkpoint anchors on the GUARD, not on the fail node.** A resume
starts execution at the checkpoint's node, so anchoring on the fail node
would re-dispatch the fail node and reproduce the identical outcome — the
guard that refused would never be re-evaluated and the raised cap would
change nothing. The engine therefore anchors a resumable fail on the
PREDECESSOR whose outgoing edge routed into it: the resume re-executes that
guard against the new caps and takes the other edge. Concretely:

```bash
iterion resume --run-id RUN_ID --max-cost-usd 10
```

**Fallback, stated out loud.** When no single predecessor can be named —
the fail node IS the workflow `entry:`, or several branches converged on
it, so no one guard owns the refusal — the promise cannot be kept. The run
then ends **terminal `failed`** and the engine logs a WARN naming the node
and the reason, rather than offering a resume that would silently do
nothing. The `code:` and `message:` still land on the run; only the
resumability degrades. (A fail node reached inside a `fan_out_all` /
`fan_out_each` branch never takes this path at all: the branch reports the
node's diagnosis as its error and the collector decides the run's fate.)

**A typed failure is NOT auto-resumed.** `--auto-resume` and the cloud
runner's retry both gate on a closed allow-list of engine codes
(`EXECUTION_FAILED`, `TIMEOUT`, `RATE_LIMITED`, `USAGE_LIMIT_BLOCKED`,
`NETWORK_TRANSIENT`, `TOOL_FAILED_TRANSIENT`, and `BUDGET_EXCEEDED` with a
raised cap). A bot-defined code is outside it by construction, and
deliberately so: the run refused on purpose, and nothing an unattended
retry can do changes the verdict — only an operator can (a raised cap, a
different `--var`). The run stays parked at `failed_resumable` for a
human, and the log says "not auto-recoverable (code &lt;YOURS&gt;)".

## CLI

The source path is optional when it was persisted at launch:

```bash
# Failed, cancelled, or operator-paused run: re-execute the checkpoint node.
iterion resume --run-id RUN_ID

# Human pause: answer fields individually or with typed JSON.
iterion resume --run-id RUN_ID \
  --answer approved=true \
  --answer note="ship it"

iterion resume --run-id RUN_ID --answers-file answers.json

# Human pause: upload a file for a `file`-typed output field (curl-style @).
iterion resume --run-id RUN_ID --answer music=@./theme.mp3

# Resume after deliberately changing the workflow source.
iterion resume --run-id RUN_ID --file workflow.bot --force
```

`--answer` is repeatable and carries strings; the runtime coerces them to the
paused node's output schema. `--answers-file` preserves JSON types. Explicit
flags override keys loaded from the file. A `file`-typed field is answered
with `key=@./path` — the path is staged as a run attachment before the node
resumes (the same upload the studio gate performs); the `@` convention is
honoured only for fields the schema declares as `file`, so a string value
that legitimately starts with `@` passes through untouched, and a literal
leading `@` inside a file field is written `@@`.

Use `iterion inspect --run-id RUN_ID` before taking over a suspicious
`running` run. `--force-stale` refuses when `events.jsonl` was flushed less
than 60 seconds ago, and the per-run lock still prevents two resume processes
from executing concurrently.

## What the checkpoint preserves

Current checkpoints include:

- the restart node and accumulated per-node outputs;
- loop counters, round-robin positions, loop previous/current snapshots, and
  `foreach`-relevant output state;
- artifact version counters and resolved variables;
- node recovery-attempt counters;
- pending interaction questions and interaction ID;
- backend session/conversation information for mid-agent `ask_user`;
- tokens, cost, iterations, active duration, and cumulative run spend.

Budget accounting therefore **does not reset on resume**. Legacy checkpoints
that predate the accounting fields deserialize them as zero, which retains the
old fresh-budget behavior for those historical runs only.

A best-effort checkpoint is written after each successful node boundary. On a
normal execution failure, the final checkpoint is anchored at the failing node;
resume re-executes it. Cancellation and operator pause anchor the node that was
about to execute. If a failure has no checkpoint, the runtime can restart from
the workflow entry.

Treat restartable node effects as at-least-once: a node may run again. Tool
commands should be idempotent or detect already-completed work, and coding
agents should inspect the durable Git history before repeating an item.

### The checkpoint restores outputs, not files

A cloud run bound to a repository (`RepoURL` on the queue message) gets its
workspace from a clone the runner makes on **every claim**: it `RemoveAll`s the
per-run directory before cloning, and deletes it again when the run returns. A
resume therefore starts from a pristine checkout of the run's ref — **anything
an earlier node edited but did not commit is gone**, and the resume is normally
claimed by a different pod anyway.

The checkpoint still replays that node's *outputs*. So a downstream node reads
"the previous node changed these files" against a tree where they no longer
exist, and the two disagree with nothing failing. A re-executing run emits
`run_workspace_reset` so the discarded tree is visible on the timeline —
keyed on the checkpoint's existence rather than on the delivery being shaped
as a resume, since a redelivery of a run still marked `running` re-clones the
same way.

What the fresh clone does NOT lose is committed work. A re-execution restores
what the run's earlier attempt left on the forge — the storage branch its
death bank recorded (`final_branch` / `final_commit`), else the newest attempt
ref a pause or an interrupted delivery parked — and emits
`run_workspace_bank_restored` naming the branch, the head and the base it was
put back on. The restore is two-step (the chain's own base, then a
fast-forward to its head) so the clone's reflog still reads "started from the
run's base": a bot that derives what the run changed from the newest reflog
entry that is not its own commit (docs-refresh's scope gate) does not mistake
the commits the target branch gained meanwhile for the run's work. A bank
branch that moved past the recorded head, or a chain with no common ancestor,
is refused loudly (`restored: false` + `reason`) and the run continues on the
fresh clone.

The consequence for authoring: **do not separate a node that mutates the
workspace from the node that persists the mutation by a resumable boundary
unless something checks the two still agree.** Either commit inside the
mutating node, or have the persisting step verify against the tree rather than
against the upstream node's claim (`dep-update-guard`'s `commit_check` is the
worked example — it compares `align.applied` with the branch head and blocks on
a contradiction). Git is the durable state; an uncommitted working tree is not.

## Source integrity and bundles

The launch stores a SHA-256 workflow hash. Resume recompiles the source and
refuses a mismatch unless `--force` is present. The CLI and HTTP service perform
this check before dispatching the resumed execution, so a rejected Studio
answer remains on the paused form; the Studio then offers an explicit **Resume
with updated workflow (force)** retry. Force is useful after repairing the
workflow, but it is an operator assertion that stored outputs, node IDs,
schemas, and the new graph are still compatible.

`--file` defaults to the persisted `FilePath`. Bundle runs also persist their
bundle path; resume reopens a `.botz` or bundle directory so prompts, skills,
attachments, recipes, and the selected preset are restored. If the original
archive moved, pass its new path explicitly.

Resume also rebuilds the execution environment: sandbox, run work directory,
plugin contributions/hooks, library skills, subbot runner, and worktree
context. A worktree run that eventually finishes goes through the normal
persistent-branch and merge-finalization path. A worktree workspace whose
`.git` pointer names a gitdir that no longer exists (the repository that
registered it is gone) refuses to resume with an explicit error — every git
command there would fail, and nodes would read the workspace as "no repo".
Cloud runs never create that shape: with a rootless store (cloud Mongo) the
engine skips worktree isolation and runs in place in the per-run clone, so
the workspace survives the runner's clone-recycle between queue deliveries.

## Overrides on resume

The resume command accepts the same recovery-relevant controls as launch:

| Flags | Use |
|---|---|
| `--max-cost-usd`, `--max-tokens`, `--max-duration`, `--max-iterations`, `--max-parallel-branches` | Raise or replace effective workflow caps. Already-consumed accounting remains charged. Zero means inherit. |
| `--permission`, `--permission-allow`, `--permission-ask`, `--permission-deny` | Rebuild the tool-permission policy; an allow rule can authorize the call that previously paused or failed. |
| `--model`, `--backend` | Re-apply selector-based execution overrides. Launch-time rules are stored for display but are not automatically re-applied to the resumed executor. |
| `--auto-resume N` | Retry eligible transient/rate-limit failures, or budget/timeout failures when a larger cap was supplied, with bounded backoff and forfait-cap checks. |
| `--force` | Permit a source-hash mismatch. |
| `--force-stale` | Take over an orphaned local `running` run after the staleness guard passes. |

When raising a budget, choose a cap above the amount already consumed. Merely
repeating the old cap causes the re-executed node to hit the same guard.

**Cloud runs.** `POST /api/runs/{id}/resume` (and `iterion remote runs
resume`) accepts the same budget-override flags as the local CLI —
`--max-cost-usd`, `--max-tokens`, `--max-duration`, `--max-iterations`,
`--max-parallel-branches`. The wire body carries them as
`{"budget": {"max_duration": "4h", ...}}`; the override MERGES per
field over the launch ask persisted on the run doc — a non-zero field
in the spec beats the doc, a zero field inherits it. Passing only
`--max-duration 4h` therefore raises the duration without erasing the
launch's cost/tokens caps. Without any override the resume replays
from the doc as before.

Two mechanics matter when raising a cap here:

- **The consumed accounting travels across resume.** The checkpoint
  restores `budget_elapsed_ns`, `budget_cost_usd`, `budget_tokens_used`
  and `budget_iterations_used` on every axis, so a resume with no
  override restarts against the same clock that killed it. Measure: a
  run parked on a 2h30m duration cap (elapsed = 9902 s) resumed bare
  walks another 5 min and dies at 9901.6 s + 10% = the exit-grace
  ceiling. The escape hatch is exactly what `--max-*` on resume raises.
- **The merged ask is persisted onto the run doc.** So a subsequent
  unattended auto-retry (usage-window sweeper) keeps the raised cap
  instead of reverting to the launch ask that already killed the run.
  A run resumed with `--max-cost-usd 120` retries at $120, not at the
  launch's $10.

The `--max-*` flags are the recommended path. The historical
"source-swap" recovery (POST `{"source": "<the .bot with a larger
cap>", "force": true}`) is NOT equivalent: it edits `wf.Budget` before
compile, and `resolveResumeBudgetAsk` then still merges the persisted
launch ask over top — on a run whose launch ask carried an explicit
cap the swap is overridden and the run dies at the persisted cap.
Prefer the flags; the swap is retained as a last-resort escape for the
case where no launch ask was ever recorded.

## Rewind: resume from an *earlier* node

Resume always restarts at the checkpoint node. `iterion rewind` moves that
checkpoint **backwards** onto a node the run already executed, so the next
resume replays from there instead of from where the run stopped. It is the
loop for iterating on a bot's configuration: run it, watch a node misbehave,
fix the prompt/schema/edges, replay just the affected part.

```bash
# edit main.bot, then let iterion locate the edit:
iterion rewind --run-id RUN_ID --auto
iterion resume --run-id RUN_ID --force

# or name the pivot yourself:
iterion rewind --run-id RUN_ID --node implement
```

### `--auto`: rewind to the node you edited

`--auto` diffs the workflow source the run executed against the source on disk
now, and rewinds to the earliest node the edit affects — so the loop is *edit,
rewind, resume*, with nothing to translate by hand. It prints what it detected,
so you can confirm it understood the change before resuming.

Detection is declaration-granular and resolves indirection:

| You edited | `--auto` rewinds to |
|---|---|
| A node's prompt, model, schema ref, tools, command… | that node |
| A shared `prompt` / `schema` / `cursor` body | every node referencing it, earliest first |
| An edge (condition, `with` mapping, loop bound) | the node the edge leaves, which re-selects it |
| `vars:`, `budget:`, `sandbox:`, `permission:`, `entry:` | the entry node — workflow scope reaches everything |
| Several nodes on one chain | the upstream-most one, so a single pass tests them all |

It errs toward reporting *more* nodes than strictly necessary: a false positive
costs re-executing a node that did not need it, while a false negative would
test the new configuration against stale downstream state — the failure the
feature exists to prevent. Nodes the run never executed are ignored, and edits
on independent fan-out branches are refused with the candidates named, since no
single pivot covers them.

Iterating repeatedly is safe: the source is re-stamped on each resume that
executes a changed workflow, so the second rewind of a session diffs against
what the first one actually ran rather than re-reporting its edits.

`--auto` needs `Run.WorkflowSource`, the `.bot` text captured at launch
(`WorkflowHash` only answers *whether* the source changed, never *which node*).
Runs started before that capture existed refuse `--auto` and still accept
`--node`. Comments and line shifts are not changes: the comparison runs on the
AST encoder, which omits source spans.

The run keeps its id, name, inputs, lineage, and budget accounting. Choose
between the two operations by intent:

| | `iterion fork` | `iterion rewind` |
|---|---|---|
| Run id | New child run | **Same run**, mutated |
| Original | Untouched | Outputs downstream of the pivot invalidated |
| Intent | An alternative future worth keeping side by side | "I misconfigured this — back up and replay" |
| Anchor | Requires a **turn** checkpoint, so LLM nodes only | Any node with a recorded output, including `tool` and `compute` |
| Audit | Two runs | One run plus a `run_rewound` event |

### What gets invalidated

The pivot's output is dropped, plus every node **forward-reachable from the
pivot that cannot reach it back**. Subtracting the ancestors is what makes
loops behave: in `implement -> verify -> implement as fix(3)`, `verify` is
both downstream and upstream of `implement`, and its output is exactly what
`implement` re-reads through `{{loop.fix.previous_output}}` on re-entry — so
it survives. Over-dropping is as harmful as under-dropping, because a node
whose output is deleted without re-executing before something reads it
resolves to `nil` rather than to a re-run.

Deliberately **not** reset:

- **Budget accounting and loop counters.** Refunding them would make
  rewind-then-resume an unbounded way around `max_cost_usd` and
  `max_iterations`. Raise the caps on the resume instead
  (`--max-cost-usd`, `--max-iterations`).
- **Artifact versions.** Re-execution appends a new version, so the
  superseded artifacts stay on disk and remain readable for comparison —
  invalidation here is logical, not a delete.
- **`events.jsonl`.** Append-only, never truncated. The dropped nodes' original
  records stay in the timeline, and the `run_rewound` event explains why they
  are about to repeat. Its payload is
  `{from_node, to_node, dropped_nodes, tombstoned_artifacts, orphaned_child_runs,
  promoted_from, files_reverted, files_ref, files_revert_commit,
  files_backup_ref, files_skip_reason}`.

Reset, because carrying them into the replay would be wrong: any pending
interaction (the rewound run never asked that question) and the backend
session/conversation rehydration (the pivot must replay against the *edited*
prompt, not the conversation the operator is trying to change).

### Preconditions and limits

The run must be `failed`, `failed_resumable`, `cancelled`, `paused_operator`,
`paused_waiting_human`, or `queued` — never `running`, whose engine owns the
checkpoint and would overwrite the rewind at its next node boundary. Cancel or
pause it first. The claim is a CAS, so a concurrent resume loses the race
rather than being rewound out from under itself.

`failed` stays semantically distinct from `failed_resumable`: the graph
deliberately abandoned the run (no auto-resume), it did not crash. But the
distinction no longer destroys the recovery point — a run that reaches the
DSL `fail` node keeps its checkpoint, so an explicit rewind can still recover
it. Runs that failed **before** that preservation existed carry no checkpoint
and stay unrecoverable (`run ... has no checkpoint — nothing to rewind`).

The run is parked in `cancelled`. That is the one resumable status a cloud
runner treats as "explicit resume required"; `failed_resumable` and
`paused_operator` are auto-resumed on queue redelivery, which would race the
operator's edit and execute the stale workflow.

Rewind resolves "downstream" against the workflow source **as it is now**,
which is why it performs no hash check — you rewind precisely because you
edited the `.bot`. The resume that follows still needs `--force`. Pass
`--file` when the source is not where the run recorded it.

### Subbots and parallel branches

A `subbot` node is an ordinary graph node, so the topology rules above apply to
it unchanged. But the child run is tracked outside the checkpoint, in
`Run.SubbotChildren` (**ADR-084**), and `ReattachSubbotChild` consults *only*
that map and the child's status — never the parent's checkpoint, and before the
child `.bot` is even compiled. Dropping the node's output is therefore not
enough on its own: rewind also **releases the child pointers** of every dropped
node, so the replay launches a fresh child against the edited child workflow
instead of adopting the previous one. The released child run ids are reported
(`orphaned_child_runs`); the runs themselves are left alone, so cancel one that
is still burning budget.

`--auto` does **not** see edits to a child `.bot`: it diffs the parent's source,
and `SubbotNode.Source` points at a different file. After editing a child
workflow, target the subbot node explicitly with `--node`.

The checkpoint is granular to the **node**, never to the execution: at
convergence the engine writes one entry per node id (last write wins), so N
parallel executions of the same node collapse to one output. A rewind naming a
node inside a fan-out body is therefore **promoted to the router** that
orchestrates it, and the whole fan-out replays. Anchoring on the body node
would replay it once, linearly, with no `each` context — silently testing one
iteration instead of all of them. The promotion is reported as `promoted_from`;
nested fan-outs promote to the outermost router, since an inner one re-run
alone would itself lack the outer iteration's context.

Sequential `loop` and `foreach` bodies need no promotion — they execute one at a
time. Rewinding into one resumes at the **current** iteration, because loop
counters are preserved; restarting the loop from zero would also refund the
`max_iterations` budget.

A concurrent run edit can make a rewind or rename return HTTP 409. Reload the
run before retrying: full-document saves compare the version that was read,
so they cannot undo a cancellation, resume, or checkpoint written meanwhile.
A failed final rewind save can leave workspace restoration already applied;
inspect the workspace before retrying when file restoration was requested.

Two further limits worth planning around:

- **Only engine state is rolled back.** Board cards, forge comments, pushed
  commits, and already-launched subbot child runs are not. A rewound subbot
  node re-runs and launches a *new* child; the old one stays, orphaned but
  traceable.
- **A rewind cannot target one branch of a fan-out.** The pivot is promoted to
  the router and every branch replays (see above) — there is no per-branch
  state to rewind to.

### Files the dropped nodes produced

A node's real product is often **not** its output map: a docs or code bot
writes dozens of files and returns a summary. Rewinding the checkpoint alone
would leave that half-written tree in place, and the replayed node would build
on top of its own previous production instead of meeting its prior conditions.

So a rewind also restores the workspace to the state the pivot started from.
The anchor is `refs/iterion/runs/<run>/pre/<node>/<iter>`, written by
`markPreNodeBoundary` when the node *starts* — distinct from
`refs/…/nodes/<node>/<iter>`, written when it *finishes* and therefore holding
that node's own output. The two bracket each node's effect. The pre-boundary
marker is an alias of the previous boundary's snapshot commit (nothing touches
the tree in between), so it costs one `update-ref` and no extra index walk.

It is a **revert, not a reset**: the prior tree is committed on top of HEAD, so
the run's own commits stay in `git log`, and the state at the instant of the
rewind — uncommitted and untracked work included — is banked first under
`refs/iterion/runs/<run>/rewind/<node>/<seq>`. Nothing the run ever had becomes
unreachable. `--restore-scope none` (formerly `--keep-files`) opts out.

A run **without** a worktree cannot use that path at all: its workspace is the
operator's live checkout, and `git add -A` there would stage their own
uncommitted work as a side effect of running a bot. That is the default shape
(17 of 30 catalog bots), so those runs are versioned by iterion itself —
see [workspace versioning](workspace-versioning.md). The rewind picks the
mechanism per run and reports which one ran:

| Run shape | Files restored by |
|---|---|
| `worktree: auto` (docs-refresh, feature-dev, wiki-gen…) | per-node git snapshots |
| in-place (whole-improve-loop, modernize, review-pr…) | `pkg/workspacetrack` |
| paths the ignore rules exclude, oversized files | neither — reported in `files.skip_reason` / `files.restored.skipped` |
| paths outside the workspace | neither |
| paths no execution of the run recorded changing (in place, default scope) | neither — reported in `files.left_in_place` |

Out-of-tree scratch is untouched by any scope: `${PROJECT_SCRATCH_DIR}` resolves
outside the workspace, so a campaign bot's `verify.sh` / `verify.log` survives a
rewind. That is correct — but it does mean a replayed verify gate can meet a
stale script.

#### How much comes back: `--restore-scope`

On an in-place run the workspace is **yours**, and a rewind is launched right
after you edit it — `--auto` derives the pivot by diffing that very edit. So at
the moment of every rewind the tree holds human work by construction, and
forcing the whole tree back would revert it. `--restore-scope` is the dial:

- **`produced`** (default in place) — only the paths this run is *recorded* to
  have changed after the pivot started. That set is the union of the diffs
  between consecutive boundaries in `[pre:<pivot> … the run's last boundary]`,
  not a diff of the two endpoints: a file one node rewrote and a later node put
  back is identical at both ends while demonstrably being a file this run
  writes to, and excluding it would let a third, uncaptured write survive.
- **`full`** (default in a worktree) — every versioned path in the snapshot.
  "Versioned", not "the whole disk": ignored, protected and never-captured
  paths are still untouched.
- **`none`** — nothing; the node replays against the tree as it stands.

`produced` is **refused, not silently widened, on a `worktree: auto` run**: git
is the mechanism there and it reverts the whole tree or none of it, so the
rewind says so and leaves the workspace alone rather than substituting the
maximal blast radius for a request to narrow it.

**What iterion cannot attribute, it reports.** A node that dies before its
boundary is written and an operator editing in another terminal leave identical
evidence, so the rewind names both sets instead of guessing:
`files.overwritten` (in-scope paths whose disk content matched neither side of
the recorded range — work that arrived after the run stopped recording it) and
`files.left_in_place` (changed since the run's last boundary, not restored).
Both also land in the `run_rewound` event, so an API- or agent-driven rewind
leaves the same audit trail as a CLI one.

To keep that honest the engine writes a **`fail:<node>:<iter>`** boundary when a
node's execution does not complete — a failure, an interruption, an operator
cancel — a **`pause:<node>:<iter>`** one when it parks on a human gate, and a
**`resume:<node>:<iter>`** one when it picks back up into a node that already
has a `pre:` boundary (rather than overwriting it, which used to redefine "what
this node started from" as "whatever is on disk now").
Without them a run that stopped *inside* a node would have nothing recorded
after the state that node started from (`pre:` is an alias and does not advance
the chain head), the scope would be empty, and the node's own debris would
survive its rewind. When the scope *is* empty the rewind says so and restores
nothing, rather than reporting a success it did not perform.

`pause:`/`fail:` open, and `resume:` closes, the one interval in which **nothing
of the run is executing** — and the scope excludes it. A file that appears while
a run waits for you, or between a failure and the resume you trigger after
triaging it, came from your editor, not from a node; without the exclusion the
capture the resume takes folds it into that node's apparent output and the
rewind deletes it. That is the only place authorship is decidable. An edit made
*while a node is running* is not, and lands in `files.overwritten` if the
restore takes it.

## Human and agent interaction resume

For an ordinary `human` node, answers become that node's typed output and edge
selection continues from it. Review gates use a dedicated multi-turn path and
can re-pause on the same interaction while preserving the dialogue.

When an `agent` or `judge` calls `ask_user`, the node has not completed and is
re-invoked after the answer:

- the in-process `claw` backend persists the opaque conversation and pending
  tool-use ID, then appends the answer as the matching tool result;
- CLI-agent backends reuse their session ID when supported; the runtime also
  injects the prior question and answer into the prompt as a compatibility
  fallback.

This keeps multi-turn interaction attached to the same node rather than
mistaking the pause for a completed human node.

### Recovery pauses re-execute the node

A **recovery pause** is written by the recovery dispatcher for a node whose
execution *failed* (`RecoveryPauseForHuman`: the provider rejected the
credential, the budget is exhausted, a policy asked for a human). Its
synthetic question (`acknowledge_recovery`, plus `recovery_code` and
`last_error`) promises a retry, and the resume delivers one: the answer is
recorded on the interaction (`kind: "recovery"`) as the audit trail of what
was fixed, and the node **re-executes from its own dispatch** — exactly as a
`failed_resumable` resume restarts it — before any successor runs. The
acknowledgement never becomes the node's output. The checkpoint carries the
marker (`recovery_pause` / `recovery_code`), and the `run_resumed` event
names the retry: `{resumed_from: "recovery_pause", restart_node, recovery_code}`.
The per-(node, code) attempt counters survive the pause, so a second identical
failure is judged as the second attempt. A budget pause resumed without a
raised cap is refused up front with the real cause, the same pre-flight the
failure path applies — raise it with the `--max-*` flags.

Distinguish it from a delegate pause: an agent that *asks* (`ask_user`,
`_needs_interaction`) has not failed; it re-invokes mid-conversation with the
answer merged into its input (above), and its checkpoint carries the backend
session, not the recovery marker.

## Dispatcher and last_run

A run is durable across a studio or dispatcher restart. If the board card
still points at that run (`issue.last_run`) and the status is live or
waiting, the dispatcher **does not mint a new run id**.

| `last_run` | Dispatcher action |
|---|---|
| `paused_waiting_human` / `paused_operator` | Re-park the card in `awaiting_input` (dispatcher-owned runs). No auto-resume (no answers). |
| `running` | Hold while the owner lives. An orphaned run (no live lock, past the grace window) is promoted to `failed_resumable` / `failed` first — never a sibling from the workflow entry. |
| `queued` | Hold without a lock probe: pipeline-queued runs have no lock owner until their concurrency slot opens. |
| `failed_resumable` / `cancelled` | `Engine.Resume` on the **same** id. |
| `finished` | Fresh run allowed — dragging the card back to `ready` is the re-queue gesture. |
| none, or hard `failed` + ticket explicitly back in `ready` | Fresh run. |

`iterion resume --force` (or the studio force-resume) continues **this**
run after a bot edit. If that resume parks again on a later human node,
the next dispatcher tick re-parks the same card — it does not start the
workflow over. To throw the work away, finish the run or let it fail
hard: a `cancelled` run is resumed from its checkpoint, not replaced.

See [dispatcher](dispatcher.md#paused-runs--the-awaiting_input-column--parked-sweep).

## Failure behavior

Most runtime errors use the checkpoint-aware failure path: LLM/delegate errors,
schema validation, edge/routing failures, budget and timeout errors, fan-out
failures, and resumable sandbox startup failures. A recovery policy may retry,
repair, or ask a human before the final status is written.

### Sandbox startup — which failures are resumable

A failure at `sandbox start` happens before the first node runs, so it is
classified from the DRIVER's typed error rather than assumed either way.
Both resumable codes are persisted on `failed_resumable`, so
`iterion resume`, `--auto-resume` and the cloud runner's redelivery all
pick them up; anything else stays terminal `failed`, because a redelivery
would re-hit it identically and only spend a pod per attempt.

| Failure | Code | Status |
|---|---|---|
| A bounded setup phase that RAN and stalled (workspace copy, git fixup) | `SANDBOX_SETUP_TIMEOUT` | `failed_resumable` — a fresh pod routinely clears the stall |
| The pod is still `Pending` past the deadline, unscheduled (`Unschedulable`, `Insufficient cpu`: the fleet is at its request ceiling) | `SANDBOX_CAPACITY` | `failed_resumable` — the run executed nothing; a later attempt re-places it |
| The pod is still `Pending`, scheduled, its node not having started the container (`ContainerCreating`, `PodInitializing`, or no container status reported yet) | `SANDBOX_CAPACITY` | `failed_resumable` — same: `Pending` IS the API's guarantee that no container was created |
| A broken image reference (`ErrImagePull`, `ImagePullBackOff`, `InvalidImageName`) | — | `failed` — every pod re-hits it; the operator fixes the reference. Overrides the phase: such a pod is `Pending` too |
| An invalid spec (`CreateContainerConfigError`, `CreateContainerError`) or a crash-looping container | — | `failed` |
| The pod reached `Running` (or `Unknown`) but never Ready | — | `failed` — a container came up; nothing says the run did nothing |
| The pod could not be inspected at all (RBAC, apiserver blip) | — | `failed` — no evidence, nothing claimed |

The cloud runner re-offers a `SANDBOX_CAPACITY` delivery after a delay
long enough for a cluster autoscaler to add a node; a fleet that stays
full through every permitted delivery parks the run on the DLQ like any
other repeated failure. The start deadline itself is
`ITERION_SANDBOX_K8S_POD_READY_TIMEOUT` — see
[sandbox](sandbox.md#scheduling-requests-and-node-spread).

Cancellation saves state with a detached, bounded store context so Ctrl-C can
still persist after the execution context is cancelled. If checkpoint writing
itself fails, the runtime logs the loss and can fall back to non-resumable
failure; resume never reconstructs execution state by replaying events.

## Implementation map

| File | Responsibility |
|---|---|
| `pkg/store/run.go` | Run statuses and the complete `Checkpoint` schema. |
| `pkg/store/store_run.go` | Atomic pause/failure/status persistence. |
| `pkg/runtime/checkpoint.go` | Checkpoint construction and budget restoration. |
| `pkg/runtime/run_failure.go` | Resumable failure and cancellation paths. |
| `pkg/runtime/resume.go` | Status dispatch, state/environment restoration, answer handling, and restart execution. |
| `pkg/runtime/pause.go` | Operator and daily-cap checkpointed pauses. |
| `pkg/cli/resume.go` | CLI admission, source reopening, overrides, locking, and stale-run takeover. |
| `pkg/runview/rewind.go` | In-place rewind: pivot validation, downstream computation, checkpoint mutation, artifact tombstones. |
| `pkg/runview/rewind_auto.go` | `--auto` targeting: declaration-granular source diff → impacted nodes → earliest pivot. |

See also [persisted formats](persisted-formats.md), [human interaction](human-in-the-loop.md), [permissions](permissions.md), and [merge policy](merge-policy.md).
