# ADR-053 — Unified, store-agnostic run streaming (events + logs, local + cloud)

Status: **accepted** (2026-07-03; amended same day — see Amendment). The
immediate asymmetry that motivated it is already fixed (`EnsureLogSource`,
commit `d4f12e39d` — see Context); this ADR records the target architecture
and the migration so the next streaming change lands on one seam instead of
the current four.

## Context

The studio's run console streams two byte-streams to the browser over the
run WebSocket: the structured **event** timeline (`events.jsonl`) and the raw
**log** (`run.log`). Today there are **four different wirings** for getting
those bytes to a subscriber, chosen by *how the run was produced* rather than
by a single contract:

1. **In-process launch** (studio/CLI `Launch`) — the runtime observer feeds
   the broker directly and the per-run logger tees into a `RunLogBuffer`
   ([pkg/runview/service_launch.go](../../pkg/runview/service_launch.go),
   [service_log.go](../../pkg/runview/service_log.go)).
2. **Detached subprocess** (managed runner) — the subprocess owns the files;
   the parent tails them with fsnotify (`startEventSource` + `startLogSource`,
   [file_event_source.go](../../pkg/runview/file_event_source.go),
   [file_log_source.go](../../pkg/runview/file_log_source.go)).
3. **External / dispatcher run, not produced in this process** — events got an
   **on-demand** fsnotify tailer via `EnsureEventSource`
   ([service_eventsource.go](../../pkg/runview/service_eventsource.go)); logs
   had **no equivalent**, so `handleSubscribeLogs` treated a nil live buffer as
   "terminated" and did a one-shot replay of `run.log`. Result: an *active*
   external run's logs never streamed — the studio needed a full page refresh
   to see new lines. **Fixed** by `EnsureLogSource`, the run.log twin of
   `EnsureEventSource` (commit `d4f12e39d`): on subscribe, if the run is active
   and not in-process, stand up a refcounted in-memory buffer + fsnotify tailer.
4. **Cloud** — runs execute on runner pods pulling from the NATS queue and
   writing to the **Mongo** store; the server pod shares no filesystem, so
   fsnotify (which all of 1–3 ultimately rely on) does not apply. Events
   already stream via a Mongo **change-stream** source
   ([pkg/runview/eventstream](../../pkg/runview/eventstream/iface.go),
   wired in `cmd/iterion/server.go`); **logs have no cloud path at all** —
   the runner logs to pod stderr and the Mongo store persists no run.log.

A fifth wiring hides in the WS layer itself: **cross-store** (`?store=`,
observing a run owned by another local daemon) re-implements the fsnotify
tailers a third time inside `pkg/server`
(`tailCrossStoreEvents` / `streamLogsCrossStore`).

Structural problems: (a) fsnotify is filesystem-only, so cloud can never reuse
the local tailers; (b) events and logs drifted apart (events had the on-demand
path, logs didn't — the #3 bug; events have a cloud path, logs don't), because
there is no single "subscribe to this run's stream" seam that both go through.

## Decision

Introduce **one store-agnostic streaming source** behind a small per-store
interface, and route **both** events and logs, in **all** production modes
(including cross-store), through it.

```go
// pkg/runview/runstream
type Source interface {
    // Persisted replay + live tail, gap-free, at-least-once, batched.
    SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (EventSubscription, error)
    // Offset-tagged log bytes from fromOffset; Chunks() closing = stream over.
    SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (LogSubscription, error)
    Capabilities() Capabilities // {LiveTail, HistoricalRange, Logs}
    Close() error
}
```

Key contract points:

- **Replay lives inside the subscription.** "Deliver everything ≥
  fromSeq/fromOffset, no gap, then tail live" is the source's job, so the WS
  handler does `snapshot → Subscribe(effectiveFrom) → pump` identically in
  every mode. Dedup guards (event seq high-water mark + the unpersisted
  `EventAlert` Seq=0 bypass; log offset cutoff slicing) move *inside* the
  filesystem subscription — the same mitigations as before, relocated to the
  only place double delivery can occur.
- **Backends, selected by the store the run lives in:**
  - **Filesystem store** (local) → the Service's broker/`RunLogBuffer`
    machinery becomes the *internal fan-out* of this backend; the existing
    fsnotify tailers (with polling fallback) are its feeders. The refcounted
    `ensureEventSource`/`ensureLogSource` attach points become backend
    internals whose release is folded into `Subscription.Close()`.
  - **Cross-store** (foreign local root) → the same filesystem tailers,
    parametrized by store root (`runstream.FileSource`), with a run.json
    terminal poll (a foreign run has no in-process completion signal). The
    third tailer copy in `pkg/server` is deleted.
  - **Mongo store** (cloud) → **change streams**, symmetric for both flows:
    the existing events change-stream source moves under this interface, and
    logs gain a twin — a new append-only **`run_logs`** chunk collection
    (`{tenant_id, run_id, offset, data, ts}`, unique `(run_id, offset)`, TTL
    shared with events) written by the runner through a batching tee on its
    per-run logger, tailed by the server pod via a change stream on inserts.
- In-process `Launch` keeps feeding the broker/buffer directly (fast path);
  detached runs keep their eager tailers (run-health alerts consume the event
  flow even with zero WS subscribers). A subscriber never branches on "did
  this process launch it?".
- **Tenancy (cloud):** subscriptions are per-subscriber and scope replay +
  change-stream by the caller's tenant ctx (`store.TenantFromContext`); authz
  happens before subscribe (tenant-filtered run load at WS upgrade).
  Refcount-sharing exists only in the filesystem backend, which is
  single-tenant by construction.
- **Log persistence API:** a new *optional* store interface (same pattern as
  `PIDStore`/`SpendStore`) — `RunLogStore{AppendRunLog, ReadRunLogRange,
  RunLogSize}` — implemented by the filesystem store over `run.log` (becoming
  the one read path for the scattered direct-file readers) and by the Mongo
  store over `run_logs`. The runner seeds its offset counter from
  `RunLogSize` at claim time (resume/redelivery-safe; the unique index is the
  race safety net). Runner-side write failures degrade **loudly** (bounded
  retry, then drop the batch with an ERROR log + counter) but never block or
  kill the run: the log stream is a derived observability view, not run
  correctness — the degradation is explicit, never silent.
- `Event.LogOffset` (the event↔log-position correlation behind the per-node
  Logs tab) gets stamped in cloud too: the Mongo store gains the same
  `SetLogPositionFn` hook as the filesystem store, fed by the runner's log
  writer total.

The immediate `EnsureLogSource` fix was the first step of this convergence
(logs symmetric with events on the filesystem backend). The remaining work:
(a) lift the tailers + the events change-stream source behind
`runstream.Source`, (b) add `run_logs` persistence + the Mongo log source,
(c) delete the mode-specific branches in `handleSubscribe` /
`handleSubscribeLogs` and the duplicated cross-store tailers.

## Amendment (2026-07-03) — change streams, not the eventbus

The original proposal routed the cloud backend over `pkg/eventbus`
(`NATSBus` on the `ITERION_EVENTS` stream). Implementation review reversed
that choice in favour of Mongo change streams:

- **`NATSBus` does not exist.** Only `InProcBus` is implemented; the
  `ITERION_EVENTS` stream constants are declared but never provisioned or
  used. The change-stream tailer, listed below as the alternative, already
  exists and works for cloud events.
- **Wrong delivery semantics.** The eventbus is a deliberately *lossy*
  notification spine carrying `trigger.Event` (ADR-046). Run streaming needs
  lossless resume — which means the store must persist log chunks anyway for
  backfill, at which point the bus would be a second delivery path to keep
  consistent with the first.
- **Store-derived streaming keeps one source of truth.** The persisted
  `(run_id, seq)` / `(run_id, offset)` anchors are exactly what the wire
  protocol already exposes (`from_seq`/`from_offset`); change streams give
  lossless live delivery from the same data the replay reads, with no new
  infrastructure (a replica set is already required).

## Alternatives considered

- **Keep the four wirings, fix bugs case-by-case.** Rejected — the #3 log bug
  was invisible for exactly this reason; the next mode (or the next stream)
  will grow its own gap.
- **Ride `pkg/eventbus` (NATSBus on `ITERION_EVENTS`) for cloud** — the
  original decision. Rejected on implementation review; see Amendment.
- **Per-run `RunStreamSource{Events(),Logs(),Close()}` handles** (the
  original interface sketch). Superseded by the per-store `Source` with
  per-stream subscribe methods: it matches the existing events source shape,
  avoids a handle object per (run, subscriber), and keeps refcounting a
  filesystem-backend internal instead of part of the contract.
- **Poll the store everywhere** (drop fsnotify). Simpler but strictly worse
  latency locally, and wasteful at scale — the local fsnotify fast path is
  worth keeping behind the interface.

## Consequences

- One seam to reason about; a new stream (e.g. a metrics channel) or a new
  store backend is one implementation, not four.
- Cloud gets true live streaming (events + logs) without a filesystem, closing
  the local/cloud parity gap — including per-node log slicing via
  `Event.LogOffset`.
- ~550 lines of duplicated cross-store tailers in `pkg/server` are deleted;
  cross-store `unsubscribe_logs` (previously not honoured) works for free.
- Migration is incremental and non-breaking: `EnsureLogSource` already
  shipped; the interface, the `run_logs` persistence and the Mongo log source
  land behind it without changing the WS wire protocol (`log_chunk` / event
  envelopes are unchanged, pinned by the contract tests in
  `pkg/server/runs_ws_logs_test.go`).
- Risk: double-delivery if a mode both tees directly and attaches a source —
  mitigated by the seq/offset dedup that moves inside the filesystem
  subscriptions (the same guard that made `EnsureEventSource` safe to
  over-subscribe).
