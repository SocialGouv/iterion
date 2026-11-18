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

A bound log stream ends on exactly three sources, and no other:

1. **The producer's own end**, delivered on the lane: terminal (after its
   finalize wrote the last lines), paused (the stream parks; the resume's epoch
   replaces it), or lost (a typed error, never a clean completion).
2. **A retained closed epoch** the subscriber observes — the same value, read
   from the registry rather than from the event. The first end of an epoch is
   **sticky**: E1 and E2 can never apply twice.
3. **No lane and no local producer**: a terminal status plus the settle window
   (500 ms) ends the stream after one last drain. This is the foreign-writer
   promise — a run produced elsewhere on the same host finishes here.

A status poll is never a fourth source: on a live lane it only warns once, when
the producer stalls past its grace. The engine persists the terminal status
*before* its finalize writes the last log lines, so a stream that ended on the
status would truncate them.

## What this does not establish

- A lane is not transferred between `Service` instances: after a hot-swap the
  new Service sees no lane and applies the foreign-writer settle, so a writer
  still finishing more than the settle window after the status loses its tail.
- Cross-store WebSocket connections keep the legacy end model (status-driven),
  and so does any request without a binding.
- A producer whose capture failed after a subscriber was admitted ends that
  subscriber's stream with the typed cause; a producer that never renders its
  own end is ended by `Stop`/`Drain`, never by a silent completion.
- Cloud log storage stays outside this path (protocol 1 is off).
