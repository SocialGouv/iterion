# Persisted formats — V1 reference

This page describes the filesystem run store. Mongo/cloud uses equivalent
documents and collections rather than reproducing the directory layout. The
current Go structs and constants under [`pkg/store/`](../pkg/store/) are the
authoritative schema; readers must tolerate additive fields and event types.

## Filesystem layout

```text
<store-root>/runs/<run_id>/
  run.json
  events.jsonl
  run.log
  user_messages.jsonl
  .pid
  artifacts/<node_id>/<version>.json
  interactions/<interaction_id>.json
  attachments/<name>/<original_filename>
  attachments/<name>/meta.json
  plans/<sequence>.json
  tools/<tool_use_id>/input
  tools/<tool_use_id>/output
```

Entries are created only when the feature is used. Files and directories are
private by default (`0600` / `0700`). Large tool inputs and outputs are kept in
`tools/`; their events carry previews and references instead of forcing the
whole body into `events.jsonl`.

### Detached-runner PID

With `ITERION_RUNS_DETACHED=1`, a server-launched run executes in a managed
`iterion run --background` process. `.pid` contains one decimal PID and is
removed on normal exit. On restart, the server uses the PID plus run locking
and event freshness to reattach or reconcile an orphan. Absence is valid for
in-process and historical runs.

## `run.json` (`format_version: 1`)

`run.json` contains the run's identity, source, lifecycle, effective launch
metadata, worktree/finalization state, cloud ownership/queue metadata,
attachments, artifact index, and checkpoint. A deliberately abbreviated
example is:

```jsonc
{
  "format_version": 1,
  "id": "01938f4c-78b3-7d2e-bc44-5e6a7b8c9d0e",
  "name": "kind-otter",
  "workflow_name": "review",
  "workflow_hash": "<sha256>",
  "file_path": "/repo/review.bot",
  "bundle_path": "",
  "preset": "strict",
  "status": "running",
  "inputs": { "scope": "pkg/runtime" },
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:01:00Z",
  "finished_at": null,
  "error": "",
  "checkpoint": { "node_id": "inspect" },
  "artifact_index": { "inspect": 2 }
}
```

The complete shape is [`store.Run`](../pkg/store/run.go). `omitempty` fields
from local/cloud, worktree, webhook, secret, attachment, budget, and Studio
features can legitimately be absent.

`nodes_served` maps each LLM node's id to the last `(backend, model)` that
served it (`model` is the provider-reported effective model; `declared_model`
is what the node asked for). It is the run-record half of making a finished
run self-describing without replaying `events.jsonl`. Empty for legacy runs
and for workflows that never delegated. The event stream is the full history
(`delegate_started` carries `declared_model`; `delegate_finished` /
`delegate_error` add `effective_model` / `context_window` /
`max_output_tokens`; `model_drift` fires when the two model fields name
different models).

### Run statuses

| Status | Meaning | Resume posture |
|---|---|---|
| `queued` | Cloud message accepted but not yet claimed by a runner. | Internal queue state. |
| `running` | An engine owns or is expected to own the run. | Not normally resumable; stale local runs can be reconciled. |
| `paused_waiting_human` | A durable interaction is waiting for answers. | Resume with answers. |
| `paused_operator` | Soft operator or daily-spend-cap pause with no pending human form. | Runtime-restorable checkpoint; see [resume](resume.md). |
| `finished` | Reached `done`. | Terminal. |
| `failed` | Intentional `fail` or failure without resumable persisted state. | Terminal (no auto-resume), but the checkpoint is preserved so an explicit `rewind` can recover it; runs failed before that preservation have none and stay unrecoverable. |
| `failed_resumable` | Failure with a restart checkpoint, or an entry restart marker. | Resume without answers. |
| `cancelled` | User interruption with state preserved when possible. | Resume without answers. |

`failed_resumable` and `cancelled` are terminal for polling purposes even
though an explicit resume can transition them back to `running`.

## Checkpoint

The checkpoint embedded in `run.json` is the source of truth for resume.
`events.jsonl` is an audit/observation stream and is never replayed to rebuild
execution state. A checkpoint may be present while a run is still `running`
because it is saved best-effort after successful node boundaries; it is
preserved for resumable/cancelled/paused states and cleared on ordinary
finished/failed transitions.

```jsonc
{
  "node_id": "review",
  "interaction_id": "run_123_review",
  "outputs": { "inspect": { "summary": "..." } },
  "loop_counters": { "retry": 2 },
  "round_robin_counters": { "alternate": 1 },
  "loop_previous_output": {},
  "loop_current_output": {},
  "artifact_versions": { "inspect": 3 },
  "selected_incoming": {
    "gate": [{ "from": "validate", "to": "gate" }]
  },
  "vars": { "scope": "pkg/runtime" },
  "interaction_questions": { "approved": "Ship?" },
  "backend_session_id": "session-id",
  "backend_name": "claude_code",
  "backend_conversation": null,
  "backend_pending_tool_use_id": "",
  "node_sessions": {
    "writer": {
      "backend": "claude_code",
      "session_id": "sess-…",
      "fingerprint": "…",
      "state_ref": "ulid"
    }
  },
  "backend_session_state_ref": "",
  "node_attempts": { "review": { "RATE_LIMITED": 1 } },
  "budget_tokens_used": 42000,
  "budget_cost_usd": 1.25,
  "budget_iterations_used": 7,
  "budget_elapsed_ns": 90000000000,
  "cost_usd_total": 1.25
}
```

The loop snapshots preserve `loop.<name>.previous_output`; backend fields
preserve mid-agent interaction; recovery counters keep retry ceilings honest;
budget fields prevent resume from granting a fresh allowance.
`selected_incoming` is the set of incoming edges routing actually fired
into each node for its current visit, so a resume of that node applies
the same with-mappings (issue #484). Missing fields on historical
checkpoints take their zero-value compatibility behavior — for
`selected_incoming` that means the pre-#484 fallback of merging every
incoming edge whose source has produced output.

## `events.jsonl`

Each line is one [`store.Event`](../pkg/store/event.go), ordered by a monotonic
per-run `seq`:

```jsonc
{
  "seq": 12,
  "timestamp": "2026-01-01T00:00:30Z",
  "type": "node_finished",
  "run_id": "run_123",
  "branch_id": "",
  "node_id": "inspect",
  "data": {},
  "log_offset": 1842,
  "active_ms": 23000
}
```

The current persisted event vocabulary is grouped below. Payload keys are
event-specific and additive; consult comments beside the constants and the
emitter when a consumer needs an exact payload contract.

| Family | Persisted event types |
|---|---|
| Run lifecycle/control | `run_started`, `run_paused`, `human_input_requested`, `human_answers_recorded`, `interaction_answered`, `run_resumed`, `run_auto_resumed`, `run_retry_scheduled`, `run_workspace_reset`, `run_steered`, `run_health`, `run_finished`, `run_failed`, `run_cancelled`, `run_interrupted` |
| Graph/budget/artifacts | `branch_started`, `branch_finished`, `branch_abandoned`, `node_started`, `node_recovery`, `node_verified_action`, `node_finished`, `edge_selected`, `join_ready`, `budget_warning`, `budget_exceeded`, `budget_exit_grace`, `artifact_written`, `plan_written` |
| LLM, delegation, and tools | `llm_request`, `llm_prompt`, `llm_retry`, `llm_step_finished`, `assistant_text`, `llm_compacted`, `tool_started`, `tool_called`, `tool_error`, `delegate_started`, `delegate_finished`, `delegate_error`, `delegate_retry`, `model_fallback`, `model_drift` |
| Review gate | `review_turn`, `review_verdict`, `review_merged` |
| Sandbox/network | `sandbox_skipped`, `sandbox_started`, `sandbox_claw_routed_via_runner`, `sandbox_host_state_mounted`, `sandbox_user_remap`, `sandbox_uid_mismatch_warning`, `sandbox_devbox_provisioned`, `sandbox_workspace_export_failed`, `network_blocked`, `sandbox_build_started`, `sandbox_build_finished`, `sandbox_build_failed` |
| Browser/preview | `preview_url_available`, `browser_screenshot`, `browser_session_started`, `browser_session_ended` |
| Operator messages | `user_message_queued`, `user_message_delivered`, `user_message_consumed`, `user_message_cancelled` |
| Worktree / repo bank | `worktree_branch_failed`, `run_bank_refused`, `run_bank_superseded` |

`alert` is deliberately not persisted: it is an ephemeral broker event.
`run_health` is its persisted, replayable counterpart. A single torn JSONL
tail line is tolerated; widespread corruption returns the typed
`ErrEventsCorrupted` rather than presenting a partial audit as complete.

`budget_exit_grace` is the audit record of a **deliberate** overspend: the
node ran on a cap that was already spent, inside the bounded exit grace, so
the run could reach a terminal node and deliver work it had paid for. Its
`data` carries `{dimension, used, limit}` — the axis that overran and *its
own* used/limit pair, never another axis's — with the graced node in the
event's own `node_id`. On the sequential path it is deduplicated per
`(node_id, dimension)`; a fan-out emits one per branch boundary instead,
because branches run concurrently and one event per boundary is the honest
audit. `iterion report` renders it, so an operator never has to open the raw
stream to learn that a run spent past what it declared. See
[dsl.md](dsl.md#budget-and-loop-back-edges).

## Artifacts

`artifacts/<node_id>/<version>.json` stores published node outputs:

```jsonc
{
  "run_id": "run_123",
  "node_id": "inspect",
  "version": 0,
  "data": { "summary": "..." },
  "labels": ["review", "runtime"],
  "written_at": "2026-01-01T00:00:30Z"
}
```

Versions are zero-based and increment on publication. `artifact_index` in the
run record accelerates latest-version lookup; older records fall back to a
directory scan.

### Deployment-report output contract

Reserved output keys any workflow can emit to declare a **delivery**. The
run-view reducer ([`pkg/runview/snapshot.go`](../pkg/runview/snapshot.go),
`recordDeployment`) folds them out of `node_finished` into
`RunHeader.deployment`, and the studio renders them as the run header's
deployment row. The seam is the **field names** — no bot name, node name or
manifest flag is involved, so any bot that reports a deployment lights it up.

Two groups, recognised independently so a bot may emit them from one node or
split them across a deploying agent and a deterministic traceability gate
(the [app-dev](../bots/app-dev/main.bot) shape):

| group | recognised by | fields |
|---|---|---|
| delivery | `deployed_url` present | `deployed` bool · `healthy` bool · `deployed_url` string · `image_ref` string · `commit` string · `notes` string |
| traceability | `verifiable` present **and** at least one of `pushed` / `image_from_repo` / `built_from_head` | `verifiable` bool · `pushed` bool · `image_from_repo` bool · `built_from_head` bool · `commit` string · `trace_log` string |

Last-write-wins per group: a redeploy loop re-reports both, and the final
attempt is the run's outcome. A node output carrying neither key contributes
nothing, so a run that deploys nothing carries no `deployment` at all.

**`verifiable` is the meta-fact and it is load-bearing.** `false` means the
gate could not establish the three traceability facts (git unreachable, gate
miswired) — an environment fault, *not* a verdict against the deploy, and the
studio renders it as its own state rather than as a failure. The three
booleans below it carry no information when it is false.

The traceability group exists because liveness is necessary and not
sufficient: an app served from a ConfigMap on a stock base image answers 200
and reports every liveness field honestly while nothing was pushed and nothing
is reproducible. A delivery must *also* be traceable — commits reachable from
a remote branch, and the running image published under the repo's own registry
path, naming the deployed commit.

## Interactions

`interactions/<interaction_id>.json` records the durable question/answer
exchange. Review gates additionally retain the ordered companion↔human turns:

```jsonc
{
  "id": "run_123_review",
  "run_id": "run_123",
  "node_id": "review",
  "requested_at": "2026-01-01T00:01:00Z",
  "answered_at": null,
  "questions": { "approved": "Ship?" },
  "answers": {},
  "turns": [
    { "role": "companion", "content": "Run the smoke test.", "at": "..." },
    { "role": "human", "content": "Passed.", "at": "..." }
  ],
  "tenant_id": ""
}
```

The checkpoint embeds pending questions as a resilience fallback if the
separate interaction record is lost.

## Attachments, plans, tool blobs, and messages

- Attachment bytes live under `attachments/<name>/`; metadata is mirrored in
  the run record and a sidecar so the filesystem can be re-indexed. Cloud uses
  object storage with an opaque `storage_ref`.
- `plans/<sequence>.json` contains deduplicated snapshots of an agent's living
  todo list, ordered by zero-padded sequence.
- `tools/<tool_use_id>/{input,output}` contains large exact tool bodies.
- `user_messages.jsonl` is the durable operator-message inbox; companion events
  expose queue/delivery/consumption transitions in the run timeline.

## Compatibility rules

- Missing or zero `format_version` is treated as V1-compatible.
- New struct fields are additive and normally optional; do not reject unknown
  JSON fields in external readers.
- Event consumers must ignore event types and payload keys they do not know.
- `Event.Data` remains schemaless by design.
- Filesystem paths are an implementation of store interfaces, not a portable
  cloud storage contract.
