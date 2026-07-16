# ADR-075 — IRRef fallback: offload an oversized compiled IR out-of-band at cloud dispatch

- Status: accepted
- Date: 2026-07-16
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

Cloud-mode dispatch marshals a run's compiled IR (the AST `File`) inline
onto the NATS `RunMessage.IRCompiled` field. NATS rejects any message
larger than the server-negotiated `max_payload` (default 1 MiB), so a
workflow whose compiled IR exceeds that budget **hard-failed at enqueue**:
`pkg/queue/nats` pre-checked the marshaled body against `max_payload` and
returned an error that literally read "IRRef fallback (T-42) not yet
implemented"; the runner's `loadWorkflow` symmetrically errored when
`IRCompiled` was empty.

The wire contract already anticipated the fix: `queue.RunMessage` carried
an unused `IRRef` field (`StorageKey` + `Backend ∈ {s3, mongo}`) and
`Validate` enforced "exactly one of IRCompiled / IRRef". Only the two
ends of the pipe — publisher offload and runner fetch — were stubs.

Large IRs are rare (most bots are well under 1 MiB compiled) but not
hypothetical: fan-out/subbot expansions, big `vars` defaults, and
generated workflows push the ceiling. The failure mode was the worst
kind — a clean local run that becomes undispatchable in cloud with an
opaque size error.

## Decision

Implement the fallback as an **out-of-band object reference**, reusing the
S3 blob backend that already holds artifacts and tool blobs (the
ADR-073 cloud-twin pattern), rather than trying to make the IR fit on the
wire.

- **New store seam `store.IRBlobStore`** (`PutIRBlob`/`GetIRBlob`/
  `IRBlobBackend`) + `AsIRBlobStore`, mirroring `ToolBlobStore`. The Mongo
  store satisfies it, backed by `blob.Client` under `ir/<run_id>.json`
  (one object per run — the fallback only ever stashes a single IR).
  Filesystem/local stores deliberately do **not** implement it: the local
  runtime never crosses the queue, so an oversized-IR offload cannot arise.
- **Publisher offloads in `p.publish`** (covers both Launch and Resume).
  It sizes the marshaled `RunMessage` against `Conn.MaxPayload()`; when the
  inline IR would blow the limit it PUTs the IR to the blob store and swaps
  `IRCompiled` for an `IRRef`. A cheap `len(IR)+64 KiB` envelope-reserve
  gate keeps the small-IR hot path off the precise marshal.
- **Runner fetches in `loadWorkflow`**: when `IRCompiled` is empty and an
  `IRRef` is present, it hydrates the IR through `AsIRBlobStore`.
- **Fail loudly, never silently.** An oversized IR with no `IRBlobStore`
  seam is a hard error at publish (not a truncated message); a present ref
  the store can't fetch is a hard error at load. The `nats` guard stays as
  a final safety net for a message that reached publish un-offloaded.
- **Wire-key re-validation.** `GetIRBlob` re-derives the canonical
  `ir/<run_id>.json` key from the wire `StorageKey` and rejects any
  mismatch, so a tampered queue reference can never widen the S3 key space
  (same containment discipline as `RunFileKey`).

The `Backend` enum keeps `mongo` as a documented option, but the shipped
impl is `s3` — the blob backend is where run-scoped bytes already live.

### Alternatives rejected

- **Chunk the IR across multiple NATS messages.** Rejected: reassembly,
  ordering, and partial-failure semantics on a work-queue stream that is
  designed for one-message-one-unit-of-work; a large multiplier of
  complexity for a rare case.
- **Compress the IR inline (gzip on the wire).** Rejected: buys only a
  constant factor — a large enough workflow still exceeds `max_payload` —
  while adding a decode branch and an opaque binary blob on every message.
  The out-of-band ref removes the ceiling entirely.
- **Store the IR in Mongo (GridFS / a document).** Rejected: a compiled IR
  can pass the 16 MiB BSON ceiling, and the S3 blob path already models
  run-scoped byte blobs (artifacts, tool bodies, run files) — mirroring
  ADR-073's tool-blob choice.

## Consequences

- Workflows whose compiled IR exceeds `max_payload` dispatch and run in
  cloud instead of failing at enqueue. The common (small-IR) path is
  unchanged — no extra marshal, no blob write.
- The oversized path adds one S3 PUT at publish and one S3 GET on the
  runner's cold path, and couples oversized-IR dispatch to blob-backend
  availability (a blob outage surfaces as a loud publish/load error, which
  is the intended behaviour).
- New S3 key space `ir/<run_id>.json`, swept by the per-run `DeleteRun`
  cleanup alongside artifacts/tool-blobs/run-files.
- `Conn.MaxPayload()` is now exposed on the queue connection; the publisher
  holds a `maxPayload func() int64` seam so the offload decision is unit
  testable without a live NATS server.
- `migrate to-cloud` does not copy the new `ir/` key space; consistent with
  ADR-073's note that the blob spaces are not yet migrated (captured as a
  board finding).
