---
name: iterion-run-and-refine
description: >
  Run, observe, diagnose, resume, and refine Iterion .bot workflows against
  real repositories. Use when asked to test a workflow, investigate a failed
  or paused run, improve convergence or output quality, inspect artifacts and
  events, adjust budgets or permissions, or safely continue a persisted run.
---

# Run and refine Iterion workflows

Treat a persisted run as evidence, not as a disposable prompt session. Inspect
its status, checkpoint, events, artifacts, worktree, and source hash before
changing anything. The checkpoint in `run.json` is the authoritative resume
state; `events.jsonl` is an audit and observation stream, not a replay log.

Read [`docs/resume.md`](docs/resume.md) for status and override semantics and
[`docs/persisted-formats.md`](docs/persisted-formats.md) for stored shapes.
Use [`docs/cli-reference.md`](docs/cli-reference.md) for current command flags.

## Refinement loop

1. Validate the workflow or bundle.
2. Launch with an explicit store, realistic inputs, finite budgets, and the
   smallest useful log level.
3. Observe node transitions, artifacts, deterministic gates, and Git commits.
4. Diagnose the first load-bearing failure or quality gap.
5. Fix the smallest responsible layer: workflow, prompt/skill, environment, or
   engine. Do not mask an engine defect with a misleading prompt workaround.
6. Revalidate changed workflow sources.
7. Resume only when the stored state is compatible; otherwise start a new run.
8. Repeat until the deterministic acceptance criteria pass and the output is
   useful, not merely terminal.

Record durable lessons in the bot README, a companion design document, or a
dated [`docs/bot-runs/`](docs/bot-runs/) bilan according to repository
convention. Include evidence and trade-offs rather than a chronological diary.

## Launch and observe

```bash
iterion validate bots/example/main.bot
iterion run bots/example/main.bot \
  --store-dir .iterion \
  --var scope=pkg/runtime \
  --log-level info \
  --timeout 2h
```

Prefer public inspection surfaces over ad-hoc parsing:

```bash
iterion inspect
iterion inspect --run-id RUN --events
iterion inspect --run-id RUN --list-nodes
iterion inspect --run-id RUN --node verify --section all
iterion report --run-id RUN --output run-report.md
```

When deeper evidence is necessary, inspect the selected store under
`runs/<run_id>/`: `run.json`, `events.jsonl`, `run.log`, versioned artifacts,
interactions, plans, and large tool blobs. Never print secret material while
debugging.

Useful event families include:

- lifecycle: `run_started`, `run_paused`, `run_resumed`, `run_failed`,
  `run_finished`, `run_cancelled`;
- graph: `node_started`, `node_finished`, `edge_selected`, `join_ready`,
  `budget_warning`, `budget_exceeded`;
- LLM/tools: `llm_request`, `llm_retry`, `llm_step_finished`,
  `delegate_*`, `tool_started`, `tool_called`, `tool_error`;
- durable outputs: `artifact_written`, `plan_written`, review and sandbox
  events.

Event payloads are additive. Inspect the current emitter before depending on a
specific key.

## Resume by status

| Status | Action |
|---|---|
| `paused_waiting_human` | Supply answers and resume. |
| `failed_resumable` | Resume; the checkpoint node is re-executed. |
| `cancelled` | Resume without answers when the preserved state is still wanted. |
| `running` | Do not start a second engine. Confirm ownership; use `--force-stale` only after the 60-second freshness guard allows takeover. |
| `paused_operator` | Resume directly without answers; CLI/runview use the failure-style checkpoint path. A changed workflow still requires explicit `--force` after the pre-dispatch hash check. |
| `queued` | Leave cloud queue ownership to the publisher/runner path. |
| `finished` / `failed` | Do not resume; start a new run if more work is required. |

Examples:

```bash
# Failure or cancellation; --file is optional when launch persisted it.
iterion resume --run-id RUN --store-dir .iterion

# Human answers. --answer is repeatable and schema-coerced; a JSON file keeps
# explicit nested/array types.
iterion resume --run-id RUN --answer approved=true --answer note="ship it"
iterion resume --run-id RUN --answers-file answers.json

# Deliberately repaired workflow source.
iterion resume --run-id RUN --file bots/example/main.bot --force

# Provably orphaned running process only.
iterion resume --run-id RUN --force-stale
```

Use `--force` only to acknowledge a workflow-source hash mismatch. It asserts
that node ids, schemas, stored outputs, loops, and the new graph remain
compatible; it is not universally safe and does not override run ownership.

Checkpointed cost, tokens, iterations, active duration, and cumulative spend
survive resume. Raise a cap above already-consumed usage when recovering from a
budget failure:

```bash
iterion resume --run-id RUN --max-cost-usd 25 --max-duration 3h
```

Reapply launch-time `--model` and `--backend` selectors when continuity
depends on them; they are stored for display but are not automatically
re-applied to the resumed executor. Permission and allow/ask/deny overrides can
authorize a previously gated call. Use `--auto-resume` only for eligible
transient/rate-limit failures or raised-cap budget/timeout recovery.

Assume node effects are at-least-once. Before re-executing a mutating node,
inspect Git history and external side effects so retries remain idempotent.

## Diagnose the responsible layer

### Workflow and DSL

- Run `iterion validate` after every structural edit.
- Check schemas, prompt references, edge exhaustiveness, declared cycles,
  router-mode properties, convergence, resources, and workspace safety.
- Add an exit edge for loop exhaustion. Do not make a loop unbounded merely to
  silence `LOOP_EXHAUSTED`.
- Keep decisions that can be computed or tested out of LLM prompts.

### Structured LLM output

- Inspect the node's prompt, schema, backend events, and raw/tool output.
- Distinguish a backend transport failure from valid prose that failed schema
  validation.
- Ask for the required structured result clearly, but do not weaken a schema
  solely to make an unreliable response pass.
- Preserve useful session continuity only where graph semantics allow it;
  convergence nodes cannot blindly inherit one branch's session.

### Environment, tools, and sandbox

- Confirm the effective sandbox driver/image, host-state policy, mounts,
  egress rules, and non-interactive `PATH`.
- Declare repository tools in the repository `devbox.json`; declare bot-only
  tools beside the bot's `main.bot`. Iterion installs both and prepends their
  profile bins for all node processes, repository first.
- Do not replace a missing declared tool with an unpinned download in a prompt.
- Use `iterion sandbox doctor --strict <workflow>` before expensive runs.

### Runtime and infrastructure

- Treat rate limits, transport timeouts, runner loss, and sandbox startup
  failures separately from workflow-quality defects.
- Preserve the original error and checkpoint. If checkpoint persistence
  failed, events cannot reconstruct execution state.
- Reproduce suspected engine defects with the smallest deterministic test
  before changing runtime code.

### Quality and convergence

- Define success as a testable outcome, not an edit count or self-reported
  confidence.
- Keep a living, bounded work ledger and commit verified units in stride for
  long mutating campaigns.
- Use deterministic build/test/scope gates as truth oracles.
- When work repeats, compare consecutive evidence: unchanged blockers may mean
  a bad fix, a false-positive gate, stale inputs, or a missing capability.
- Separate completion of the current unit from global completion so a global
  metric does not trap a local repair loop.
- Bound every continuation path by workflow and run budgets.

## Worktrees and external effects

For `worktree: auto`, inspect the run's persisted `work_dir`, base/final commit,
storage branch, and merge status. Successful finalization may preserve or merge
commits according to launch settings; failed and cancelled runs can retain
recoverable state. Do not remove a worktree or branch while a run might still
own it, and do not assume a live URL, pushed branch, or generated file proves
the requested delivery is traceable and complete.

Before declaring convergence, verify:

- the current workflow still validates;
- deterministic checks pass from the intended workspace;
- required artifacts and external deliveries correspond to the current commit;
- no unresolved human interaction or deferred branch remains;
- the final status, report, and Git state agree;
- documentation records only evidence observed in this run.
