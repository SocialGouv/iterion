# Persisted Formats — V1 Reference

This document describes the on-disk formats used by the iterion store.
These formats are considered **stable for V1** — tooling may rely on them.

## Directory Layout

```
<store-root>/runs/
  <run_id>/
    run.json                            # Run metadata & checkpoint
    events.jsonl                        # Append-only event log
    run.log                             # Free-form runtime log (best-effort)
    .pid                                # Detached-runner PID (Phase 2 only)
    artifacts/
      <node_id>/
        0.json, 1.json, ...            # Versioned node artifacts
    interactions/
      <interaction_id>.json             # Human input/output exchanges
```

### .pid (detached-runner mode)

When `ITERION_RUNS_DETACHED=1` is set in the studio server's
environment, runs launched by the server are spawned as detached
`iterion run --background` subprocesses instead of in-process
goroutines. The runner subprocess writes its PID to `.pid` and
removes the file on exit; the studio server reads `.pid` to
re-attach to the runner across its own restarts (e.g. after a
`watchexec` rebuild during development).

Format: a single decimal integer, optionally followed by a newline.
Written atomically via tmp + rename. Absence is meaningful: a
"running" run with no `.pid` is either an in-process run or a
pre-Phase-2 run, and the legacy flock-based reconciliation still
applies. Presence of `.pid` whose PID has died is cleaned up by the
reconciler on next server boot.

## run.json (format_version: 1)

```jsonc
{
  "format_version": 1,               // integer, current = 1
  "id": "01938f4c-78b3-7d2e-bc44-5e6a7b8c9d0e",
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
| `queued`                | Cloud mode only: submitted to the NATS queue, not yet claimed by a runner pod (`RunStatusQueued`, pkg/store/run.go) | —          |
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

The authoritative list is `pkg/store/event.go` (`EventType` constants). Current persisted event types are:

| Type | Node? | Data keys |
|---|---|---|
| `run_started` | no | — |
| `branch_started` | yes | — |
| `branch_finished` | yes | `error?`, `join_node?` |
| `node_started` | yes | `kind` |
| `llm_request` | yes | `model`, `message_count`, `tool_count`, `reasoning_effort?` |
| `llm_prompt` | yes | `system_prompt`, `user_message` |
| `llm_retry` | yes | `attempt`, `delay_ms`, `error?`, `status_code?` |
| `node_recovery` | yes | `code`, `reason`, `attempt`, `delay_ms?`, `error?` |
| `llm_step_finished` | yes | `step`, `input_tokens`, `output_tokens`, `finish_reason`, `tool_calls`, `cache_read_tokens?`, `cache_write_tokens?`, `response_text?`, `tool_call_details?` |
| `llm_compacted` | yes | `before_messages`, `after_messages`, `removed_message_count` |
| `tool_started` | yes | `tool`, `input_size`, `tool_use_id?`, `input?`, `input_preview?`, `input_ref?` |
| `tool_called` | yes | `tool`, `input_size`, `duration_ms`, `tool_use_id?`, `input?`, `output?`, `output_preview?`, `output_size?`, `output_ref?` |
| `tool_error` | yes | `tool`, `input_size`, `duration_ms`, `tool_use_id?`, `error`, `input?`, `output?`, `output_preview?`, `output_size?`, `output_ref?` |
| `artifact_written` | yes | `publish`, `version` |
| `human_input_requested` | yes | `interaction_id`, `questions` |
| `run_paused` | yes | — |
| `human_answers_recorded` | yes | `interaction_id`, `answers` |
| `run_resumed` | no | `resumed_from?`, `restart_node?`, `from_entry?` |
| `join_ready` | yes | `strategy`, `failed_branches?` |
| `node_finished` | yes | `output`, `_tokens`, `_cost_usd` (router nodes may instead emit `selected_route`, `reasoning`) |
| `edge_selected` | no | `from`, `to`, `condition?`, `negated?`, `loop?`, `iteration?`, `expression?` |
| `budget_warning` | yes | `dimension`, `used`, `limit` |
| `budget_exceeded` | yes | `dimension`, `used`, `limit`, `hard_limit?` |
| `run_finished` | no | — |
| `run_failed` | yes | `error`, `code`, `resumable?` |
| `run_cancelled` | yes | `reason` |
| `run_interrupted` | no | `reason` |
| `delegate_started` | yes | `backend` |
| `delegate_finished` | yes | `backend`, `duration_ms`, `tokens`, `exit_code`, `raw_output_len`, `parse_fallback`, `formatting_pass_used`, `stderr?` |
| `delegate_error` | yes | `backend`, `duration_ms`, `tokens`, `exit_code`, `error?`, `stderr?` |
| `delegate_retry` | yes | `backend`, `attempt`, `delay_ms`, `error?` |
| `sandbox_skipped` | no | `driver`, `mode`, `source`, `reason` |
| `sandbox_started` | no | `driver`, `mode`, `source`, `image`, `has_post_create` |
| `sandbox_claw_routed_via_runner` | no | `reason`, `limitations_v1` |
| `network_blocked` | no | `host`, `reason`, `run_id` |
| `sandbox_build_started` | no | `driver`, `dockerfile`, `context` |
| `sandbox_build_finished` | no | `driver`, `target`, `duration_ms` |
| `sandbox_build_failed` | no | `driver`, `error` |
| `preview_url_available` | yes | `url`, `kind?`, `scope?`, `source?` |
| `browser_screenshot` | yes | `attachment_name`, `url?`, `source`, `mime?`, `tool_call_id?` |
| `browser_session_started` | yes | `session_id`, `node_id` |
| `browser_session_ended` | yes | `session_id` |

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
