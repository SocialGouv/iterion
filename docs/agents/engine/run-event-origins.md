# Event origins in producers, readers and broker scopes

Companion to [run-streams.md](run-streams.md) and [run-cursors.md](run-cursors.md).

`store.RunObservation` copies the selected parent header, tenant and presence within the owning store. Unknown (the zero value) differs from observed historical absence. This is data provenance, not authorization, store-instance identity or execution ownership. A historical parent replaced with an identical header remains indistinguishable; no epoch is invented.

`Event.EventObservation` exposes a value copy. `WithEventObservation` replaces any previous observation on an event copy; it neither clones mutable Data nor establishes storage authority. Owned JSON/BSON decoders clear private provenance by decoding into a fresh event before assignment. Malformed input leaves the destination unchanged. The observation is absent from wire records; arbitrary third-party decoder reuse is outside this guarantee.

Filesystem append captures the selected parent under its write lock and stamps the returned event only after publication. Mongo captures the parent before callbacks/encoding and compares it to the transaction-selected parent before publishing. Transaction retries do not repeat projection callbacks. A loaded event and the append return can therefore differ in private provenance even when their wire representation is equal.

`AppendObservedEvent` preserves the actual outer appender. `ObservedEventProducer` explicitly validates that receiver: inheriting a capability through embedding while overriding AppendEvent does not admit a wrapper. An intentional wrapper must implement the contract itself, preserve context and validate before its own observer dispatch. No generic unwrapping bypass is provided.

For this helper, a private context marker requests validation of the selected parent before native payload creation or callbacks. A present filesystem parent's canonical empty tenant stays empty; a tenant-bearing caller is refused instead of rewriting that identity. Plain AppendEvent retains its historical behavior. The helper also validates the returned run, observation, format and original binding before returning to its caller. It does not adopt a missing strict binding. A broken custom producer may already have written before this post-check refuses; there is no rollback guarantee.

`WithEventProjection` supplies invocation-owned log position and active duration. The callback must use captured resources, never rediscover a current run by public ID; filesystem execution remains under the write lock. Nonzero caller fields are preserved, both prefilled fields avoid calling the projection, and a refused projection never falls back to the historical ID-only callbacks.

These seams do not activate scoped runtime delivery. Store-instance and resource-handle ownership, registries and the default Service stream wiring of logs, alerts and stores other than the native filesystem one, wrapper observers and HTTP/WS authorization remain separate integration work. In particular, a reader's authorization tenant must not be silently treated as the canonical stored tenant. Protocol-one admission remains disabled until the complete migration is qualified.

## Retained readers and delivery

Native filesystem and Mongo selections capture a separate observation alongside
request authorization context. A present parent's tenant comes from canonical
metadata captured with the header, never from the event row or a later lookup.
Retained validation compares that original tenant as well as the existing
header and domain identity. The filesystem parser reads the same retained raw
header bytes without decoding execution payloads. Mongo's retained-reader
policy preserves top-level tenant-key cardinality before projection; ordinary
writer and cleanup metadata policies remain unchanged.

Missing and empty canonical tenant strings are historical representations;
ambiguous aliases, repeated keys, null and nonstring values are refused by this
retained-reader policy. An absent filesystem parent has no stored tenant:
its observation's tenant records the original request partition only. Orphan
bytes remain stored by public ID, so two request tenants can read the same
bytes with different absent observations. This establishes neither physical
isolation nor authorization and cannot enable scoped Service admission.

The optional observed-selection contract endorses the actual receiver, without
unwrapping. An inherited capability cannot bypass a wrapper's overridden
behavior. Native consumers capture the admitted observation once and verify
every event in a batch before delivery or cursor advancement. Unknown or
mismatching origins from an opted-in selector fail permanently. Legacy custom
selectors lacking this opt-in remain usable outside the observed guarantee.

The filesystem observed decoder fills an otherwise accepted missing/empty
RunID from the retained stream ID; contradictory IDs are refused. This is an
explicit emitted-field compatibility change for observed FileSource delivery.
The direct historical DecodeEvent path retains its empty-ID behavior. Other
fields, exact numeric payloads and the private observation's wire omission are
preserved; a format-one stream line keeps its numbers as written
(`json.Number`), never through float64.

Decoding can stamp retained A bytes as A after storage changes: it does not
reload B to repair the stamp. Existing snapshot validation and pre-delivery
validation still control delivery. Validation and external filesystem mutation
are not an atomic transaction. Replay keeps the first deterministic refusal,
including when a custom backfill returns nil or cancellation after callback
false, and cannot deliver a later batch or advance the cursor after that
refusal. A closed delivery pipe stops the producer rather than restarting the
replay.

This integration covers retained FileSource and selected Mongo delivery, and
the Service's StreamSource events on the native filesystem store, one retained
FileSource subscription per client ([run-ws-origin.md](run-ws-origin.md)). It
does not by itself migrate other Service registries, captured resource
handles, ephemeral alerts, logs or HTTP/WS authorization. Those paths still require
their own original-observation integration before scoped admission is enabled.

## Observed broker scopes

The additive EventBroker.AcquireObserved API creates an owner lease in that
broker instance. It admits known present observations only; historical empty
BirthID remains supported with the indistinguishable-replacement limitation.
Absent filesystem request partitions are refused. Admission validates routing
data, not authorization or a producer's truthfulness.

Different acquisitions of the same observation share a live topic but own
distinct leases. Copies of one handle share its private release state. A
subscription adds no owner reference: Cancel closes only that subscription;
the last owner release closes the topic. Late old cancellation or release
cannot retire a replacement topic. Already buffered events remain readable
after channel closure. These handles do not add an execution epoch.

Scoped Publish checks the event's observation and RunID before any delivery,
including with no subscribers. Matching ephemeral alerts with sequence zero
are supported. Unknown or mismatching events refuse without counting as slow
consumer drops. Sends remain nonblocking, with per-subscriber drop counters.
Each subscriber receives its own Event value, while referenced Data objects
remain borrowed and read-only; there is no deep payload clone.

Legacy Publish, Subscribe, CloseRun and SubscriberCount remain confined to the
legacy table, even for stamped events. Neither direction publishes into the
other table implicitly. The explicit Service feed below uses this capability. Default StreamSource
still requires wrapper observers, alerts, captured log/resources and HTTP/WS
authorization to be composed before its scoped delivery is enabled.


## Explicit Service observed feeds

Service.AcquireObservedEventFeed accepts an already-authorized retained
RunObservation. NewService owns one native filesystem bridge, created after
options and store construction; no producer starts before acquisition.
Unsupported Mongo, wrapped/custom stores and a configured borrowed StreamSource
refuse this API while retaining existing Service behavior. An observation does
not establish authorization. No later LoadRun repairs the supplied original.

This feed is lossy fan-out: a new producer replays its backlog, but a client
joining an existing producer receives only post-registration events. Drops is
per client. It is not a cursor/replay subscription and does not replace
StreamSource, WS, alerts or logs. Referenced payloads remain borrowed/read-only.

Done and Err describe the shared producer. Non-last Close closes the client's
Events and releases its reference while Done can remain open. Await Done for
the final Err; already queued original events remain readable after channel
closure. Last Close joins owned cleanup. Retiring entries block same-observation
replacement until their subscriptions and source cleanup finish, including a
client whose physical Cancel is still in progress.

StopBackground closes admission under the reservation mutex before its other
shutdown work, cancels all captured entries including pending acquisitions,
and waits within the supplied context. Stop and Drain use the same path. The
live StreamSource event streams stop the same way, with
`runstream.ErrSourceClosed`; Drain stops those last, after the drained runs'
own streams end (bounded).
A deadline is reported as a warning, not successful cleanup; entries remain
tracked and a subsequent stop can finish joining. Cancellation happens even
when the join budget has already expired. Already-completed cleanup succeeds.
Post-stop acquisition is refused; cancellation may be reported before the
closed-capability check, so no single post-stop error type is promised.

Only owned sources/subscriptions are closed: the store, configured borrowed
source, external broker owners and running engines are not owned by this feed.
Remaining AfterFunc callbacks may finish idempotent captured-entry bookkeeping;
the guarantee does not join every callback goroutine. StopBackground preserves
run status; Drain retains its existing interruption rules. Explicit feeds may
end before Drain emits its final legacy interruption event. Protocol-one
admission and the default stream migration remain separately gated.

## Snapshot scan binding

BuildSnapshot loads metadata through LoadRunWithValueFormat, then scans events
with that run's observation — its header and the tenant its store recorded —
bound into the context. A native filesystem or Mongo store then refuses a run
recreated under the same ID with a different header, or in another tenant,
between those two reads, as it already refused a deleted one: the HTTP and WS
snapshot fails instead of folding a successor's events into the loaded
metadata. A scan failure after events reached the reducer returns no snapshot.
Metadata naming another run, or none, is refused before the scan; a custom
store that ignores its context gets no guarantee.

The binding covers this function's own two reads; a caller that already
carries a binding — the WS socket's origin, [run-ws-origin.md](run-ws-origin.md)
— has both reads compared with it, and a header-only re-bind keeps its tenant.
Each native read compares that tenant with the one the strict parser reads
from the same record (a duplicate, case-alias or non-string `tenant_id` is
`ErrInvalidRunDataIdentity`); a tenant-scoped Mongo query does not see a
successor in another tenant, which answers as absent. Historical runs whose
births are both empty stay indistinguishable. Service.SnapshotCtx's merge
reconciliation writes only to the birth it loaded (`store.ExpectedBirth`).
`store.BindRunObservation` is the shared compare-only helper for such a
binding. Protocol-one admission stays off.
