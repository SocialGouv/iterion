# Persisted Formats — V1 Reference

This document describes the on-disk formats used by the iterion store.
These formats are considered **stable for V1** — tooling may rely on them.

## Directory Layout

```
<store-root>/runs/
  <run_id>/
    run.json                            # Run metadata & checkpoint
    events.jsonl                        # Append-only event log
    artifacts/
      <node_id>/
        0.json, 1.json, ...            # Versioned node artifacts
    interactions/
      <interaction_id>.json             # Human input/output exchanges
```

## run.json (format_version: 1)

```jsonc
{
  "format_version": 1,               // integer, current = 1
  "id": "run_1234567890",
  "workflow_name": "my_workflow",
  "status": "running",                // see Run Status below
  "inputs": { "key": "value" },       // workflow inputs (optional)
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:01:00Z",
  "finished_at": "2026-01-01T00:02:00Z",  // set on terminal status (optional)
  "error": "",                         // non-empty on failed/failed_resumable/cancelled (optional)
  "checkpoint": null,                  // present when paused, failed_resumable, or cancelled (optional)
  "artifact_index": { "analyze": 2 }  // node_id → latest version (optional cache)
}
```

### Run Status

| Status                  | Meaning                                          | Resumable? |
|-------------------------|--------------------------------------------------|------------|
| `running`               | Execution in progress                            | —          |
| `paused_waiting_human`  | Paused at a human node, awaiting answers         | Yes (needs answers) |
| `finished`              | Completed successfully (reached done node)       | No         |
| `failed`                | Non-resumable failure (fail node or no checkpoint) | No       |
| `failed_resumable`      | Failed with checkpoint (can resume)              | Yes        |
| `cancelled`             | Interrupted by user with checkpoint preserved    | Yes        |

### Checkpoint (when status = paused_waiting_human, failed_resumable, or cancelled)

The checkpoint is the **authoritative source of truth** for resume. Events
(`events.jsonl`) are observational only and are never replayed to reconstruct
state. If the checkpoint is lost, recovery is not possible via event replay.

A checkpoint is saved after every successful node execution (best-effort). On
failure, the checkpoint points to the **failing node** — resume re-executes
that node. On cancellation, the checkpoint points to the node that was about
to be executed.

```jsonc
{
  "node_id": "review",                          // human node where paused
  "interaction_id": "run_123_review",            // pending interaction
  "outputs": { "node_id": { "key": "value" } }, // accumulated outputs
  "loop_counters": { "retry": 2 },              // current iteration counts
  "artifact_versions": { "node_id": 3 },        // *next* version per node (not latest)
  "vars": { "repo": "my-repo" },                // resolved workflow variables
  "interaction_questions": { "summary": "..." } // embedded for resilience (optional)
}
```

`interaction_questions` duplicates the questions from the interaction file so
that resume is self-sufficient even if the interaction file is deleted.

## events.jsonl

One JSON object per line, append-only. Events are ordered by `seq` (monotonic within a run).

```jsonc
{
  "seq": 0,                              // int64, starts at 0
  "timestamp": "2026-01-01T00:00:00Z",   // wall-clock UTC
  "type": "run_started",                  // see Event Types below
  "run_id": "run_123",
  "branch_id": "",                        // non-empty for parallel branch events
  "node_id": "",                          // context-dependent (optional)
  "data": {}                              // event-specific payload (optional)
}
```

### Event Types

| Type                      | Node? | Data keys                              |
|---------------------------|-------|----------------------------------------|
| `run_started`             | no    | —                                      |
| `run_resumed`             | no    | —                                      |
| `run_paused`              | yes   | —                                      |
| `run_finished`            | no    | —                                      |
| `run_failed`              | yes   | `error`, `code`, `resumable?`          |
| `run_cancelled`           | yes   | `reason`                               |
| `branch_started`          | yes   | —                                      |
| `node_started`            | yes   | `kind`                                 |
| `node_finished`           | yes   | `output`, `_tokens`, `_cost_usd`       |
| `llm_request`             | yes   | provider-specific                      |
| `llm_retry`               | yes   | `attempt`, `error`                     |
| `node_recovery`           | yes   | `code`, `reason`, `attempt`, `delay_ms?`, `error?` |
| `llm_step_finished`       | yes   | provider-specific                      |
| `tool_called`             | yes   | `tool`, `args`                         |
| `tool_error`              | yes   | `tool`, `error`                        |
| `artifact_written`        | yes   | `publish`, `version`                   |
| `human_input_requested`   | yes   | `interaction_id`, `questions`          |
| `human_answers_recorded`  | yes   | `interaction_id`, `answers`            |
| `join_ready`              | yes   | `strategy`, `required`, `failed_branches` |
| `edge_selected`           | no    | `from`, `to`, `condition?`, `negated?`, `loop?`, `iteration?` |
| `budget_warning`          | yes   | `dimension`, `used`, `limit`           |
| `budget_exceeded`         | yes   | `dimension`, `used`, `limit`           |

## artifacts/<node_id>/<version>.json

```jsonc
{
  "run_id": "run_123",
  "node_id": "analyze",
  "version": 0,                          // 0-based, increments per execution
  "data": { "summary": "..." },          // node output
  "written_at": "2026-01-01T00:00:30Z"
}
```

Artifacts are versioned per-node: each loop iteration or re-execution increments the version.
`LoadLatestArtifact` uses the `artifact_index` in `run.json` for O(1) lookups
and falls back to a directory scan for runs created before the index was introduced.

## interactions/<interaction_id>.json

```jsonc
{
  "id": "run_123_review",
  "run_id": "run_123",
  "node_id": "review",
  "requested_at": "2026-01-01T00:01:00Z",
  "answered_at": "2026-01-01T00:05:00Z",  // null until answered
  "questions": { "summary": "..." },       // mapped from upstream
  "answers": { "approve": true }           // filled on resume
}
```

## Compatibility Notes

- `format_version` was introduced in V1. Older data may have `format_version: 0` (zero value) — this is treated as V1-compatible.
- New fields added to any format use `omitempty` and are optional. Tooling should tolerate missing fields.
- Event types may be added in future versions. Consumers should ignore unknown event types rather than failing.
- The `data` field on events is schemaless by design. Known keys are documented above but additional keys may appear.
