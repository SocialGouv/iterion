# ADR-084 — Subbot parents re-attach to their child across a restart

Date: 2026-07-22
Status: accepted

## Context

A `subbot` node runs a child `.bot` as a nested run and maps its terminal
output back to `outputs.<subbot>.<field>`. When the child hits a human gate
its own engine pauses (`paused_waiting_human`), and the parent's subbot node
**parks** in `AwaitSubbotTerminal` — an in-memory goroutine polling the child
run doc until it reaches a terminal state.

That park is only as durable as the process. When the studio/CLI process
restarts while a parent is parked:

1. the orphan sweep promotes the stale `running` parent to `failed_resumable`;
2. the CHILD stays `paused_waiting_human` — answerable on the pipeline board;
3. answering it completes the child, but **nothing picks the result up** (the
   parent goroutine is gone);
4. resuming the PARENT re-executed the subbot node, and both runners
   (`runview.subbotRunnerFor`, `cli.subbotRunnerForCLI`) called
   `store.GenerateRunID()` unconditionally — spawning a **fresh child from
   scratch**. The answered child's work was lost and its stale review lingered
   on the card.

The child already links UP to the parent (`Run.ParentRunID` +
`Run.ParentNodeID`), so `ListChildRuns(parent)` can enumerate a subbot node's
children. But that reverse link alone cannot make re-execution idempotent: in a
looped subbot, iteration 1's child is `finished`-and-consumed while iteration
2's is in-flight; a crash between the loop-back and the next spawn would let a
status heuristic re-attach to the already-consumed child and silently skip an
iteration.

## Decision

**Persist the in-flight child on the PARENT, keyed per node execution, and
make the runner re-use it.**

- **`Run.SubbotChildren map[string]string`** — a new field on the parent run
  doc mapping a subbot-node *execution key* to the child run id. Written when
  the child is launched (before its engine runs, so an interrupt while parked
  leaves a durable record) and cleared when the terminal output is consumed.
  Two atomic per-key mutators on both `RunStore` backends
  (`SetSubbotChild`/`ClearSubbotChild`: filesystem RMW-under-mutex, Mongo
  per-key `$set`/`$unset`) so concurrent fan-out branches writing distinct keys
  never clobber each other.

- **Execution key** = node id + loop-iteration path + fan-out branch id,
  computed by the engine (`Engine.subbotReattachKey`) and handed to the runner
  on `SubbotRequest.ReattachKey`. Loop-iteration path disambiguates loop
  iterations (no consumed-child re-use); branch id disambiguates concurrent
  fan-out branches (no fresh-run cross-branch collision). Both branch ids
  (`branch_<router>_<i>`) and iteration paths are deterministic, so the key is
  **stable across resume**. Sanitized to a Mongo-safe field name.

- **`ReattachSubbotChild`** — one shared oracle (in `runview`, called by both
  runners) that, at re-execution, reads the recorded child and decides
  reuse-vs-fresh from its real status: `finished` → its terminal output;
  `paused`/`running`/`queued` → re-enter `AwaitSubbotTerminal` (re-park);
  `failed`/`cancelled`/vanished → clear the record and spawn fresh. The record
  is cleared only on successful consumption — a shutdown mid-park (or a child
  that ended badly) LEAVES it, so a resumed parent re-attaches / re-decides.

## Alternatives considered

- **Reverse lookup via `ListChildRuns` + a status heuristic (no new field).**
  Rejected: cannot distinguish an already-consumed `finished` child from an
  in-flight one across loop iterations, and gives no safe way to pick the right
  child per fan-out branch (children carry no branch label). It would trade a
  lost-work bug for a silently-skipped-iteration bug.

- **Persist the child id in the resume `Checkpoint`.** Rejected: the checkpoint
  is saved *after* a node finishes, but a parked subbot node has not finished —
  the last checkpoint is the previous node's. The record must be written at
  child launch, independent of the checkpoint lifecycle. The run doc is that
  independent home.

## Consequences

- A parent parked on a child's human gate now survives a process restart: a
  resume with the child still paused simply re-parks; a resume after the child
  was answered picks up its output; no fresh child, no lost work, no lingering
  stale review.
- Both surfaces (studio in-process + CLI) share `ReattachSubbotChild`, so a bot
  behaves identically on either — the same invariant the recursive-runner and
  `AwaitSubbotTerminal` wiring already hold.
- Every subbot spawn now does one extra parent `LoadRun` + one small map write;
  negligible against launching a child engine.
- The record is best-effort (failures log, never abort the run): the worst-case
  degradation is exactly today's spawn-fresh behaviour.
- The pipeline-board stopgap ("answering reviews alone won't finish an
  interrupted pipeline") is now largely obsolete — a resumed parent re-attaches
  — though a resume of the parent is still required.
