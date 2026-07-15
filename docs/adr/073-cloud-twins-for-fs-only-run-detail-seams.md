# ADR-073 — Cloud twins for the three filesystem-only run-detail store seams (turns, tool blobs, artifact files)

- Status: accepted
- Date: 2026-07-12
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

Three optional `store.RunStore` seams were implemented only on
`FilesystemRunStore`, so the features anchored on them silently degraded
or 503'd for cloud runs — the same shape as the Commits/Files panels
before their Mongo twin (ADR-067) and plan snapshots before theirs
(`run_plans`). Each was documented as a known limitation on
[pkg/store/iface.go](../../pkg/store/iface.go) but was a real cloud hole:

1. **`TurnStore`** (per-LLM-turn checkpoints). No Mongo impl →
   `AsTurnStore` returned nil, so `iterion fork` reported "cloud stores
   not yet supported" and the per-turn capture hook
   (`model/hooks.go` `turnSink`) skipped silently. Fork-from-a-prior-turn
   and the studio per-node timeline did not work in cloud.
2. **`ToolBlobStore`** (large per-tool-call I/O bodies). No Mongo impl →
   `GET /api/runs/{id}/tools/{toolUseID}/{kind}` returned 503. Mitigated
   (events carry a ~4 KB inline preview so the studio mostly doesn't
   fetch), but *expanding* a large tool output failed.
3. **`RunFilesStore`** (tool-produced artifact files: run reports, SBOMs).
   No Mongo impl → the Artifacts panel was empty and
   `/artifact-files/{path}` 404'd. Tool-produced files were invisible in
   cloud.

All three are wired at their call sites through the `AsXStore(store)`
optional-interface pattern, so the design constraint was: **implement the
Mongo methods and the features light up automatically**, no call-site
rewrites. Each must be tenant-scoped, `DeleteRun`-cleaned, and have
TTL/GC parity with the sibling derived-observability streams.

The seams differ in one axis that drove the storage choice: **where the
bytes live and how big they get.**

## Decision

Ship all three as Mongo/S3 twins, choosing the backing store per seam by
size and write topology, and prove each against the shared
`pkg/store/storetest` conformance suite run against **both** backends.

### 1. TurnStore → Mongo `run_turns` collection (messages inline)

Mirrors the `run_plans` precedent exactly: one tenant-stamped document
per `(run_id, node_id, loop_iter, turn_index)` (the idempotent-overwrite
key `WriteTurn` upserts on), unique + tenant-compound indexes, and a TTL
on the top-level `ts` date field sharing the `eventsTTLDays` knob with
events/run_logs/run_plans. `DeleteRun` sweeps the collection.

The claw message blob (`TurnCheckpoint.Messages`, the accumulated
conversation Fork rehydrates from) is stored **inline** in the document,
not in S3. Rationale: a run's message history is well under the 16 MiB
BSON ceiling, and keeping it in the same document makes `WriteTurn` a
single atomic upsert with no cross-store ordering to reason about. The
read methods (`LoadTurn`/`ListTurns`/`LatestTurn`/`LoadTurnAtIndex`)
project the blob **out** and return `Messages == nil`, mirroring the fs
reader (which never inlines the sibling `messages.json`); only
`LoadTurnMessages` fetches it.

The runner's `metricsEmitter` wrapper forwards `WriteTurn` to the inner
store (like `AppendPlanSnapshot`), so the capture hook's
`emitter.(TurnWriter)` assertion resolves through the wrapper instead of
seeing the method-less wrapper and disabling capture.

### 2. ToolBlobStore → S3 (`tools/<runID>/<toolUseID>/<kind>`)

Tool bodies are exactly the payloads that *exceed* the inline preview
threshold and routinely pass the 16 MiB BSON ceiling (a large command
output is the whole reason the sidecar exists), so they live in S3, not
Mongo — the events collection keeps only the small preview + ref.
`blob.Client` gains `PutToolBlob` / `GetToolBlobRange` / `DeleteRunToolBlobs`.
`GetToolBlobRange` does a portable HEAD-then-Range-GET (a 416 from a bare
Range on an over-offset request carries a gateway-dependent Content-Range
we don't want to parse across S3/MinIO). A missing blob maps to an
`os.ErrNotExist`-compatible error so the HTTP surface 404s, matching the
fs store. Tenant isolation rides the run-document layer: the handler
loads the run under the caller's tenant ctx before fetching the blob
(same gate as `OpenAttachment`); S3 keys are not tenant-prefixed, exactly
like artifacts + attachments. `metricsEmitter` forwards `WriteToolBlob`.

### 3. RunFilesStore → S3 with a runner-side scratch→S3 bridge

This seam is the only one whose **write target and read source
legitimately differ**, because `EnsureRunFilesDir` must return a real
local directory to bind-mount into the sandbox, and the sandbox runs on
the **runner** pod while the panel is served by the **server** pod:

- `EnsureRunFilesDir` returns a runner-local scratch dir
  (`Config.RunFilesScratchDir`, default `<tmp>/iterion-runfiles`) — tools
  write files exactly as on the fs backend.
- A new optional companion seam, `store.RunFilesUploader.UploadRunFiles`,
  walks that scratch tree after the run and PUTs each file to S3 under
  `runfiles/<runID>/<relPath>` (reusing the attachments blob path). The
  runner calls it post-run alongside `recordRunGitMeta`, best-effort on a
  tenant-scoped background ctx.
- `ListRunFiles`/`OpenRunFile` read from S3, so the server pod — which
  never saw the runner's disk — serves the panel.

`RunFileKey` sanitises each segment of the multi-segment relative path
and rejects `..`/absolute/empty — the containment invariant the fs
`OpenRunFile` enforces via its openat walk, applied to the flat S3 key
space. `DeleteRun` sweeps `runfiles/<runID>/` **and** the local scratch
dir. `FilesystemRunStore` deliberately does **not** implement
`RunFilesUploader` (its scratch dir already *is* the read source), so
`AsRunFilesUploader` returns nil there and the runner no-ops.

### Alternatives rejected

- **Turn messages in S3.** Rejected: adds a cross-store write + ordering
  concern for a blob that fits comfortably in a Mongo document; `run_plans`
  set the inline precedent.
- **Tool blobs / artifact files in Mongo.** Rejected: both can exceed the
  16 MiB BSON ceiling; S3 is the size-appropriate home and the attachments
  path already models file blobs.
- **Live streaming of cloud artifact files** (incremental upload during
  the run). Deferred: run reports/SBOMs are produced at the end, so a
  post-run upload makes them visible at completion — the acceptance bar
  ("identical for a finished cloud run"). Live visibility during a running
  cloud run is a follow-on.

## Consequences

- Fork-from-turn, the per-node timeline, full tool-I/O expansion, and the
  artifact-files panel work for a finished cloud run identically to local.
  No call-site changes were needed — the `AsXStore` seams lit up.
- New Mongo collection `run_turns` (TTL-parity with events); new S3 key
  spaces `tools/<runID>/` and `runfiles/<runID>/`. All three are
  `DeleteRun`-cleaned and tenant-scoped (Mongo docs by `tenant_id`, blobs
  by the run-document tenant gate).
- Cloud artifact files become visible at run completion, not streamed
  live — documented on the `RunFilesStore` interface.
- The shared conformance suite (`pkg/store/storetest`) gained
  `TurnStore` / `ToolBlobStore` / `RunFilesStore` cases and is now run
  against the filesystem backend too (`TestConformance_FilesystemShared`),
  so every seam the cloud twin must satisfy is proven identical on both
  backends in-tree without a live Mongo. The RunFilesStore case bridges
  via `RunFilesUploader` when present, so the same assertions hold on fs
  (no bridge) and Mongo (scratch→S3); a mongo white-box test models the
  runner→server two-pod handoff over a shared in-memory blob.
- `migrate to-cloud` does not yet copy the three new blob/collection
  spaces — a follow-on (the tool captured it as a board finding).
