# Run stream selections and source lifetime

Storage namespaces and cleanup: [run birth](run-birth.md). Protocol-one
admission remains closed; the resolver also serves historical subscriptions.
HTTP/WS provenance, brokers and runtime registries still need their own
integration.

## One observation per Mongo subscription

The server composes `NewMongoResolved` with the store's
`ResolveStreamSelection`, through a structural interface. Runstream does not
import the Mongo store, and generic store interfaces carry no Mongo types.
Resolve runs once per subscription. Supplied bindings are compared; an unbound
initial reader captures actual metadata in a snapshot. Selection retains
original identity, format, namespace, parent presence and authorization.
Reconnects revalidate that observation; they never select a successor's codec,
collection or tenant. A privileged caller still selects the parent's canonical
tenant. Returned filters cannot mutate the retained selection.

A present historical header with an empty birth differs from an absent parent.
Neither becomes an invented format-one binding. Headerless opaque logs preserve
format-only intent; structured event reads retain their stronger requirements.
A later parent appearance refuses an observed absence. Historical empty-birth
selectors cannot distinguish replacement by an old writer with identical
identity fields: the writer-version boundary still applies. Qualified writers
mint a new birth, which the retained observation detects. This grants data
access, not execution ownership or a new epoch fence.

The old `NewMongo` constructor remains available for unbound historical use. It
refuses every supplied data binding, including protocol zero, and explicit
format-two intent: raw collection pointers cannot honor those capabilities.
Missing logs remain `ErrLogsUnsupported`. Production uses the resolver seam.

## Replay and live delivery

Every attempt opens its watch before backfill. Replay uses the retained
selection's snapshot methods, including log anchor plus dependent rows in the
same snapshot. Events keep an inclusive initial tail and an explicit
inclusive/exclusive cursor, avoiding sequence arithmetic at `MaxInt64`. Logs
retain an initial end and the chunk straddling the requested offset. Pages
bound buffered row count, not total history or payload bytes. They are separate
snapshots: retention can remove rows between pages, and a later error can
follow earlier delivered data.

Callbacks run after transaction commit, so a transaction retry does not replay
an external callback. Delivered progress advances only after a successful send.
Exact events use the captured codec, including numbers outside float64 range.
Raw logs remain bytes. Persisted log extents must be nonnegative and
representable; the valid maximum end is accepted. Query failures remain query
failures; deterministic BSON decoding and extent failures are corruption.

After receiving a live event or log frame, the source validates its original
observation before delivery. A check before waiting for the frame, or periodic
terminal polling alone, cannot protect historical rows lacking a birth
namespace. Same-birth terminal status stays valid for this check. Terminal
polling separately triggers a final replay; errors in that replay propagate.

## Retained filesystem subscriptions

`FileSource` asks the optional `FilesystemStreamStore` capability for a
retained selection. The constructor's legacy root argument is not payload
authority. The store captures directory handles, original header/presence and
codec; notifications use paths only as wake-up hints. Payload files are opened
relative to retained roots, checked as regular files, and the original
observation is validated before delivery. Root or parent replacement refuses
rather than adopting a successor. Metadata checks inspect the deletion marker
before reading the retained header; this does not claim a marker check after
every raw-file read. Same-birth leaf rotation or truncation can restart log
bytes at offset zero; the file handle and the retained directory are distinct.

An unbound event subscription can wait for its first directory under a retained
configured ancestor. It has no original run observation until one exists.
Unbound logs whose directory is initially absent complete empty. An already
bound missing origin refuses instead. Once initial resolution is published, a present directory with an absent
header retains that absence and cannot adopt a later header. Headerless opaque logs
can retain explicit format-two intent; structured events still require their
own format checks. Existing configured aliases are resolved at capture; the
metadata lock is addressed through the original canonical root, never a later
alias target. The context-aware wait uses the same metadata lock namespace as
writers, without a detached goroutine that could acquire a lock after cancel.
On Unix the reader opens only an existing lock. If none exists, its initial
header is provisional until a final lock probe: an appearing writer is awaited
and the header is captured again in the same retained run directory. A resolved
selection instead compares its immutable original. This preserves read-only
observation without creating lock files or treating a writer's intermediate
missing header as a completed absence. Windows observes its existing PID
namespace without claiming ownership: a live writer or an empty PID record
waits, a verified dead PID can be quiescent, and malformed records refuse.
Readers neither create nor remove PID entries; their read handles permit writer
deletion. Since Windows removes its entry at release, initial metadata is
recaptured after the final quiescent probe in a run root retained before that
probe. A directory not yet retained returns to initial waiting/empty-log
behavior. This is not an atomic PID transaction or a new execution fence.

Watch installation precedes the initial finite file drain. Each drain captures
its current end, keeps a partial event line for the next drain, and uses the
captured codec. The event cursor is internal inclusive/after state; the public
subscription still accepts an inclusive sequence. Successful sends alone
advance delivered progress. Notifications can be lost, so polling remains a
fallback; they do not select a new origin. Terminal polling triggers a final
drain held at an end-of-events barrier ([run-ws-origin.md](run-ws-origin.md)),
and deterministic terminal JSON decoding failures are permanent.

Filesystem selections own their roots; the producer owns and closes returned
file descriptors. The Source borrows the store. Closing a subscription or
Source cancels and joins pending initial resolution, the producer and its
poller before releasing those resources; a `FileSource` close ends its open
subscriptions with `ErrSourceClosed`, a `MongoSource` close ends them clean
and untyped (`terminated`, `log_terminated` on the socket).
Protocol-one support remains platform-
dependent and typed-refused where its storage capability is unavailable.
Historical regular-file opening has its own portable implementation, not a
strict-protocol admission dependency.

Windows qualification runs actual packages under Wine. Successful regular-file,
codec, rotation and lifecycle checks do not establish every native filesystem
behavior: reparse links and a verified dead PID could not be observed, and the
process handle counter returned no usable measurement. These are named
unqualified residues, not product defects or successful no-leak proofs. Public
format-one and format-two creation returned a directory-sync access-denied
error after publishing a real header; the PID-ABA test requires that exact
failure and reads the published header. It does not claim successful writer
acknowledgement or complete Windows parity. OS refusal to rename an open
ancestor is also distinct from a successful replacement experiment.

## Errors, cancellation and ownership

Identity, format, missing-original and corruption refusals stop the source.
Transient infrastructure errors (on FS also a permission or descriptor failure,
never a replacement) retry with the same selection and delivered position. A joined cancellation cannot hide a permanent refusal. Pollers report
their cause to the producer and are joined on every exit, including failed
watch/backfill and a blocked send cancelled by the poller. Normal cleanup
cancellation does not erase the error that caused cleanup.

Resolved Mongo and filesystem producers publish a fatal error even when
transient warnings filled the error buffer, then close the channels. Only the
producer closes them. This ordering does not guarantee a downstream `select`
reads the error before a closed data channel: consumers must preserve the
refusal. The WS pumps retain the first permanent cause and, on data closure,
drain at most the number of errors already queued. They emit `events_corrupted`
for event corruption, `event_stream_failed` for other permanent event errors
and `log_stream_failed` for permanent log errors, instead of normal
termination. Warnings alone preserve clean completion. This cannot recover an
error a source dropped or published after closing its data channel. `Warn` is
best effort; these resolved producers and the Service's own source use the
guaranteed producer-only `Fail`. The pump's
decision concerns the server's outbound queue: a broken socket or full queue
can still prevent wire delivery. Studio logs these error envelopes; a
`scope_lost` close makes the page refuse every request and socket for that
run until a reload ([run-ws-origin.md](run-ws-origin.md)).

Subscription `Close` cancels and joins its producer and poller. Source `Close`
also cancels and joins pending resolution and registered subscriptions.
Repeated calls are safe. Subscribe racing with shutdown cannot escape
registration. The Mongo client is borrowed: closing the Source does not
disconnect it or cancel unrelated store users.

## WebSocket subscription lifetime

Each event or log reservation owns admission, snapshot work and its forwarding
pump. Unsubscribe cancels that reservation and waits for the subscription to
close and the pump to finish before queuing its acknowledgement. Frames already
queued remain ahead of the acknowledgement; the old pump cannot enqueue another
frame after it. A naturally completed reservation stays registered until
unsubscribe, preserving duplicate-subscribe behavior.

Connection close signals cancellation and closes the socket. The connection's
run lifecycle performs blocking cleanup outside the connection mutex, closes
only a connection-owned cross-store Source, and joins its subscription workers.
The shared Source remains open. A subscription returned after cancellation is
closed without being installed. These guarantees rely on Source operations
honoring cancellation; they do not forcibly terminate arbitrary
implementations.

Every reservation's parent context binds the run the upgrade loaded, so the
store reads of each subscribe and resubscribe of a socket admit only that
run, and on the native filesystem store its live events, its logs and a
registered run's alert lane; unregistered runs' alerts stay keyed by ID
([run-alert-origin.md](run-alert-origin.md)). A redial carries the expected
birth, compared before upgrade.

## Event cursor boundaries

[run-cursors.md](run-cursors.md) describes inclusive/exclusive anchors, replay progress, and legacy compatibility at the int64 boundaries.

## What this does not establish

A subscription retains its first observation; a WS socket binds the run of an
earlier HTTP snapshot only through the birth its dial carries, compared by the
gate before the upgrade. Each subscription of a socket is bound to its
upgrade's observation — header, format and recorded tenant — within the limits
of [run-ws-origin.md](run-ws-origin.md). A rendered state for fatal stream
errors other than a lost origin remains an activation obligation. Neither this
adapter nor the storage migration enables protocol-one admission.

Retained event-origin capture and delivery checks are described in
[run-event-origins.md](run-event-origins.md), including the observed FS blank-ID
normalization and the limits of absent-parent tenant observations.
