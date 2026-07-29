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

## Failure behavior

Most runtime errors use the checkpoint-aware failure path: LLM/delegate errors,
schema validation, edge/routing failures, budget and timeout errors, fan-out
failures, and resumable sandbox startup failures. A recovery policy may retry,
repair, or ask a human before the final status is written.

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

See also [persisted formats](persisted-formats.md), [human interaction](human-in-the-loop.md), [permissions](permissions.md), and [merge policy](merge-policy.md).
