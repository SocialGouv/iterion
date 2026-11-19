# Run action origin

Companion to [run-ws-origin.md](run-ws-origin.md), whose gate admits a request
on its expected birth. Admission is a read: the run can be replaced right after
it. An admitted action is held to the run it was admitted on
(`pkg/server/run_origin_action.go`, `runview.ActionOrigin`).

## Carriers

A handler reads the gate's admission for the path's run (`admittedAction`).
Unchecked — no expectation, `ITERION_RUN_BIRTH_CHECK=0`, another run than the
path's — it calls its service as the CLI, MCP and bots do: the request context
unbound, no read added. Checked, it uses one carrier, frozen per route and
envelope by `TestEveryRunMutationHasACarrier`:

- **Bound.** The service call runs under `store.BindRunObservation` of the
  admitted origin — never the identity context, nothing an execution keeps —
  so each store access refuses a successor: merge and its conflict routes,
  commit-and-finalize, rename, delete, tags, notes, watch, queued messages and
  their cancel, file content, the inactive cancel's flip. The three GET reads
  take the same binding and keep their own failure mapping.
- **Expected.** An action that continues an execution or reads other runs
  carries `runview.ActionOrigin`: `Verify` compares a loaded run with it
  (header and stored tenant), `Bind` binds a named store-only write of that
  run; the execution never inherits the binding. Resume verifies its load
  before its admission and orphan reconcile, binds its budget writes and
  `spawnRun`'s budget replay, and verifies again under the spawn lock.
  Answer-human and host events resume with it; an interaction answer's loads,
  record, queue and pending re-check run bound and its auto-resume carries it.
  Rewind verifies after its load, fork verifies its source (the child is
  created unbound), assistant watch and mission creation verify the target.
  Their other reads stay unbound: the loads of the resume handler (its
  answer-upload promotion runs bound), answer-human and the host event before
  Resume, the chat-history projection, a watch's target snapshot.
- **Pre-checked.** A signal keyed by run ID — the in-process and dispatcher
  cancel, pause, bump-loop and raise-budget, browser attach — follows a bound
  load of the run.
- A watch or mission stop is keyed by its own ID; the shell and the CDP pump
  are admitted at the upgrade only.

A body field is compared right after the handler's own load of that run (a
mission reattach loads it first).

## Failure rule

A checked action that fails, whatever the error, reads the addressed run's
origin again, unbound (`Service.CaptureRunOrigin`). Only when that run is gone
or holds another birth does it answer 409 `run_origin_lost` naming the run,
with a Warn log naming the route, the admitted birth and what holds the ID now;
otherwise the handler's own mapping answers. A birth never returns once
replaced, so the verdict is read from the run itself: another run's absence (a
body-carried run, an assistant run, a projection's source) never marks the
addressed run lost. A run store that may drop the context
(`store.RequireBoundObservationStore`) is refused before the action: 503
`scope_unavailable`.

## Store doors

The filesystem store compares a write's binding under its mutex and the run
lock. A bound `DeleteRun` parses the strict header of the `run.json` it would
remove and compares the binding and the stored tenant, as a bound load does; a
bound delete of an absent or deleted run answers `ErrRunNotFound` /
`ErrRunDeleted`.
Unbound deletes (the prune, the CLI, internal callers) check the format only.

Mongo compares the binding at each read. `SaveRun`, the child-record
transactions, the merge doors and `DeleteRun`'s transaction write
conditionally on what that read saw. Under a binding every metadata write
through `runMetadataFormat.filter` (`updateRunMetadata`,
`findOneAndUpdateRunMetadata`: outcome and status CAS, watched issues, budget
writes, the queued flip) also evaluates the bound birth inside the write, `""`
included; a miss answers `ErrRunDataIdentityChanged`. Unbound metadata writes
carry no birth predicate.

## Cloud

`SubmitResume` verifies the expectation on the document it republishes from
and runs its queued claim bound; credentials, stamps, the publication and the
rollback after the claim keep the unbound context. A checked cancel calls
`Service.CancelCtx` with its bound context detached from the request
(`context.WithoutCancel`): `CancelRunWithReason` keeps the binding through
`store.WithoutTenantFilter`, so its load and status flip refuse a successor,
and its NATS cancel follows only a flip that changed the run. An unchecked
cancel is `Cancel`, on `context.Background`.

## The run socket

On a socket admitted on an expected birth, with `ITERION_RUN_BIRTH_CHECK` not
`0` when the envelope arrives: `queue_message` and `cancel_queued_message` run
bound, `cancel` follows the HTTP cancel, `pause`, `bump_loop` and
`raise_budget` are pre-checked, and `answer`'s resume carries the origin while
its load and chat-history read stay unbound. A failure follows the rule above,
then closes the socket `scope_lost`. Any other socket makes the unbound calls;
the bound load of [run-ws-origin.md](run-ws-origin.md) still precedes each
mutating envelope. No header, field, error code, close token, queue or NATS
message is added: older studios, runners and CLIs see the same wire.

## What this does not establish

- Signals keyed by run ID — the Manager's cancel and pause, the dispatcher's
  cancel, the steering channel and NATS steer, the async-answer wake, browser
  attach, the NATS cancel subject — are pre-checked, not bound: a run deleted,
  recreated and launched between the pre-check and the signal receives it.
- Rewind's writes after its `Verify` are check-then-act. A resume its `Verify`
  refuses under the spawn lock still runs `FireForRun` and `emitRunOutcome` for
  its run ID, whichever run holds it.
- A merge's forge token (`forgeTokenForRun`) is opened for the record the bound
  load returned, under no birth condition. Mongo's delete releases
  credential-slot holders by run ID across tenants (`credSlots.UpdateMany`, by
  design), only under the bound run's pending tombstone.
- Effects outside the run store — git between the bound load and the CAS, the
  workspace file write, model calls, forge pushes: the bound load guarantees
  the record they derive from, not the files they touch.
- Body-carried runs (handoff, delegation and launch `source_run_id`, launch
  `parent_run_id`, the assistant run of watches and missions) are
  check-then-act. The fork reads its source after the check without a binding:
  a source replaced mid-fork can leak its successor's data into the child.
- Assistant watches and missions record their target's run ID, no birth: they
  keep following the ID after a replacement.
- The confirm read: "lost" is monotone, "intact" is a read a later replacement
  can overtake; under a non-primary Mongo read preference both reads can lag.
- The socket's act rung reads the run's team (`runTenant`) unbound right after
  `originHolds`, under DisableAuth's dev identity as under a credential.
- A bound delete racing another delete of its run answers 204 on Mongo (the
  tombstone keeps the birth: the idempotent success) and 409 on the filesystem
  (`ErrRunDeleted`); neither reaches another run.
- An interaction answer's auto-resume sends no answers, which
  `validateResumable` refuses for a run paused on a human: that resume does not
  run, expected or not.
- The socket `answer`'s chat-history failure has no witness: no test surface
  declares `history:`.
- Authorization: the birth is provenance; standing ticks and the mutation gate
  stay the authority.
