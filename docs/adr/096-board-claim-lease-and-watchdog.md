# ADR-096: Board claim lease + fenced watchdog

- Status: accepted
- Date: 2026-09-02
- Extends: ADR-028 (dispatcher actor / tracker-is-claim-authority)
- Cites: ADR-094 (trigger-effect outbox — the lease+fencing shape),
  ADR-070 (k8s orphan GC — the TTL+parenthood+reaper triad, conserve on
  doubt), ADR-014 (a dispatched paused run is *parked*, its retained
  claim load-bearing), ADR-095 (terminal-state contract + failure
  taxonomy — the reserved `PROCESS_ORPHANED` writer)

## Context

The dispatcher claim was a bare marker string `<host>-<pid>` on the
board card, with **no lease**. The only expiry sweep ran once at boot
and only reclaimed **same-host, pid-gone** markers — a dead owner on
another host, or the cloud board dispatcher's `board-dispatcher:<uuid>`
marker, was never reclaimed. `boarddispatch.go` documented the hole in
its own comment (R5ceb26: *"no TTL and no reaper exists yet"*). A card
whose owner died mid-run stayed claimed forever, and `SetState` allowed
any→any transitions, so a done/blocked card could be silently
resurrected.

C1 slice 1/3 (ADR-095) gave us the terminal-state predicates and a
`PROCESS_ORPHANED` code reserved *for this card*. This is slice 2/3: the
detect→act machinery.

## Decision

**1. The claim carries a persisted, fenced lease.** Every card gains
`ClaimEpoch` (a per-issue fencing counter, bumped on every fresh
acquisition — never on an idempotent same-marker re-claim), `ClaimedAt`,
and a single indexed `ClaimLeaseUntil`. `Claim` returns a
`tracker.ClaimToken{Marker, Epoch}`; the owner heartbeats it
(`RenewClaim`, lease/3 cadence) for the whole hold — dispatch through the
finish worker's last write locally, the whole unbounded poll-to-terminal
in the cloud. On Mongo the lease is stamped with the **server clock**
(`$$NOW`) so a pod with a fast local clock cannot mint itself extra
lease.

**2. Every owner write is fenced.** `SetStateOwned`, `SetLastRunOwned`,
`SetAwaitingInputOwned`, `SetGaveUpOwned`, `ReleaseOwned`, `RenewClaim`
are CAS on `(claim, claim_epoch)` — a worker whose claim was stolen
finds every late write refused (`ErrClaimConflict`) instead of
clobbering the new owner's state. This is ADR-094's effect-outbox
contract applied to the board card. On loss the local session posts
`cmdClaimLost` (the actor cancels the run with `ErrRunInterrupted`) and
the cloud worker cancels its poll — the fenced writes would refuse
everything anyway; the cancel just stops burning tokens toward refusals.

**3. The reaper reclaims by TRANSFER, never clear-then-decide.** A
periodic (60s), cross-host reaper lists cards whose lease expired,
resolves each card's run (through the `promoteIfOrphaned` liveness
oracle — it only ever promotes when it can *prove* no live owner holds
the run lock), consults `DecideStuckCard`, and for actionable rows
**CAS-transfers the claim to a recovery owner** before touching
anything. Transfer-first is load-bearing: an eligible-state card *freed*
before its disposition is decided is instantly re-dispatchable by the
very tick the reaper is cleaning up after (a double-launch window). The
CAS carries the whole precondition (claim still exactly `prev` AND still
expired), so a renewal, an operator, or another replica's reaper between
the list and the act is a clean skip.

**4. One decision table, liveness-first.** `DecideStuckCard(run, err,
card)` is pure and shared by the local reaper and the cloud sweeps, so
two authorities can never classify the same situation differently. A read
error conserves; a running/queued run is never stolen from; a paused run
keeps ADR-014's parking brake; an operator cancel is never auto-routed;
a platform continuation (redelivery/retry armed) owns its future;
finished→complete, terminal-failure→fail, resumable→back to the pool; an
unknown status is conserved, never guessed at.

It judges the card as well as the run, and both of those rows are bounded
on purpose — an unbounded "keep" is the same outcome, for the operator,
as the stuck card this watchdog exists to clear:

- *No run recorded but the card is in the running column* → keep, because
  the run stamp is best-effort and lands AFTER the launch, so its absence
  proves nothing and freeing the card could double-launch a live worker.
  Bounded by the claim's own age (`StampWindowOpen`): the real window is
  seconds, and past two leases "the stamp is late" stops being credible.
- *A card parked outside the dispatch pool* → keep, because returning it
  to the pool only means something if the pool can reach it; releasing
  lifts a brake somebody set. A decision that flips to keep only AFTER
  the transfer is granted **once** — the recovery marker left on the
  claim is the record of that grant.

The decision is taken on the state the TRANSFER observed, never on the
listing's copy: the listing only selects a candidate, and an operator can
move the card in the window between the two.

**5. Terminal states are sinks.** Leaving a `Terminal:true` state through
the ordinary `SetState` family is refused (`ErrTerminalStateExit`, which
*wraps* `ErrTransitionRejected` so existing matchers keep working);
terminal→terminal stays an ordinary operator refiling. The one exit is
`Reopen` — operator surfaces only, working-state target, refused while
dependents already promoted on this card's DONE are outstanding.
Automated writers use `SetStateFrom` (CAS on the declared source). Bots
(`board.move`) get the refusal with **no** fallback — a run must not
drag a card out of done.

**6. The fence cannot repeat, and a lost fence is refused rather than
guessed.** The epoch is floored at the server clock, so it is monotone
even across a write that DROPS it: derived from the document alone it
would restart at 1, and since markers are per-process rather than
per-claim, two successive holds by one worker would be handed identical
tokens — the fence defeated with no exotic premise. When the field is
missing entirely the fence refuses *everyone*, including the live holder:
admitting a document with no epoch admits every generation of that
marker, which lets a superseded token re-stamp the fence at its own older
value and lock the live holder out of its own card. A refused holder
stops cleanly, which is the safe failure; the card stays recoverable
through the candidate listing's un-leased arm, on a much longer horizon
(a missing lease is an absence, where an expired one is positive evidence
a heartbeat stopped).

**7. The twins diverge on purpose, in three places.** Written down
because an undocumented divergence reads as an oversight, and the next
contributor "fixes" it:

- The FS listing has no un-leased arm. It cannot need one: its store is
  a local file its own process rewrites whole, so no peer strips a lease
  from under it, and its boot sweep already probes the pid behind a
  same-host marker.
- `watchdogRunCeiling` is cloud-only. Returning a card to the pool costs
  a FRESH run there (that launcher cannot resume a recorded one); the
  local dispatcher resumes the run the card records, so repeating costs
  nothing new and needs no ceiling.
- The epoch is a counter on the FS twin and a server-clock floor on the
  Mongo one. The shared contract is monotonicity, not the increment —
  only the Mongo twin can lose the field to another binary's write, and
  only it therefore needs a floor that survives the loss.

**8. What the gate stops, and what it must not.**
`ITERION_BOARD_CLAIM_REAPER` is what an operator pulls when they judge
the watchdog itself wrong, so it gates DECISIONS: with it off, nothing
files a card, returns it to the pool, or otherwise rules on it. It does
not gate REPAIR. Two populations are left behind by a watchdog that is
switched off — its own abandoned recovery claims, and ordinary claims a
mixed-fleet write stripped of lease and fence (whose holder can then
neither renew, write, nor release). Both are swept at startup, ungated
and release-only: dropping a claim nobody can use restores the card to
what it was, whereas filing it would be a decision, and a terminal one
promotes dependents past the point where Reopen can undo it.

**9. Rollout: expand/contract, two releases.** `ITERION_BOARD_CLAIM_REAPER`
gates the reaper, default **off**. Release N ships the lease fields +
heartbeats + fenced writes with the reaper off, so a mixed fleet can
never reap a claim an old binary silently un-leased. The `replace()`
neutralisation (commit 0) protects one direction only — a NEW binary no
longer erases the claim family; an OLD binary's full-document
`ReplaceOne` still strips epoch + lease from any card it writes (§8),
which locks the live holder out of all three fenced verbs and leaves an
un-leased claim behind. That residue is swept on the watchdog cadence,
**guarded twice**: the QUERY selects only running-column cards with a
recorded run (the batch cap applies at the query, so a post-hoc filter
would let the conserved population — never written, always oldest in
the updatedat order — permanently starve the batch), and the release
fires only for a disposition the fork-adoption reconciler then FILES —
finished, terminal failure, or a settled failed_resumable (the
reconciler's own reach; a conserved claim hides the card from
ListEligible and therefore from that reconciler, for ever). A PRUNED
pointer released bare would sit unclaimed and unfiled in the running
column — the reconciler cannot read it — and a kept claim (pause brake,
operator cancel, armed continuation) is load-bearing: those stay
conserved for the gated reap's own two-arm listing, like the no-run
shape (proves nothing) and the launch-column shape (a bare release
re-arms a fresh spend).
The un-leased claim is not the only thing an old binary's full-document
`ReplaceOne` re-applies: it also rewrites **state, labels, runs, gaveup
and comments** from its stale snapshot — the exact lost-update class the
scoped `$set` closed (a terminal filing rewound, a consumed one-shot
label restored) stays OPEN wherever an old pod writes, for the duration
of the rolling window. Nothing sweeps that residue (a rewound state or a
resurrected label is indistinguishable from an operator's own write
after the fact); the bound is the rolling window itself, which is why
release N must fully drain the old ReplicaSet before N+1 turns any
decision-making on.

Two label-resurrection shapes are NOT bounded by that window, because
they need no old binary. The admin label sweeps (rename/merge/delete)
transform a `listAll` snapshot that ages for the whole walk; their write
is CAS-guarded on the labels each card was read with and re-transformed
on a miss, so a one-shot consumed mid-sweep stays consumed (this was a
real same-version lost update, closed at the choke). What remains — on
BOTH twins, deliberately — is intent: `set_labels` is an ABSOLUTE list,
so a bot that composed its list from a read taken before the consume
re-arms the trigger by stating it. That is the API's shape (one-shots
are documented re-armable), not a lost update; an add/remove label
operation is the follow-up that would remove the foot-gun.
Release N+1, once no old binary can un-lease a claim, enables the
reaper — and with it the full decision table over whatever the
mixed-fleet window stranded. The other gate-independent releaser is the
recovery-claim sweep (`reaper:<host>` markers, every watchdog pass):
releasing an abandoned recovery claim is a pure return to the card's
pre-watchdog state, which is what the rollback lever promises.

## Consequences

- The reaper reuses the existing event vocabulary (`EvtIssueClaimed` for
  a reclaim, `EvtIssueState` for a Reopen or a filing) rather than
  minting audit types. Consequence, pinned by test: a machine-caused
  event — the enumerated `tracker.IsMachineReason` provenance
  (`watchdog`, the schema migrations) — **fires no effect at all**: not
  a launch, not a board promote, and no `consume_labels` one-shot. A
  subscription is written for an operator's (or a bot's) gesture; gating
  only the one-shot left the ordinary launch open, and a column rename
  (one event per card) mass-launched a run per card nobody moved. The
  events still FLOW (audit tails and projections see every transition,
  actor blanked); only the effect layer declines them. `unblocked` is
  deliberately NOT machine — the auto-promote is the cascade of closing
  a card's blocker, and its triggers keep firing.
- **The provenance is split by what the write SAYS, not by who wrote
  it.** The watchdog's TERMINAL filings carry the run's own descriptive
  verdict (`run_finished` / `run_failed` — outside `IsMachineReason`),
  so the card's downstream chain fires exactly as it would have for the
  living owner: the watchdog merely writes what the run already decided.
  Everything the machine DECIDES on its own — a repark into a launch
  column, the run-ceiling filing (whose run is merely resumable, not
  failed) — stays under the machine reason: firing there would re-arm a
  spend on a card nobody moved. The rule is target-and-truth-shaped and
  enforced on both twins (`fileStuckCard` blanks the reason for any
  launch-column target; the cross-twin conformance suite pins the
  reasoned write). Reclaim audit lives in the monotone `ClaimEpoch`
  and the Warn log line, not a separate event.
- `reconcileStalled` stays the actor's in-memory authority over the local
  concurrency SLOT; the reaper is the authority over the persisted CLAIM.
  Disjoint roles, documented, so the two never fight over the same run.
- Legacy claims (no lease stamped) are never reaped by time — only the
  historical same-host pid-probe sweep touches them. A deliberate long
  operator stop that wants the OFF-window claims left alone advances the
  lease; the reaper's transfer is fail-safe otherwise.
- **A PRUNED pointer is a give-up, not a release.** "No run loaded" has
  two causes the table keeps apart through `StuckCard.RecordedRunID`. No
  run *recorded* is the claimant dying before launch — release-only, the
  card is simply eligible again. A recorded run that is *gone* (pruned by
  `iterion runs prune`, deleted behind a tombstone) means a run happened
  and nobody can tell whether its work was delivered: released bare, the
  card re-entered the pool (the running column is eligible locally; the
  cloud repark writes it back to `ready`) and the launch path read the
  absence as a legitimate fresh start — a NEW run minted on the
  watchdog's authority for possibly-delivered work. Such a card is filed
  into the failed column under MACHINE provenance (no downstream chain
  fires) with a give-up stamp naming the gone run and the reason
  (`GiveUp.Reason`, rendered in the pipeline board's Needs-attention
  lane), and an operator decides: reopen or re-queue to run it again,
  close to acknowledge. The stamp window is irrelevant to this row — the
  pointer is the stamp. Under a disabled gate the un-leased sweep keeps
  conserving it: it is a decision, and the gate stops decisions.

## Non-goals (slice 3/3)

Webhook `Delivery.Status` reconciliation, the nested-subbot parent
backpointer, cross-boundary retry-counter reconciliation. General
optimistic versioning of the whole board document beyond the claim
fields (the residual `replace()` lost-update on comments/labels) is a
separate follow-up.
