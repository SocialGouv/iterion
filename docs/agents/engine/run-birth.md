# Store-owned run birth

`Run.BirthID` identifies a durable run creation, independently of
`ExecutionEpoch`. Claims, pauses and resumes rotate the epoch but preserve the
birth; delete/prune/recreate assigns a new birth. Saves preserve historical
absence: an empty caller field preserves stored identity, while a contradictory
nonempty field refuses. Birth identity grants no execution authority. Older
writers are not qualified to preserve it; strong consumers need a writer-version
boundary.

Migration creates a destination-owned birth. Reimport compares the captured
birth and metadata version together; a conflict cannot be retried by adopting
a replacement. Historical absence stays absent. Importing strong child-reference
graphs still requires explicit identity translation or refusal.

## Reservation and publication

`RunBirthStore.BeginRunBirth` is optional and requires an explicit child-format
binding. It reserves advisory liveness before publishing a run row.
`RunBirthReservation.Publish` is exclusive and single-use, including against an
older creator. It never adopts an incumbent. `Unlock` is idempotent and never
removes a published run.

FS preparation locks and unique staging directories live outside `runs/`. A
stage contains complete canonical metadata and the held legacy `.lock` inode.
Publication holds the stable metadata write lock and atomically moves the stage
without replacement. An older `LockRun` either wins first and makes publication
refuse, or opens the already-held published inode. Ordinary `LockRun` is unchanged.
Unknown, corrupt and lock-only directories are not treated as abandoned births.
Linux and Darwin use their atomic primitives; unsupported platforms are
explicitly refused. Syscall failure never authorizes weaker atomicity.

A prepublication crash may leave private staging, but no run shell. No stage
collector is introduced: any future collector must prove ownership has ended
and never unlink a contended stable lock. After publication, a crash leaves
complete metadata and releases the execution lock. Durability syncs follow the
rename.

Mongo takes the external cross-process lease before canonical exclusive insert.
Reservations retain the acquisition request's expiry, serialize publication,
refresh and release, and bound publication by the last acknowledged deadline.
Expired reservations cannot start publication; a refresh acknowledged after the
previous deadline cannot extend it. This is advisory liveness, not fencing for
external effects.

## Errors and integration boundary

A nonnil receipt with an error means publication occurred but its durability or
lifetime was not acknowledged. Retain the receipt for retirement; do not execute.
Cancellation or sync failure after rename cannot turn an owned birth into an
absent run. Conversely, an ambiguous remote commit without a receipt does not
prove absence: reconciliation must tolerate a committed row without a receipt.

All four subbot hosts reserve before preparation. The runview manager may refuse
without publishing a directory. The lease's `PrepareInvocation` consumes the
reservation and retains validated receipts through publication errors. Hosts
publish before recording the parent link, retaining the reservation until
handoff or park. The runview, runner, dispatcher and CLI subbot hosts run their
owned executor cleanup before releasing or parking the lease. Release remains
deferred if cleanup panics; this orders ownership release without promising
that every internal closer completes after a panic.

Parent links remain historical best-effort writes. Strong references must make
link publication authoritative before Engine handoff, validate original parent
and child incarnations and formats, and define historical-link policy. Safe
reattachment and immutable payload ownership remain delivery gates.

A failed existing-run preparation has no validated row to annotate. The runner
logs that leading refusal without retrying a durable write; later pre-engine
failures still require their normal recorder. Child-output readers propagate
scan and artifact errors, including partial-scan failures. Reattachment retains
the link on refusal; partial output is not successful consumption. Strong-reference
guards remain required beyond this error propagation.

## Staged data identity

`RunDataBinding` retains an original header and tenant observation apart
from numeric format and execution authority. Rebinding a successor or other
child refuses;
a format-only context cannot erase data identity. Observed historical absence
differs from no binding. JSON/BSON header parsers reject ambiguous selector
spellings without decoding execution payloads.

`RunDataHeaderStore` exposes metadata-only observation without a `LoadRun`
fallback. It retains existing bindings and checks the requested public ID.
Mongo applies the tenant predicate, projects original selector spellings and
refuses future schemas and deletion markers. FS observes under the stable
metadata lock, checks its deletion marker, and retains the configured-root
boundary rather than adding tenant filtering. Observation neither binds later
payload calls automatically nor authorizes execution; malformed payloads need
not prevent header observation.

`IncarnationDataScope` derives immutable filesystem and blob namespaces outside
historical cleanup roots. A trailing blob separator prevents matching a longer
sibling prefix. Namespace naming alone is not safe payload access.

`RunCreateSpec.DataProtocol` makes the birth choice explicit and independent of
numeric format. Historical zero remains supported. Protocol one still returns
`ErrRunDataCapability` before creation; unknown versions return
`ErrUnsupportedRunDataProtocol`. Only qualification of the complete storage
capability may remove this fence, never a runtime opt-in alone.

FS and Mongo snapshot reads and replacements refuse unsupported protocols
before decoding or overwriting payloads. Header-only observation remains
available. Both full reads and replacements validate retained data bindings.
Replacement independently enforces protocol immutability and a
matching nonempty birth for protocol one, even after admission is enabled.
`SaveRun` cannot create a protocol-one birth. Historical format-only contexts
retain parent-metadata reads during child preparation.

## Staged payload access

The shared FS/Mongo sidecar admission paths retain original birth/protocol
identity and reject stale callers. The FS payload-path resolver covers sessions,
tool blobs, scratch, artifacts, attachments, plans, notes, tags, routes,
interactions, inboxes, events, attempts, turns, logs, run files and Git metadata.
Attachment storage references follow the selected physical path. This lexical
resolver does not itself provide symlink-safe IO or fence older binaries.

[Retained filesystem payload access](retained-payloads.md) records the migrated
operation classes, complete-operation lifetimes, streaming publication and
counterproof boundaries. Roots remain open through composed reads/writes;
cache-key paths are never reopened for strict IO. Run-files grants retain the
initial binding and directory through managed readback and final upload; their
ordinary host-path limits are documented with the retained clients. Protocol1
remains closed; admission-only test overlays qualify staged IO without changing
production admission.

Inbox counters share the selected physical leaf. The optional context-aware
capability reports stale bindings and read errors. The claw hook retains its
Bind-time identity, preserves operation cancellation, bypasses legacy counters
for bound calls, and caches a version only after a successful load.

Mongo inbox readers validate the header and query rows in one snapshot; a
separate preflight can adopt a replacement. Failed transactions return no partial
result. Missing headers permit historical orphan reads only for unbound callers
or retained same-run legacy absence, never foreign or nonempty birth bindings.
The staged selector uses `run_data_v1_user_messages` with a birth-qualified
namespace in row IDs and filters. Append, insert-once, status CAS, existence
checks, readers and cleanup share it. Historical collection and ID shape stay
unchanged. Qualification checks physical collection placement, cross-birth
cleanup and recreation. Mongo inbox version counters remain absent; callers
without that optional capability load directly.

FS event caches are keyed by the selected file and invalidated when another
handle changes its identity, size or modification time. The stable write lock
covers reseeding and append. Every checkpoint synchronizes the selected event
file first, including another handle's writes, and strict access synchronizes
its parent entry. Failure blocks the checkpoint without replacing it.
Incarnation-aware runtime callbacks remain an activation obligation.

## Mongo stream storage

Strict events, logs, counters, plans and notes use `run_data_v1_events`,
`run_data_v1_logs`, `run_data_v1_seq`, `run_data_v1_plans` and
`run_data_v1_notes`. `data_namespace` comes from
`IncarnationDataScope`; tenant authorization is separate. Indexes, counter
keys, tail seeding, plan deduplication and cleanup share that selection.
Event/plan/note counters are independent. Historical collections
and counter-key convergence stay unchanged. Notes have no TTL, even
with events/logs/plans retention.

Writers select from the parent validated in their transaction.
Finite readers validate metadata and dependent rows in one snapshot, including
log anchor and range queries. Errors return no partial slice. Opaque historical
log/plan/note reads preserve missing-header and post-deletion behavior after
identity/format validation. Strict tombstones refuse; structured event readers
retain their deletion refusal. A format-only context is neither an observed
birth nor absent format metadata.

`ScanEvents` retains its original observation and inclusive initial tail.
Snapshot pages commit before callbacks; retries cannot replay a visitor.
Appends cannot extend that tail; seq0 differs from an empty stream. Paging bounds
buffered row count, not payload bytes or total results. Pages are not one
point-in-time view: retention may remove rows, and cancellation or parent change
may fail after earlier callbacks. No successor replaces the original observation.

Strict stream cleanup retains its target during remote IO, deletes selected
rows/counter and leaves historical stream collections alone. Pending markers and retries
govern completion. Historical granular cleanup cannot select strict rows.

Global note-author reattribution covers both collections and retained births;
[account erasure](account-erasure.md) defines partial counts and retry semantics.

Admission stays closed. [MongoSource](run-streams.md) retains one observation;
runtime registries, brokers and HTTP/WS still need scoped integration and proofs.
Org purge covers strict orphans and nested counter tenant keys. Failed or
unacknowledged deletes and cancellation block the identity cascade. Errors
retain known counts; retries resume. Unscoped historical string counter keys
remain unattributable. Streaming activation still needs its own proofs.

## Remaining activation gates

Global note-author erasure walks retained physical roots, including old
and orphaned births without headers; it refuses namespace-directory links and unknown format roots.
Complete cleanup, remaining caller propagation, strong references and
authenticated URL claims remain activation obligations.

The optional blob views `IncarnationRunObjects` and `IncarnationArtifacts`
retain the original scope. S3 refuses other run IDs and uses disjoint keys for
artifacts, attachments, tool blobs, sessions, scratch and run files, including
list/delete and direct S3 presigning. Unsupported clients have no legacy
fallback. Mongo selects views from the original binding, never a fresh lookup
or format-only fallback.

The separate `IncarnationIR` view binds compiled IR and bundle snapshots to the
original namespace and refuses foreign or legacy keys. Mongo queue mediation
and server-signed attachment claims remain unwired. These are staged contracts:
protocol-one declarations do not make incomplete sidecars incarnation-safe.
