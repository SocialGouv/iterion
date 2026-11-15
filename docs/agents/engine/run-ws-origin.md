# Run origin

Companion to [run-streams.md](run-streams.md) and [run-event-origins.md](run-event-origins.md).

## Expected birth

The snapshot header and the run rows (list, children, global-active) carry
`birth_id`, the run's store-owned birth (`""` for a historical run). A client
sends it back as the birth it expects: the `X-Iterion-Expected-Birth` header
on a fetch, `expected_birth` on a URL the browser loads by itself and on every
socket dial or redial, `<x>_run_birth` beside a run ID carried in a body
(launch `source_run_birth`, assistant watch and mission `assistant_run_birth`,
handoff `source_run_birth`/`destination_run_birth`), `run/<id>@<birth>` in an
assistant context reference. `historical` expects a run with no birth.

`runOriginGate`, inside the auth middleware — after `scopeRunByID`'s 404 and
ladder, and on the DisableAuth chain — compares it on every run-wildcard
route, before any handler: a mismatch, a deletion or an absence answers 409
`run_origin_lost` naming `run_id`; a socket is accepted then closed
`scope_lost` (the run socket first sends `run_origin_lost`; the shell and the
CDP pump get the close alone); a malformed or conflicting value is 400
`invalid_expected_birth`, a socket closed with that token (not redialed);
another read error 503 `scope_unavailable`. A body field is compared right
after the handler's own load of that run (a mission reattach loads it first);
a context reference resolves to reason `run_origin_lost` for another birth or
a deletion, `not_found` when not found — a tenant-scoped store's answer for a
run of another team. Without an expectation a request is unchecked and costs
no read — the CLI, MCP, bots and older studios address runs by ID. The read
follows the Mongo URI's read preference; the store warns at open when it is
not primary. `ITERION_RUN_BIRTH_CHECK=0` (operator-grade) admits every HTTP
and body expectation of the server's own store unread; it does not lift the
foreign-store refusal below, a socket's binding (a dropped birth still
closes it `scope_lost`) nor the studio's registry.

The guarantee covers the server's own store — the one its run console
resolved, from the work dir when no store dir is configured. A `?store=`
naming it (symlinks followed) is removed before scoping, the gate and the
handlers (`normalizeOwnStore`): the desktop's links name a run's store even
when the daemon serves it. A foreign `?store=` (a cross-store view) with an
expectation is refused 400 `store_unsupported` (a socket closed with that
token); without one, every route answers as it would without the gate.

GET of the run, its events and its log read under the admitted origin
(`store.BindRunObservation`) and answer 409 for a successor. A live log
buffer records the birth of the run it was created for — an on-demand
tailer's, the run it tails, read without the subscriber's binding — and a
tailer writes only to its own buffer. A bound request or log subscription
reads the buffer only for that birth, else the persisted log: while a
predecessor's buffer stays registered under the ID (a subscriber still holds
it), a successor-bound reader gets persisted bytes without live tailing until
that buffer is released. Only a request carrying an expected birth
skips the out-of-band merge reconciliation (a snapshot, an assistant watch's
creation; Mongo's conditional merge write has no birth predicate); a
socket's snapshot reconciles.

## Capture at the upgrade

The run socket binds the origin the gate admitted. Without an expectation the
upgrade loads the run once — `Service.CaptureRunOrigin`, or
`runview.CaptureRunOriginIn` on the foreign store of a `?store=` socket — and
keeps its `store.RunObservation`: the header (ID, format, protocol, birth)
and the run's stored tenant, kept apart from the requester's identity. A
primary-store load failure answers 404. A cross-store run absent at an
unchecked upgrade stays unbound, so a run created later is admitted; a
deleted one answers 410, any other read error 500.

## Bound reads

`store.BindRunObservation` carries the header and format as compare-only
read intent: a binding or format the context already carries is compared,
never replaced. Tenant and presence do not travel. The socket's `streamCtx`
(its unbound `authCtx` plus that binding) is the parent of every event and
log reservation, so the snapshot, replay, terminal catch-up, log admission
and persisted log reads of every subscribe and resubscribe read the
upgrade's run. Native filesystem and Mongo stores refuse a successor
recreated under the ID (`ErrRunDataIdentityChanged`), a deletion
(`ErrRunDeleted`) or an absence (`ErrRunNotFound`). Mutating actions keep the
unbound `authCtx`: a resume or a child run never inherits the binding.

## Source capability

A bound socket calls `runstream.RequireBoundObservationSource` before any
snapshot or subscription. The capability endorses the actual receiver, never
through embedding: `FileSource`, the resolved `MongoSource` and the Service's
own source implement it; the raw-collection `NewMongo` does not, and a
wrapper forwards it explicitly. A source lacking it is refused with
`ErrRunDataCapability` as `event_stream_failed` or `log_stream_failed`; the
socket stays open.

## The Service source

For every caller, a permanent refusal of the replay, of the catch-up's run
load or final replay, or of a persisted log read ends the subscription
through the producer-only guaranteed `Fail`. `Fail`, `Warn` and `Finish`
are serialized; a report after `Finish` is dropped. Run metadata that does
not decode is permanent: both stores wrap it as
`store.ErrRunMetadataUndecodable`, while an empty `run.json` (a bare
`io.EOF`, met mid-write) stays transient. A bound log subscription loads its
run before looking up a buffer or tailer and returns a permanent refusal
instead of a subscription; after a transient load error its producer retries
the admission and reads nothing before it. Other transient errors are
warnings; a transient final replay keeps tailing and replays again from its
cursor at the next poll.

## Losing the origin

When a snapshot, event or log subscription of a bound socket fails because
its origin was replaced, deleted or is missing, the socket queues the typed
error (`snapshot_failed`, `event_stream_failed`, `log_stream_failed`), then
the `scope_lost` close. Each mutating envelope first loads the run under the
bound context: a lost origin answers `run_origin_lost` and closes the same
way; a read that does not complete answers `scope_unavailable` for that
envelope only. A standing tick that closes first leaves a pending
subscription's error undelivered.

The studio keeps a page-scoped registry (`studio/src/lib/lostRuns.ts`): lost
runs per store (nested by the store the request or socket dial named, then
run ID) and the births of the server's own runs. The page's `?store=` addresses the
page's run alone, on the routes that read a foreign store (snapshot, events,
workflow, session board, run socket); for that run the page sends no
expectation and adopts no birth, and an invalid `?store=` fails the view. A
global-active row names a store only when the destination daemon does not
own it (`own_store`). A row's birth is adopted when nothing is known and
replaces the row of an earlier-sent request, never a snapshot's, and never
marks a run lost. A clicked row's birth travels with the navigation (history
state in-page, `expected_birth` on a full page load such as the desktop's
cross-daemon link); the destination pins it before its first request and
strips it, and no row replaces the pin before the view's first snapshot
adopts or refuses it. A snapshot of another birth, a 409
`run_origin_lost` or a `scope_lost` close marks the run lost until a reload.
Requests and sockets addressing a lost run are refused before they leave, the
run socket closes, and the run view, the shell and the CDP pump show it lost.
The server refusal is the guarantee; the registry is defense in depth.

## What this does not establish

- Unchecked requests: CLI, MCP, bots, older studios, a view's requests
  before it learns the birth (the children poll waits), run IDs the page
  never saw (chat-proposed, pipeline cards). Links other than the run lists
  (command palette, node chips, board callbacks) open by ID alone: the first
  snapshot adopts whichever run holds the ID.
- Cross-store views (a foreign `?store=`): no expectation is checked; only
  the run socket binds the run its upgrade read there. A panel whose route
  reads no foreign store (log, artifacts, files, shell, actions, a child's
  or parent's run and workflow) reads the server's own run of the same ID —
  a class older than the guarantee.
- Atomicity: routes other than the three bound reads, and every mutation,
  are admitted, not bound — the run can be replaced before the action; Mongo
  metadata writes carry no birth predicate.
- Isolation after admission: broker tails, tailers and alerts stay keyed by
  run ID, so an open subscription can receive a successor's in-flight frames
  until a bound read refuses it.
- Downloads outside the server (a presigned attachment) and a preview's
  subresources; replication lag under a non-primary read preference.
- Tenant changes under one birth, or two historical runs.
- Writers older than birth identities: a v6.0.x writer drops `birth_id`, so
  a newer server reads that run as replaced (a reload recovers). Roll writers
  no later than servers.
- Authorization: the origin is provenance; standing ticks and the mutation
  gate remain the authority.

Protocol-one admission stays off.
