# Run log origins — the bound log lane

A WebSocket log subscription is bound to the run origin its upgrade authorized
([run-ws-origin.md](run-ws-origin.md)). Before R54 the bytes came from a buffer
keyed by run ID alone: the in-process tee, or a tailer stood up on demand for a
run this process did not launch. After a run was replaced, that tailer kept
following `runs/<id>/run.log` and the subscriber bound to the predecessor
received the successor's bytes. The bound path now reads its bytes from a
selection retained for the admitted run, and takes its wakes from a lane that
carries no bytes at all.

## The lane

`logRegistry` (`pkg/runview/log_registry.go`) keeps one topic per producer
registration, keyed by the run observation, with an epoch per adoption and a
bail per holder — the shape of the alert registry, without a payload.

- A producer **wakes** the lane after the bytes reach the store (file tee) or
  after reading bytes already on disk (detached tailer). A wake is a cap-1
  signal: a dropped wake carries no truth, since the reader re-drains to EOF.
- A **closed epoch is retained** until a subscriber observes its end, or a newer
  epoch of the same observation replaces it. A subscriber parked across a close
  therefore never misses it.
- `runLogOwner` carries a producer's bail from registration to the goroutine
  that takes the run over: `Transfer()` runs synchronously immediately before
  each `go` and is idempotent, `Abandon()` is a no-op once transferred. A
  launcher's late `defer` can never close a live producer's lane. Every abandon
  is classified by the run's status: paused parks, terminal ends, anything else
  is lost.
- Tee, detached tailer and selection all derive from the store's canonical root
  (`store.RunStore.Root()`), so the bytes written are the bytes the selection
  reads. A store without a root (Mongo) or a memory-only producer is refused at
  adoption, typed.

## The end of a stream

A bound log stream ends on exactly five sources, and no other:

1. **The producer's own end**, delivered on the lane: terminal (after its
   finalize wrote the last lines), paused (the stream parks; the resume's epoch
   replaces it), or lost (the producer's failure — a capture or a tee that
   failed — as the stream's error, always permanent).
2. **A retained closed epoch** the subscriber observes — the same value, read
   from the registry rather than from the event. The first end of an epoch is
   **sticky**: E1 and E2 can never apply twice.
3. **No lane and no local producer**: a terminal status plus the settle window
   (500 ms) ends the stream after one last drain. This is the foreign-writer
   promise — a run produced elsewhere on the same host finishes here. It
   never concludes over a producer failure recorded meanwhile: that ends it (1).
4. **The origin's loss**: a permanent refusal of the admission's load, of
   the retained selection (a successor, a deletion), of a lane's (re)adoption
   (a memory-only producer) or of the status read with no lane ends the
   stream with that refusal, always typed.
5. **The Service's stop**: on the native filesystem store, `StopBackground`
   ends every bound log stream — open, being set up or still waiting for its
   admission — with `runstream.ErrSourceClosed`, never a clean completion.
   `Drain` lets its runs end first, so their log streams may end on their
   producer's own end; its stop then ends what is still open the same way. A
   producer that never renders its own end leaves its stream to this end,
   never to a silent completion.

A status poll never ends a live lane: there it only warns once, when the
producer stalls past its grace; with no lane it arms the settle (3) or reports
a permanent refusal (4). The engine persists the terminal status *before* its
finalize writes the last log lines, so a stream that ended on the status would
truncate them.

## What this does not establish

- A lane is not transferred between `Service` instances. A hot-swap ends the
  old Service's bound log streams typed (its stop, above), and a resubscribe
  on the same socket is refused the same way: the client redials, and the run
  is served again only if the new Service's store holds it (the studio leaves
  the run view on a project switch). The new Service sees no lane and applies
  the foreign-writer settle, so a writer still finishing more than the settle
  window after the status loses its tail.
- A local capture failure can leave the producer writing `run.log` even though
  a new bound log subscription receives no bytes and fails until a new local
  registration replaces the failure. A failure retained without an observation
  can also refuse a later run under the same ID. The socket stays open if its
  own bound read succeeds; only that read can establish origin loss.
- Cross-store WebSocket connections keep the legacy end model (status-driven),
  and so does any request without a binding.
- Cloud logs never take this path: the cloud socket reads the change-stream
  source the server injects, and a store without a root is refused at
  adoption.
