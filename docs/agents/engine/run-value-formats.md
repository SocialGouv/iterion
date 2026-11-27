# Run value formats and mutation boundaries

`Run.FormatVersion` is the durable value-format authority. An absent header,
0, or 1 means legacy; 2 means the exact record codec. A future version or a
malformed header is refused. Null, arrays, floating point headers, and repeated
JSON/BSON header keys do not mean legacy. Selector aliases (including Unicode case folds) refuse in reads and Mongo atomic predicates; projections preserve their original spelling. This storage layer is inactive at
runtime: new format-2 births remain disabled until transport, runtime, API and
reader composition is completed and reviewed.

A format binding copies the original run ID and normalized format into the
context. It is not execution, tenant, or financial authority. Delayed work and
retries retain that binding; metadata-only operations can capture a known
format at entry, while payload writers without a binding keep legacy intent.
A same-format execution takeover does not by itself invalidate a format
binding. Execution epochs, receipt targets and business CAS predicates remain
separate requirements.

## Writers

Pre-engine refusal recorders are metadata writers: use `lockExistingRunMetadata`
plus its context-preserving no-relock tail on FS, and
`findOneAndUpdateRunMetadata` on Mongo. A future format or stale original binding
refuses before mutation; unbound writes to a known format remain supported.

Filesystem mutations use the store mutex followed by the stable per-run lock.
The lock file survives deletion and recreation. A read-modify-write operation
holds the exclusion across its read and publication; helpers invoked inside
that scope do not acquire it again. Unknown headers refuse before visible
writes, including sidecars, run logs, tool/session bytes and PID files.
Attachment and scratch streams are staged privately outside the run directory,
without holding the lock while invoking a reader. Publication rechecks the
original format under the same lock. Cancellation is checked around each read;
a blocked external reader still controls when its `Read` returns.

Mongo Run metadata updates combine the captured format's scalar/cardinality
predicate with the original tenant and business conditions in the actual
mutation. The merge state machine's two doors, `ClaimMerge` and
`UpdateRunMergeIf`, also carry the birth their caller loaded
(`store.ExpectedBirth`): the filesystem store compares it under its lock,
Mongo inside the same conditional write, and a successor recreated under the
ID answers `ErrRunDataIdentityChanged` instead of receiving the outcome; `""`
is the historical empty birth. A successor in another tenant is outside a
tenant-scoped query and answers as an absent run does. Other metadata writes
carry no caller birth; under a binding they carry the bound birth
([run-action-origin.md](run-action-origin.md)).
Child collections publish in a transaction that writes the parent.
This parent write conflicts with deletion, recreation or concurrent changes;
a retry reads again but retains the original format. Legacy orphan children
reserve the parent's unique key transactionally against insert-first birth.
No user callback or external blob I/O belongs inside a retryable transaction.

Blob operations select one immutable `blob.RunObjects` view before consuming
a body, granting a local path, or starting remote I/O. Missing exact-format
capabilities refuse before these effects. Metadata publication and removal for
attachments, and attempt indexing, use a transaction on the original parent
after blob I/O. An old operation cannot publish into a successor of another
format. Deletion captures the original tombstone and cleans its own view;
cleanup completion and pruning retain that tombstone's conditions.

| Bytes | Legacy root | Exact root |
|---|---|---|
| Attachments, diff snapshots, attempt snapshots | `attachments/` | `attachments-v2/` |
| Tool bodies | `tools/` | `tools-v2/` |
| Backend sessions and scratch banks | `sessions/` | `sessions-v2/` |
| Tool-produced run files | `runfiles/` | `runfiles-v2/` |

Artifact records use their separate format-aware artifact capability. Upload
staging and IR are not exposed by the RunObjects view. The view exposes scratch
bank operations only when the underlying backend supports them. Its dynamic
method set does not expose client shutdown or unrelated namespaces.
`AttachmentRecord.StorageRef` records the selected canonical key; run file
paths returned by the blob backend remain relative to the run's file area.

Local scratch roots are resolved through their existing physical ancestors
before choosing a sibling namespace. A missing suffix is allowed; a pair of
roots that overlap cannot provide the exact scratch capability and is refused
before a path is granted. Cleanup retains the captured root. This assumes the
operator-controlled filesystem remains cooperative: another process changing
symlinks after capture is not excluded by a Mongo transaction or a path string.

An already-granted filesystem run-file path is also partitioned by format.
A format-1 path cannot overwrite a format-2 successor's run files, and the
reverse direction is isolated too. An already-open run-log descriptor continues
to address its original inode after unlink. Checked log/PID entry points take a
context so delayed callers can preserve the original binding. The historical
`TeeRunLog` wrapper falls back to its incoming logger on an open failure; it does
not expose a typed error. Callers that require admission use
`TeeRunLogContext` instead.

The boundary is inter-format isolation. It does not revoke external handles,
solve same-format ABA, make Mongo and S3 atomic together, or guarantee absence
of old-namespace orphan bytes after a delayed PUT. All old writers must be
upgraded and drained before enabling new-format births; these changes do not
make an old binary honor the new protocol.

A deleted run remains absent to the historical byte readers: sessions, tools,
scratch, diff snapshots and run files preserve their not-found error causes;
`ListRunFiles` returns an empty list without error. Only a known deleted header
gets this compatibility treatment. Unsupported or malformed headers, conflicting
bindings and infrastructure failures remain errors, and writers still refuse a
deleted run. Mongo tombstones retain a format header, so binding validation
precedes the deleted-run compatibility. Historical filesystem tombstones have
no such header: the run ID and context are checked, without inventing an
inter-format comparison after deletion.

## Run listings

The run listings (`/api/runs`, `/api/admin/runs`) ask Mongo one indexed
question per page (`store.RunWindowListingStore`, #3822, #3831): status,
workflow, `updated_at >= since`, a tenant set and the `(created_at, _id)`
keyset, newest first, through `created_id_desc`, `tenant_created_id_desc`,
`tenant_status_created_id_desc` and `tenant_workflow_created_id_desc`. A page
no index serves in that order (a `since` page, or a status or workflow page no
tenant leads) reads in finds of at most 100 documents; every other page is one
find. Known gap (#3863): a tenant set of more than 200 members can still walk,
past Mongo's `internalQueryMaxScansToExplode`. `EnsureSchema` never drops an
index: once every replica and runner pool runs #3831, the operator checks that
`db.runs.getIndexes()` lists `tenant_status_created_id_desc`, then runs
`db.runs.dropIndex("tenant_status_created")`. Both value formats store every
listing key as plain BSON (`TestRunListingKeysEncodeIdenticallyInBothValueFormats`);
each match crosses the decode `LoadRun` uses, and one that does not decode is
reported per id. Repo, bundle and node filters stay in Go and stop the limit
pushdown. Equivalence with the id-by-id scan: `TestRunListingWindowNativeMongo`.

## PR reverse lookup

Mongo derives `inputs_pr_url` from the already encoded Inputs record during
birth and SaveRun. It does not invoke caller codecs a second time. Only the
string value actually materialized by the record qualifies; numbers, bytes,
objects, arrays and nil are not stringified. Explicit null clears an obsolete
projection in the same CAS as the Run.

`ListRunsForPR` reads the legacy and derived indexes in one transaction snapshot,
unions run IDs, then sorts by `(created_at, ID)`. Both queries remain indexable;
this is two reads plus session/transaction work, not one round trip. Its memory
use follows the matching candidates. Mongo's equality can match array elements,
so the reader checks scalar type after validating each candidate's header.
Any transaction, cursor or cancellation error discards all collected IDs.

Mongo validates only the candidates selected by those two indexes; it does not
certify unrelated runs or future payloads in the tenant. The filesystem has no
projection index and must scan run headers: an unknown or malformed run refuses
the entire PR view even when it appears unrelated to the requested URL. It also
enumerates directories before loading records, so permission failures cannot
disappear in a preliminary stat filter. Orphans without a run record remain
skippable. It returns no partial IDs on read failure or cancellation, including
an empty scan and cancellation during the last record read. This stricter filesystem
boundary applies to this PR view; other historical reverse views keep their
existing best-effort behavior.

## Filesystem attachment reads and signed URLs

Filesystem `ListAttachments`, `OpenAttachment` and `PresignAttachment` admit
against the original binding under the store mutex and stable run lock. A
missing binding denotes an independent read of the current known Run; a
binding already supplied cannot adopt a successor of another format. Open
reads the metadata and opens the file before releasing the lock. The returned
descriptor pins that inode, so transfer runs outside the lock and an old
open descriptor continues to read the original bytes after deletion.

Historical absent and orphan legacy attachments remain readable on their
original terms. A deleted filesystem run lists no attachments and opening
returns `ErrAttachmentNotFound`. Presigning can still succeed before bytes
exist or after deletion; it conveys an original format, not a guarantee that
bytes exist. A headerless tombstone has no format to compare, but context and
RunID are checked. If a header remains beside a tombstone, it is validated:
unknown, malformed or conflicting headers cannot disappear behind absence
compatibility. Cancellation and infrastructure failures remain errors.

Legacy local URLs keep `exp` and `sig`, with the same HMAC plaintext, and carry
legacy intent permanently. New exact URLs additionally carry `vf=2`, included
in the authenticated plaintext. The local serving handler uses the optional
`PresignedAttachmentReader` capability through `AsPresignedAttachmentReader`,
including through `Unwrap` decorators. Verification binds the URL's original
format and the actual open rechecks it under the stable lock. Removing or
changing `vf` cannot turn an exact URL into a legacy URL. Duplicate, partial or
malformed signed query parameters refuse rather than taking the unsigned
safe-origin path. A signed read cannot replace a conflicting caller binding.
The historical `VerifyAttachmentSignature` bool API still verifies legacy
MACs; signature validity alone does not admit a read of the current Run.

An unsigned request admitted by the existing safe-origin policy is an
independent current read. Cloud presigned URLs remain served by their blob
backend, outside this local HMAC capability. No same-format ABA protection or
revocation of already open descriptors is added. Direct paths still handed to
the runtime are a separate integration task: this attachment API amendment
does not yet replace those paths with private copies or enable exact births.

## Invocation preparation

See [run invocation preparation and termination](run-invocation-preparation.md)
for prepared bindings, exclusive-birth retirement, and terminal write scopes.
