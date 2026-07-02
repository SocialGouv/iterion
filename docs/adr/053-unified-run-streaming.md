# ADR-053 — Unified, store-agnostic run streaming (events + logs, local + cloud)

Status: **proposed** (2026-07-03). The immediate asymmetry that motivated
it is already fixed (`EnsureLogSource`, commit `d4f12e39d` — see Context);
this ADR records the target architecture and the migration so the next
streaming change lands on one seam instead of the current four.

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
   fsnotify (which all of 1–3 ultimately rely on) does not apply. Cloud live
   delivery leans on store polling / a separate path.

Two structural problems: (a) fsnotify is filesystem-only, so cloud can never
reuse the local tailers; (b) events and logs drifted apart (events had the
on-demand path, logs didn't — the #3 bug), because there is no single "attach
a live source for this run" seam that both go through.

## Decision

Introduce **one store-agnostic streaming source** behind a small interface,
and route **both** events and logs, in **all four** production modes, through a
single `EnsureSource(runID)` on-demand attach point.

- **`RunStreamSource`** — per (store, runID): `Events() <-chan Event` and
  `Logs() <-chan []byte` (offset-tagged), plus `Close()`. The WS handlers hold
  a refcounted handle and never care how bytes arrive.
- **Backends**, selected by the store the run lives in:
  - **Filesystem store** (local) → the existing fsnotify tailers
    (`file_event_source` / `file_log_source`), with the polling fallback. This
    is exactly what 1–3 already do; they collapse into this one backend.
  - **Mongo store** (cloud) → ride the internal **`pkg/eventbus`** that the
    trigger spine already runs (InProc local, **NATSBus** on the `ITERION_EVENTS`
    stream for cloud — see ADR-046): the runner publishes each event + log chunk
    to the bus keyed by run id; the server pod subscribes. No fsnotify, no
    change-stream, no polling. (Alternatively a Mongo change-stream tailer, but
    the eventbus already exists and unifies with the spine.)
- In-process `Launch` keeps feeding the broker/buffer directly (fast path); it
  simply also registers with the same `EnsureSource` refcount so a subscriber
  never has to branch on "did this process launch it?".

The immediate `EnsureLogSource` fix is the first step of this convergence: it
makes logs symmetric with events on the filesystem backend. The remaining work
is to (a) lift the events/logs tailers behind `RunStreamSource`, (b) add the
Mongo/eventbus backend, and (c) delete the mode-specific branches in
`handleSubscribe` / `handleSubscribeLogs`.

## Alternatives considered

- **Keep the four wirings, fix bugs case-by-case.** Rejected — the #3 log bug
  was invisible for exactly this reason; the next mode (or the next stream)
  will grow its own gap.
- **Mongo change streams for cloud** (instead of eventbus). Viable, but adds a
  second cloud delivery mechanism next to the eventbus the spine already
  operates; the bus also gives at-least-once + fan-out for free.
- **Poll the store everywhere** (drop fsnotify). Simpler but strictly worse
  latency locally, and wasteful at scale — the local fsnotify fast path is
  worth keeping behind the interface.

## Consequences

- One seam to reason about; a new stream (e.g. a metrics channel) or a new
  store backend is one implementation, not four.
- Cloud gets true live streaming (events + logs) without a filesystem, closing
  the local/cloud parity gap.
- Migration is incremental and non-breaking: `EnsureLogSource` already shipped;
  the interface + Mongo backend can land behind it without changing the WS wire
  protocol (`log_chunk` / event envelopes are unchanged).
- Risk: double-delivery if a mode both tees directly and attaches a source —
  mitigated by the seq/offset dedup the WS layer already performs (the same
  guard that makes `EnsureEventSource` safe to over-subscribe today).
