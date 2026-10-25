# Run provenance and execution attempts

A run keeps its ID across resumes. An **attempt** starts at each persisted
`run_started` or `run_resumed` event. Its immutable snapshot, rather than a
current file or bot-source row, is authoritative for the workflow view
(#2150, F18, #2696).

`pkg/dsl/workflowfile.Snapshot` captures the compilation unit's files, bundle
prompts, transitive prompt includes and presets. The compiler retains the
bytes it reads. A cloud publisher builds the executable AST from those same
frozen files and carries `source_snapshot` in the AST transport envelope;
the runner restores it onto the workflow. Projection resolves those authoring
resources in owned scratch without binding the server's MCP configuration or
credentials. A programmatically built IR can have no source; that is explicit
missing provenance, never permission to substitute a current disk file.

The engine writes an `AttemptSnapshot` before appending the opening event and
before executing nodes. Its SHA-256 ID covers the files, workflow hash,
start time and effective budget. `Run.attempts` is the ordered, append-only
index; the opening event carries its metadata in `data.attempt`. Filesystem
stores place blobs at `runs/<id>/attempts/<digest>.json`; Mongo stores the small
index on the run and the immutable body in its run-scoped blob store. Both
verify the content digest on reads. A failed snapshot write prevents execution
from starting. The old 1 MiB best-effort `workflow_source` capture is not the
snapshot storage limit.

`GET /api/runs/{id}/workflow` selects the latest attempt.
`?attempt=<digest>` selects a historical one. Successful responses include
`attempt`, `provenance` and `workflow_hash`. Editing or deleting a source never
changes an existing attempt. Missing historical blobs do not activate a
fallback; corruption and storage failures are errors rather than synthetic
history. Typed 404 bodies contain `error_code`, `source` and `error`:

- `attempt_not_found`: the requested ID is not indexed by this run.
- `attempt_snapshot_missing`: the indexed blob or its source is absent.
- `bot_source_missing`: a legacy run's named bot source no longer exists.
- `workflow_source_missing`: a legacy local run's authoring source is absent.

Only runs without attempts use the ordered legacy path: captured
`workflow_sources` / `workflow_source` first; then, for stored bots, the exact
recorded bot-source tier and tenant; finally disk for local legacy runs with
no captured source. A legacy captured unit missing external resources can
acquire those resources from its stored bot while preserving the captured
program. This is best-effort legacy reconstruction, not immutable history.
A deleted team source never falls through to a platform bot or a server path.
The service cache uses tenant + workflow hash + captured-content digest, so
identical revisions share a projection and edited legacy rows cannot alias an
older graph. Cross-store authorization still precedes every projection.

Studio selects the attempt before folding events and executions. Opening
sequence numbers are inclusive; the next opening sequence is exclusive, even
when timestamps tie. Historical attempts have their own graph, labels,
metrics, budget snapshot and subbot creation window. A `subbot_attached`
event retains reused children after the operational reattachment pointer is
cleared. Historical sub-bots stay compact with links to their own run history;
inline expansion is reserved for the current attempt so a later child resume
cannot rewrite a parent's historical graph. Selecting another attempt
resets scrub and node selections. The current attempt uses the restamped run
budget, including removal of a cap; the budget meter's used amount remains
run-cumulative because enforcement is run-cumulative. Metric chips show only
the selected attempt's spend. Active executions missing from a legacy graph
are rendered as observed nodes, including when source projection fails.
Loaded events are retained for historical folds; there is no lossy 5,000-event
head truncation. Large run histories therefore consume browser memory in
proportion to the loaded stream.

Witnesses and mutation instructions live beside the tests:
`pkg/runview/workflow_provenance_test.go`,
`pkg/server/run_workflow_provenance_test.go`,
`pkg/server/cloudpublisher/provenance_test.go`,
`pkg/store/storetest/attempts.go`, and
`studio/src/lib/runAttempt{,View}.test.*`.

Validation sites and executed mutations: [delivery evidence](../../reviews/impl-provenance.md).
