# ADR-051 — In-bot event-driven primitives (`emit` / `wait`)

Status: **proposed → Brick 1 implemented** (2026-06-30). The intra-run
`emit`/`wait` pair backed by a run-local reliable event registry, with a
mandatory `wait` timeout, ships first. External-event resume of a parked run
(via the ADR-046 trigger spine) and a cross-process `paused_waiting_event`
status are documented follow-ons.

## Context

iterion is already heavily event-driven **at the platform layer** — the ADR-046
trigger spine (`pkg/eventbus` + `pkg/trigger`) routes `(event filter) → (bot
launch)`, supervisors ([pkg/supervise](../../pkg/supervise/supervise.go)) are
reactive `addEventListener`s on a running agent, and a `human` node is already an
`await externalEvent` (it parks the run and resumes when the answer arrives).

What is missing is event-driven control flow **inside a single `.bot`**: a node
that can `emit` an event and a node in a parallel branch that can `wait` for it.
This is the in-bot analog of `EventEmitter.emit`/`on` and `await`. It turns the
`.bot` graph from pure dataflow into a reactive coordination graph.

The natural temptation is "make it like JS" (an event loop + microtask queue +
shared mutable heap + callbacks closing over mutable scope). That model fights
two guarantees iterion deliberately keeps (see ADR-050 /
[docs/dsl-totality-and-tc.md](../dsl-totality-and-tc.md)): **write-once
immutability** (Layer C mutable state is a non-goal — it breaks resume /
reproducibility) and **resource-bounded termination**. The right model is
**actor / CSP / reactive-streams**, not the JS event loop:

| JS | iterion idiom |
|----|---------------|
| `emitter.on("x", cb)` | a `wait` node / subscription edge — not a closure |
| mutable payload shared between handlers | **immutable** event payload → handler produces a new artifact |
| `await fetch()` | a `wait` node — parks until the event, bounded by a timeout |
| global mutable state across handlers | absent by design (Layer C non-goal) |

## Decision

Add two node kinds to the DSL, backed by a **run-scoped reliable event
registry** — deliberately NOT the lossy cross-run `pkg/eventbus` (which is
at-least-once and drops under back-pressure; correct for cross-run notification,
wrong for in-run coordination).

### `emit` node

```
emit ping:
  event: "ready"
  with: { value: "{{outputs.producer.n}}" }
```

Publishes a **sticky** event named `ready` into the run registry with an
immutable payload (resolved from the `with:` mapping using the existing
`DataMapping`/`ParseRefs` machinery). Sticky = a `wait` that arrives *after* the
emit still sees it (the registry records fired events, so emit/wait are not
order-fragile). Non-mutating, no LLM, no shell.

### `wait` node

```
wait for_ready:
  event: "ready"
  timeout: "30s"          ## MANDATORY — the bornage
  output: ready_schema    ## optional: types the received payload
```

Blocks its branch until `ready` is emitted, then completes with the event's
payload as its output (`{{outputs.for_ready.value}}`). The **`timeout:` is
mandatory** — the "no silent infinity" invariant, exactly mirroring C097 on
`unbounded` loops. On timeout the node fails resumably (a future brick may route
to a `timeout` edge). Non-mutating.

### Run-scoped registry (the substrate)

A mutex-guarded structure on `runState` ([pkg/runtime/engine.go](../../pkg/runtime/engine.go)):

```go
type runEvents struct {
    mu      sync.Mutex
    fired   map[string]map[string]interface{} // event name → immutable payload
    waiters map[string]chan struct{}          // event name → close-on-fire signal
}
```

- `emit` records the payload under the event name and closes the waiter channel
  (creating a pre-closed one if none exists) → sticky + broadcast.
- `wait` takes the (possibly already-closed) channel and `select`s on it vs the
  mandatory timeout vs `ctx.Done()`. A closed channel reads immediately, so a
  late waiter never blocks.

This reuses the existing fan-out goroutine-per-branch model: branches already
share `runState` by pointer under locks, so the registry rides the same
discipline. `emit`/`wait` are non-mutating, so the single-mutating-branch
workspace-safety rule is unaffected.

**Scheduler constraint (documented, not re-architected):** a parked `wait` holds
its fan-out semaphore slot, so the emitting branch needs a concurrent slot —
`max_parallel_branches` must be ≥ the number of branches that run concurrently
with a `wait`. When it is too small the emitter can't run and the `wait` hits
its (mandatory) timeout: bounded, never a hang. Releasing the slot on park is a
future scheduler refinement.

### Diagnostics (C196–C198 — the next free block after C195)

- **C196** (error) — an `emit`/`wait` node with no `event:` name.
- **C197** (error) — a `wait` node with no `timeout:` (the mandatory-bound
  invariant; the C097 analog).
- **C198** (warning) — a `wait` on an event no `emit` in the workflow produces
  (it can then only ever time out) — the C098 analog (a dangling/unreachable
  event). Symmetrically warns on an `emit` no `wait` consumes (dead event).

### What stays out (by design / staged)

- **No mutable shared state.** Payloads are immutable; a handler produces a new
  output/artifact. (Layer C remains a non-goal.)
- **No unbounded wait.** `timeout:` is mandatory; the run stays budgeted and
  resumable.
- **External events / cross-process resume (follow-on).** A `wait` on an event
  sourced *outside* the run (a webhook, a sibling run finishing, `iterion emit`)
  parks the run as a new `paused_waiting_event` status (mirroring
  `paused_waiting_human`) and resumes via the ADR-046 spine with a new
  *resume-parked-run* effect alongside the existing *launch* effect. Deferred to
  keep Brick 1 self-contained and deterministically testable in-process.
- **Releasing the semaphore slot on park** (scheduler refinement) — deferred.

## Consequences

- The `.bot` graph gains genuine reactive coordination (CSP channels between
  parallel branches) without a new transport — it rides the existing fan-out +
  `runState` + `DataMapping` seams.
- Every guarantee from ADR-050 is preserved: immutability (payloads are
  copied-in), bounded termination (mandatory timeout + budget/fuel), resumability
  (the registry is reconstructible; Brick-1 intra-run waits are in-process).
- The model is consciously actor/reactive, not JS — which keeps it composable
  with everything the engine already promises.

## References

- Substrate: [pkg/runtime/engine.go](../../pkg/runtime/engine.go) (fan-out +
  `runState` + node dispatch), [pkg/eventbus](../../pkg/eventbus/bus.go) (the
  *cross-run* lossy bus this deliberately does NOT reuse for in-run coordination),
  [pkg/trigger](../../pkg/trigger/event.go) (the platform spine for the external
  follow-on).
- Demonstrator: `examples/events/pingpong.bot` + `e2e/events_emit_wait_test.go`.
- Doctrine: [docs/dsl-totality-and-tc.md](../dsl-totality-and-tc.md), ADR-050,
  ADR-046 (trigger spine).
