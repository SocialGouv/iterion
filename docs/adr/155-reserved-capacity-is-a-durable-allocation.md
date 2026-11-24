# ADR-155: reserved capacity is a durable allocation — one ledger, one atomic acquire, a settle and a sweep

Status: accepted — the decisions are taken: the operator decided on 2026-11-03 that the shape stands as written and that a reservation's identity is tier-qualified (decision 11). Design only — nothing below is built; the units are tracked under #950.
Date: 2026-11-03
Issue: SocialGouv/iterion#950

## Context

On 2026-09-08 three teams held what read as three credentials and were one
Anthropic account. When the shared five-hour window closed at 06:14Z every
`claude_code` run was refused — eight parked in six minutes, fourteen
within the hour. No cap had been exceeded and no quota breached: every
individual run stayed under its own ceiling all the way down. Review
simply had nothing held for it.

Two units have landed since, and both are **accounting**:

- **#1069 made the repository an accounting dimension.** `credusage.Key`
  carries `RepoID`; `docID` leaves an empty one out entirely, so no stored
  credential-month is re-identified by the deploy that adds it; `ListByRepo`
  is the read the dimension adds, and every listing that predates it sums
  the repositories back together.
- **#1099 shipped `pkg/budgetfloor` — a measured PREFERENCE.** The package
  doc states the bound first: *nothing is allocated*. `WindowCeiling`,
  `OtherReserved`, `OtherReservedSlots` and `OtherReservedUSD` are
  arithmetic over a ceiling the operator already set, and the gates consult
  them to **lower** that ceiling for every bot no reservation names.

What a preference cannot do, read on `70b5b1972`:

- **Admission acquires nothing.** `gateConcurrency` subtracts the held
  slots, calls `CountActiveRunsByTenant`, compares, returns: two replicas
  both read `cap - held - 1` and both admit. `gateMonthlyCaps` subtracts
  the held dollars and hands the remainder to `orgusage.AllowRun`, whose
  run arm *is* atomic (`$inc`, post-increment read, rollback on refusal)
  and whose cost arm reads `cost_usd_millis` — a counter `AddSpend`
  accumulates after an attempt ends. A reservation is therefore judged
  against spend already **reported**, never against spend already
  **promised**.
- **A run admitted under the bias is not held to it.** The publisher
  computes the bot's lowered policy (`capPolicyFor` → `botCapPolicy`
  `{effective, deployment, heldOut}`) and keeps it to itself; the runner's
  `usageGuardFor` re-reads `r.cfg.UsageCapSource`, the deployment-wide one.
  `grep -rn budgetfloor pkg/runner/` returns nothing: the runner cannot
  reconstruct the fact, it has no bot→reservation resolver at all.
- **The pool is unreachable for a workload that holds a credential.**
  `walkCredentialPlan` deactivates `credentialTierPool` when
  `hasCredentials()` — `holdsDefaultLLMCredential(bundle)` in live
  resolution, `len(x.api)+len(x.oauth) > 0` in the preview — and the tier's
  own BODY then re-asks the question as
  `len(bundle.APIKeys) == 0 && len(bundle.OAuthCredentials) == 0`, a second
  guard the preview does not carry. A floor-HELD credential is never sealed,
  so the bundle *is* empty at both and an unreserved bot the reserve held
  out does reach the pool — deliberate, and the restore step refuses to hand
  the held credential back. What stays inexpressible is the ticket's own
  ask: Revi on a pool of forfaits *while Revi's team holds a healthy
  credential*.
- **Execution capacity is not reservable at all.** `MaxAckPending` is one
  process-wide int applied verbatim to the shared durable and to every pool
  consumer (`poolConsumerConfig`); its only knob is
  `ITERION_NATS_MAX_ACK_PENDING`, and `grep -rni maxackpending charts/`
  returns nothing — the chart cannot express it. KEDA is one `ScaledObject`
  for the shared runner deployment; pool lanes are fixed-replica.
- **The meter has no workload dimension.** A repo quota expressed as a share
  of `review-pr`'s reservation (`ReserveSharePercent` + `ShareOfBot`) is
  consumed by *any* workload, because `credusage.Key` carries no bot.
- **Policy absent and policy unavailable are one answer.** The `platformcfg`
  resolver is process-local — one TTL cache per replica, `Invalidate` local —
  and a cold-start read failure leaves the cache nil, which
  `budgetFloorPolicy` maps to the zero policy: reserves nothing.

D31 widened the ask past the credential budget: **execution capacity** —
NATS `MaxAckPending`, the KEDA replica ceiling — is reservable too.

The 2026-09-10 cross-family plan review (14 findings) is the other input,
and the numbers below are the comment's own "Follow-ups recorded from the
review" list — not its three verified-in-source bullets. Closed on main:
the **first verified bullet** (the walk's restore step handing a held
credential back — closed at the class in `skipped_creds.go`, keyed by
fingerprint), which is not follow-up 1; finding 2's **first** half (an
inline `source`'s bot label is warned about and ignored, and an unverified
label is judged as naming no bot); finding 5 (accepted and documented as
by-design on `forge.ProjectPathFromCloneURL`). Live, each against what
closes it: finding 1 — the pool does not participate in arbitration —
decision 6 and unit 5; finding 3, decision 9; findings 4 and 6, decisions 7
and 10; finding 7, decision 8 and unit 10; finding 8, unit 9; finding 2's
second half, decision 11 and unit 11.

## Options considered

**Where the allocation lives.**

- *A — generalise `pkg/credpool.Ledger`.* It is the closest thing the repo
  has: `Reserve`/`Renew`/`AddSpend`, `LiveCommitment` derived from the open
  leases, `decide()` judging `settled + outstanding`, `ReleaseExpired`
  swept every two minutes. Rejected, and the reason is the keystone of this
  record: it closes the **overshoot**, not the multi-replica race. Its
  concurrency axis is a TOCTOU — `live LiveCommitment` is read in a separate
  round trip by the broker and passed *in* as a parameter, so two replicas
  both see `Runs = N-1` and both admit — and the `Ledger` doc says so
  outright: "the daily bucket is the atomic one … the weekly cap and the
  concurrency reading are checked around it and are soft by nature".
  `decide()` has no `requested` term either. Cloning that shape faithfully
  would leave the two-replica witness red after the fix.
- *B — grow `pkg/budgetfloor` into a stateful package.* Cheapest diff, and
  it breaks the property its own doc makes load-bearing: the package is a
  stdlib **leaf** precisely so the launch gate and the credential walk can
  import it without inverting the graph. Rejected.
- *C — a new leaf `pkg/capacityledger`, with `credpool.Ledger` and
  `orgusage.Counter` becoming adapters over it.* One tracker, the
  `pkg/errtrack` rule generalised, and the Nth-variant test of philosophy 2:
  the next reservable subject implements the seam instead of growing an
  engine `if`. It costs two adapters and a conformance suite that pins both
  twins. **Chosen.**

**How the acquire becomes atomic.**

- *A — derive `Outstanding` from the live allocations.* What `credpool` does,
  and for a good reason (an abandoned run must never leave a slot
  permanently consumed). Rejected as the admission path: a derivation is a
  second query, which is the window this record exists to close.
- *B — `$inc` an outstanding counter in ONE write, read the
  post-increment document, compensate the increment on refusal.* The only
  atomic admission pattern this repo has (`orgusage.AllowRun`,
  `credpool.MongoLedger.Reserve`'s day bucket), so an operator reasoning
  about one quota system reasons about all three. The cost is that
  crash-safety moves out of the data shape and into a sweep. **Chosen.**
- *C — insert an allocation document under a unique index and count in the
  same operation's result.* Keeps per-allocation provenance, and asks Mongo
  for a count whose cost grows with the subject's history on the hot path.
  Rejected for admission; the per-allocation record is kept as the settle
  and sweep subject, not as the counter.

**Who runs the reconcile net.**

- *A — elect it through `pkg/lease`.* Rejected as a correctness claim:
  `pkg/lease`'s own doc calls it load reduction, "not a mutual-exclusion
  guarantee", and tells the caller that work which must never run twice
  needs a claim on the item itself.
- *B — tick on every replica, CAS on still-open per item.* What
  `credPoolSweeper` already does, deliberately: "closing a lease is
  conditional on it still being open, so two replicas sweeping the same
  instant produce one close". Election would only reduce cost. **Chosen**,
  with the per-item CAS named as the correctness carrier.

**How the pool becomes reachable for a workload that holds a credential.**

- *A — automatic for any floor-held credential.* This is already today's
  side effect (a held credential is never sealed, so the bundle is empty at
  the pool tier). It does not serve the reserved workload itself, which is
  the ask. Rejected as an answer.
- *B — an explicit per-reservation opt-in, default OFF.* Never changes an
  existing deployment's posture, greppable, and the donor's audience,
  limits and ledger still decide. **Chosen.**
- *C — a deployment-wide switch.* One dial, and it hands every workload the
  donors' capacity rather than the one the operator named. Rejected.

**The concurrency rule, once allocation is exact.**

- *A — switch to the exact rule (count the holders' own active runs,
  subtract only the UNUSED part of each reserve) and keep today's
  conservative rule as the fallback when the ledger is unreachable.*
  `gateConcurrency` documents the over-refusal it accepts today — cap 3,
  reserve 2, the reviewer running 2, the third slot stays unused — and names
  the missing input: a per-bot active count, which the allocation provides.
  **Chosen.**
- *B — keep the conservative rule.* No behaviour change for anyone, and a
  deployment capped at 3 with a reserve of 2 wastes a slot forever.
  Rejected.
- *C — a policy flag.* A fifth dial on a surface that already has four axes,
  for a rule nobody wants to be wrong. Rejected.

**What a reservation names** (finding 2's second half).

- *A — keep the bare bot name as an accepted bound.* No migration, and the
  escape hatch is the operator re-pointing the reservation. Rejected: the
  name is chosen by whichever tenant registers it — botsource rows are
  unique per `(tenant_id, slug)` (`tenant_slug_unique`), and resolution
  serves a team's own row over the platform's by design — so re-pointing
  moves the reservation to another name any team can register next.
- *B — refuse a team bot whose name a reservation holds.* Closes the
  homonym at registration, and takes away the fork the resolver is built to
  serve (a team that forks a bot gets its fork on every launch surface), on
  a deployment that may have configured no reserve worth the refusal.
  Rejected (philosophy 1).
- *C — qualify the identity with the tier that serves it, and resolve
  before judging.* A reservation names the bot AND the tier; the gate
  judges the identity the server's own resolution produced, never the
  request's label. The vocabulary exists already: `launchBot.Tier()` maps
  the resolution onto `store.BotSourceTier*`, which `RunMessage` and the run
  document already carry (`Run.BotSourceTier`). **Chosen** by the operator
  on 2026-11-03.

## Decision

1. **A reservation is a durable ALLOCATION on a named capacity subject.**
   The subject is `(kind, id, axis)` — a credential window, a tenant's
   monthly dollars, a tenant's concurrency slots, a runner pool's in-flight
   ceiling — and admission judges `settled + outstanding` against it, never
   a reporting counter, where `outstanding` is the POST-ADMIT value and so
   already carries `requested`: the repo's own `runsAfterAdmit` convention
   (`Limits.Deny` in `pkg/credpool/store.go` — "the day's run count
   INCLUDING the admission being judged" — reached post-increment by the
   Mongo ledger and `orgusage.AllowRun`), compared **strictly**, so a cap of
   N admits exactly N. An explicit `requested` term belongs only where
   `outstanding` EXCLUDES the asker, as `credpool`'s derived
   `LiveCommitment` does through `excludeRunID`. Adding it on top of a
   post-increment counter counts the asker twice, and the subject's usable
   capacity shrinks by one ask: a cap of N admits N-1 unit allocations, a
   $100 reserve stops $r short where $r is the next ask, and a resume whose
   own allocation is still outstanding is refused by itself — the regression
   `excludeRunID`'s own note records. "Reserved" stops meaning "the
   others' ceiling is lower" and starts meaning "this much is held, and
   something holds it".
2. **`pkg/capacityledger` is a new leaf package** — leaf by its POSITION in
   the graph, not by an empty dependency set. Its judgement core imports
   stdlib plus `pkg/clock` and nothing else, the rule `pkg/budgetfloor`
   holds literally (`context`, `fmt`, `sort`, `strings`, `time`), while the
   Mongo twin beside it adds `pkg/internal/mongoutil` and the driver —
   exactly what `pkg/credusage`, `pkg/orgusage` and `pkg/lease` already are
   (`pkg/usagecap` is that shape plus `pkg/secrets`, for its env half). So
   the launch gate and the credential walk import it without inverting the
   graph, and this decision does not contradict the Mongo twin it asks for
   below.
   `Reserve(subject, requested) → (allowance, AllocationID, DenyReason,
   error)`, `Renew` (a resume re-checks spend without consuming a second
   unit, the rule `credpool.Ledger.Renew` already states),
   `Settle(AllocationID, actual)`, `Release(AllocationID)`,
   `Outstanding(subject)`, `Expire(now, limit)`. ONE judgement function,
   over the POST-ADMIT total: each caller makes its count post-admit before
   calling it — the Mongo twin by reading its own `$inc` back, the memory
   twin by adding `requested` itself, a DERIVED reading by adding it to an
   outstanding that excludes the asker — which is how both `credpool`
   ledgers reach the one copy of `Limits.Deny`. The **error is the last
   value on every call**, as on `credpool.Ledger.Reserve` and
   `orgusage.Counter.AllowRun`: an unreachable ledger is an error, never a
   `DenyReason` — encoding it as a refusal would invert decision 8's
   fail-open doctrine and is the silent fallback the conventions forbid. A
   capped subject with nothing left **denies** rather than granting zero:
   zero on the wire means "no ceiling". Memory and Mongo twins behind one
   conformance suite, the pattern of `pkg/credusage`
   (`NewMemoryCounter`/`NewMongoCounter`), `pkg/usagecap` (`MemStore` in
   `store.go`, `MongoStore` in `mongo.go`, one `runStoreConformance` over
   both) and `pkg/lease`.
3. **The acquire is ONE write whose own post-state yields the count**:
   `$inc` the subject's outstanding counter by `requested`, read the
   post-increment document, compensate with the mirror `$inc` on refusal —
   the shape of `orgusage.AllowRun` and of `credpool`'s day bucket. One
   round trip to admit, two to refuse, and the allow/deny decision itself is
   atomic: `AllowRun`'s own words. `requested` is named PER AXIS, because
   neither precedent has one to copy (`orgusage.AllowRun` takes
   `maxRuns`/`maxCostMillis` and no ask; `credpool.decide` DERIVES the
   allowance through `shareAcrossSlots`): one slot on the countable axes,
   the run's clamped budget ask on the monthly dollars, the reserve's share
   on the window percentage. Incrementing by zero on the money axis would
   leave the counter holding none of the promise and hand two concurrent
   admissions the same remainder — the overshoot this record exists to
   close. Where the ledger grants LESS than the ask, the same path corrects
   the counter to what it promised, by the mirror `$inc` of
   `requested - allowance`: outstanding is never a different quantity from
   the allowance handed out. It is deliberately NOT a *conditional* (filtered) update —
   `{outstanding: {$lte: cap - requested}}` yields no count on a miss and
   cannot tell a full subject from an absent one, which is why the two
   precedents filter on `_id` alone. `credpool.Ledger` and
   `orgusage.Counter` become **adapters** over it, so there is one tracker
   and not three; unit 2 carries them.
4. **The sweeper is a correctness carrier, not a cost optimisation.**
   Because outstanding is a counter and not a derivation, a pod killed
   mid-run holds its allocation until something expires it. The sweep runs
   on **every** replica, closes an allocation conditionally on it still
   being open (so two replicas produce one close), charges **no invented
   spend**, and is bounded per pass — `credPoolSweeper`'s shape, and its
   reasons.
5. **The runner is held to the lowered policy.** The publisher stamps
   `capPolicyFor`'s `effective` policy on the `RunMessage`; `usageGuardFor`
   enforces the stamp. The fact travels **with** the result, written by the
   side that knows. The live re-read of the DEPLOYMENT cap stays, so a cap
   tightened mid-run still bites, and the run is held to the TIGHTER of the
   two: a stamp never LOOSENS what the runner's own source already caps.
   Without that rule the stamp has an uncapping arm — `capPolicyFor` returns
   a ZERO `botCapPolicy` when the publisher has no cap policy wired, and
   `usagecap` reads `MaxPercent` 0 as "unenforced", so an empty stamp would
   be indistinguishable from a real lowered policy of nothing. Both ends
   have an unstamped arm and both fall back to the DEPLOYMENT policy — the
   UNLOWERED ceiling (`capPolicyFor` lowers `effective` only, over a
   `deployment` that stays `base`; `usageGuardFor` reads
   `r.cfg.UsageCapSource`) — so for that run the reserve is not held: the
   lax direction, and neither arm may be silent. A message with no stamp is
   one queued by the previous image of the single binary that publishes
   (`cmd/iterion/server.go`), and its window is RETENTION, not a drain:
   `DefaultStreamMaxAge` (24h) for a queued message, `DefaultDLQMaxAge` (7d)
   for a parked one, which `handleDLQReplay` re-publishes verbatim — the
   frozen payload, stamp absent, reached by an operator gesture rather than
   by a drain. That handler already loads the run doc and decodes the
   message, so it is where a re-stamp belongs; until it carries one, the
   seven days are an accepted bound, logged and counted. A STAMPED message
   on a runner that predates the reader is the long window — a separately
   rolled image — and it is decision 10's case: the reserve counts as unheld
   on the runner side until the fleet's floor clears the release that reads
   the stamp. Both `claw` and `claude_code` lanes read the stamp, or it is
   typed-refused for the one that does not (backend parity).
6. **The pool becomes an arbitrated capacity tier.** A reservation may be
   marked pool-eligible (default OFF), and then `credentialTierPool` stays
   active for that workload even though it holds its own credential.
   Eligibility is ONE predicate — `poolEligible(bundle, botID)` — called by
   the plan arm, by the tier body's own guard and by the preview. The body's
   guard is why flipping the plan arm alone changes nothing: the tier is
   reached and the grant refused at the acquire, while the preview reports
   the tier ACTIVE — the disagreement this decision exists to prevent, and a
   guard copied into the body is how it returns. `budgetFloorWarnings` gains
   its third advisory shape: a reservation marked pool-eligible on a
   deployment where no enabled pool admits its tenant holds nothing.
7. **The meter carries the workload.** `credusage.Key` and `Spend` gain
   `BotID` and, beside it, decision 11's reservation tier (`platform` or
   `team`; a `team` tier's team is the key's own `TenantID`, because the
   team tier serves a bot to the team that owns it alone) in a field of its
   own — never `Key.Tier`, which already names the CREDENTIAL's resolution
   tier and shares the words `team` and `platform`. `docID` leaves an empty
   workload out exactly as it does for `RepoID`, and `aggregateRepos`
   becomes ONE fold over both additive dimensions — the workload cleared
   beside the repo. The write half alone is not additive: it
   splits a credential-month into rows no pre-dimension listing can tell
   apart, since `List`, `ListByFingerprint` and `ListByTier` all read
   through that fold (`summed`) and `MonthlyUsage` exposes no bot at all —
   the reporting outage `docID`'s own comment describes, in the lax
   direction. `ListByRepo` keeps its per-repository rows and folds the bots
   inside each. A repo's share of a reservation resolves against **that
   reservation's** consumption. Enforcement of the new dimension is armed
   only on a fleet **verified** to write it, by decision 10's predicate. A
   fleet that is not cleared keeps the pre-dimension reading — the
   repository's **total** — never the bot-attributed subset, because the
   subset is the lax direction: money leaks and nothing alerts (finding 6).
8. **Policy unavailable is not policy absent.** The resolver keeps its
   process-local TTL cache, and BOTH readers of that record stop reading an
   unreadable policy as "no reservations": `budgetFloorPolicy` at the launch
   gate and `capPolicyFor` at the credential walk each map a nil record to
   the zero policy today, so fixing the gate alone would leave the window
   axis silently un-reserved on the very same cold start. One readiness
   accessor serves both. The two arms differ, and only one of them has a
   last-known value to hold a reserve on. A WARM cache already serves one:
   the resolver warns "settings read failed — serving last-known values" and
   returns the cached record, so the reserve is held on it and the
   conservative concurrency rule stands in while the ledger is unreachable.
   A COLD start that never resolved has NO policy: `cached` stays nil,
   `ready` stays false, and `Get` returns a bare `*T`, so today
   `budgetFloorPolicy` maps nil to the zero policy and cannot tell "nothing
   written" from "never read". Unit 10 exposes that distinction —
   `Resolver.ready` becomes readable — and takes the **admit** direction:
   refusing every launch on a deployment that may have configured no reserve
   at all is the worse failure (philosophy 1), so the launch is admitted
   with no allocation and the fact is logged AND counted under its own
   reason detail. Fail-open stays the doctrine for a degraded store read —
   `gateLaunch`'s doc comment owns it — made observable instead of silent.
9. **Execution capacity is reservable per pool.** `RunnerPool` gains an
   in-flight ceiling that overrides `cfg.MaxAckPending` for that pool's
   consumer (`poolConsumerConfig` is the seam; `0` inherits
   `cfg.MaxAckPending` — the operator's `ITERION_NATS_MAX_ACK_PENDING`,
   which `applyDefaults` has already resolved to `DefaultMaxAckPending`
   *only* when nothing was set, so no pool is re-capped below a global the
   operator raised, and the historic pin of `1` that serialised the whole
   fleet is the lesson this must not re-pay). The chart carries
   `ITERION_NATS_MAX_ACK_PENDING` and the per-pool value, and renders one
   `ScaledObject` per pool, gated on that pool's own `keda.enabled` so a
   cluster without KEDA still renders. **The prod numbers are the
   operator's gesture**, not this record's: `values-prod.yaml` keeps
   `keda.enabled: false`, and the infra-apps commit lands on its own lane.
10. **A rollout asymmetry is judged on the fleet's verified floor, and the
    unverified arm takes the CONSERVATIVE direction, never the lax one.**
    Decisions 5 and 7 each add a field one component writes and another
    reads, across components that roll separately — the publisher ships in
    the server binary, the runners in their own image — so both cross this
    one rule rather than carrying a guard each. **Cleared** is a predicate,
    not a helper: at least one runner build observed inside
    `engineFloorLookback`, every observed build orderable, and all of them
    at or above the release that ships the field — EXCEPT where the engine
    executes in this very process (`cfg.Mode != "cloud"`, the marker the
    server already keys local mode on): there the writer IS the reader,
    `appinfo` answers for both halves, and the arm is CLEARED. Reading "no
    observer wired" as unverified would unarm decision 7 forever on the one
    topology that knows its own build with certainty. `s.engineFloor` does
    NOT answer the predicate — with no observer wired, on an
    `ObservedRunnerBuilds` error and on an EMPTY observation it returns this
    server's OWN build as the floor, which on a current server reads as
    cleared, and `sources` does not separate those three either: each
    returns `["server <self>"]` byte-identically, so it tells observed from
    not-observed and nothing finer. Its one existing consumer PASSES an
    unorderable floor with a warning (`requiresFloorGap`'s `EngineUnknown`
    arm), the polarity this gate inverts. So the unit owes a readiness
    accessor beside `engineFloor` that carries the ARM itself, with each
    unverified one witnessed. An unverified fleet is a WARNING and never a
    refusal (philosophy 1), and the conservative reading it falls back to is
    logged and counted, so a rollout is observable rather than silent.
11. **A reservation's identity is tier-qualified, and the gate resolves the
    bot before it judges.** This record first kept the bare name as an
    accepted bound: `Reservation.BotID` carries no tier, `sameID` folds the
    name only, and the launch handler's first `gateLaunch` takes the
    request's label (`launchBotLabel`) before `resolveBotTiered` runs — so
    a tenant that registers a team bot named `review-pr` is judged as the
    reserved workload, and consumes or is refused on the platform bot's
    band. The operator decided on 2026-11-03 to close it, and this decision
    replaces the bound. A reservation names its tier beside the bot:
    `platform` for the deployment's bot — served by a platform override or
    the baked catalog, the two tiers the resolver falls to when the
    launching team has no row of that name — and `team` for a team's bot,
    which also names that team, because a team bot's name is unique only
    inside its tenant. A stored reservation that names no tier is read as
    `platform`: every reservation written before this decision protects the
    deployment's bot, and that is the migration the operator decided.
    `RepoQuota.ShareOfBot` names a reservation too, and is qualified by the
    same rule, and `Policy.Validate`'s one-reservation-per-bot check, the
    share's reservation lookup and `Policy.Bots()` key on the qualified
    identity, so a platform reservation and a `team` one of the same name
    coexist. The gate's subject is the RESOLVED identity — the canonical
    name and the tier of the row that served (`launchBot.BotID`,
    `launchBot.Tier()`) — and a continuation's is its run document's
    (`Run.BotSourceTier`; empty before the stamp, it is read through
    `BotSourceTenant`: `botsource.PlatformTenantID` is `platform`, any other
    tenant is `team` for that tenant, empty is `platform`). Only the arms
    that judge the LAUNCH against a reservation read that identity:
    concurrency and the monthly dollars at the launch gate, and the window
    reserve at the credential walk. The repository quota reads a
    reservation too, but the quota's own: `gateRepoQuota` hands `RepoCap`
    nothing but `subj.Repo`, and `RepoCap` resolves the quota's
    `ShareOfBot` — qualified above — whatever is launching, so it never
    reads the launch's identity. The record reads the operator's "before
    it judges" as before it judges the bot — its reading, put to him when
    #2980 is picked up: the arms that read no launch identity — the
    maintenance fence, the teamless and member-rung refusals, the team read
    and its suspend checks, the repository quota, and the launch-rate
    token, which therefore moves ahead of concurrency — keep their place
    ahead of the resolution, so a caller they refuse costs no resolution.
    The rate token does not bound the work of a launch refused AFTER the
    resolution: that refusal hands it back through
    `launchAdmission.rollback`, as a monthly refusal does today (#1726,
    pinned by `TestGateLaunch_RateTokenRefundedOnRollback` and the floor
    test's "a monthly refusal refunds only its rate token"), and a
    concurrency refusal never reached the token at all before this
    decision. What bounds that work is what the reservation arms are fed:
    the RAW resolution (`resolveBotTieredRaw` — the tier walk and, for a
    stored row, its materialization, which the refusal's
    `launchBot.Cleanup` removes), never the cloud snapshot:
    `resolveBotTiered` is that call followed by `snapshotLaunchBot` in
    cloud mode, and the snapshot runs only once the launch is admitted. The
    same qualified identity is what the exact concurrency rule's per-bot
    count and decision 7's workload dimension key on, so a team fork's runs
    and spend never read as the reservation's own.

## Consequences

- **The word "guarantee" becomes accurate on three axes and stays
  qualified on the fourth.** Window, monthly dollars and concurrency slots
  are allocated; the *provider's* own capacity is still learned, not owned.
  The forward consult that exists (`usagecap.Latest` + `Preflight` at
  resolution time) is conditional — `meteredWindow` requires a metered
  backend and a fingerprinted slot — and `Store.Latest`'s contract makes an
  unknown key read as "not blocked". So a credential with no reading in the
  current window has no forward estimate, only a bounded, self-healing read
  of the last observation.
- **Per-axis tenancy is unchanged.** The window axis is deployment-wide
  while monthly dollars and concurrency are per-tenant; a reservation still
  never **creates** a cap that is not configured, and the holder still faces
  the plain cap. Both invariants are pinned by the existing floor tests.
- **One quota key across forge hosts stays by design** (finding 5): the slug
  is the meter's whole key, so two deployments' `acme/widgets` share one
  quota identity. The escape hatch is the one the derivation already names —
  a caller holding the forge's own project path must prefer it.
- **A team's fork of a reserved bot is the team's workload** (decision 11):
  launched under the reserved bot's name, it is judged as other work and
  faces the ceiling the reservation lowers, while bot resolution keeps its
  team-over-platform precedence unchanged. An operator who means to protect
  a team's own bot writes a `team` reservation naming that team. A label
  that resolves nowhere is refused before any arm judges it against a
  reservation, so it never borrows a band on the way to its refusal.
- **An opted-in workload that receives a pool grant loses the platform
  tier**: `poolGranted()` deactivates `credentialTierPlatform` in the same
  plan. That is the existing rule and it is not changed here — it is named
  in the opt-in's warning text and in `docs/credential-pool.md`, because an
  operator who opts a workload in is changing which tier serves it.
- **The pool tier is visited before the restore tier**, which is why a
  floor-held credential leaves the bundle empty at the pool and why the pool
  is reachable at all today. The opt-in changes that tier's precondition,
  not the walk order.
- **#2643** (a hard per-key concurrency cap that parks instead of refusing)
  consumes the same primitive. This record defines the ledger; it does not
  pre-empt that ticket's parking semantics.
- **Observability.** A refusal caused by a reservation is indistinguishable
  today on the series an operator alerts on: `LaunchDeniedTotal` (declared
  in `pkg/cloud/metrics`) is labelled with the reason tokens that are consts
  in `launch_gate.go`, and only the DETAIL names the reserve. An additive
  token for a held-out refusal is part of the last unit; the unreserved
  wording stays byte-identical, because it is quoted in
  `docs/quotas-and-limits.md` and read by operators who configured no floor.
- **What is deliberately left**: the provider's real remaining capacity (no
  API exposes it); fork, rewind and the resumes a run's own machinery drives
  stay outside the gate, as they are today; and the `platformcfg` resolver
  stays process-local — two replicas can judge the same launch under two
  policies for up to a TTL, which the ledger's conformance suite pins
  rather than hides.

## The units, in order — each with the witness it owes

Nothing here is built. The first unit rebases on the landing of
`leap/credpool-fingerprint` and `leap/usagecap-ambient-key`, which hold
`pkg/credpool`, `pkg/runner/usage_cap.go` and the publisher.

> **Amended 2026-11-04 ([ADR-161](161-one-resolver-decides-the-credential-a-call-spends.md), debt-leap #112).** Units 4 and 6 wait on ADR-161's U2, not on the bench landing; the other units start from main.

1. **`pkg/capacityledger`** — the leaf, both twins, the conformance suite.
   Witness: a table test on the judgement over the POST-ADMIT total, and
   the suite run against both twins. Three rows carry it, because the
   overshoot direction alone leaves the other open — `pkg/orgusage`'s own
   "concurrent launches never overshoot the cap" asserts `allowed > cap`
   and nothing else, so it stays green when N-1 are admitted: the boundary
   from ABOVE (N+1 refused), the boundary from BELOW (a cap of N admits
   exactly N), and a resume whose own allocation is still outstanding, which
   `Renew` must not refuse. Three mutations: drop the outstanding term, and
   N allocations each receive the same remainder; add `requested` a second
   time on top of the post-increment read, and the from-BELOW row reddens;
   count the asker's own live allocation in a `Renew`, and the resume row
   reddens. CI's `-race` and `mongo-conformance` jobs are where the Mongo
   twin is judged.
2. **The launch gate allocates, over the two adapters** (`pkg/server`,
   `pkg/orgusage`, `pkg/credpool`) — `gateConcurrency` and
   `gateMonthlyCaps` acquire instead of counting; `launchAdmission.rollback`
   releases beside the run unit and the rate token; the terminal path
   settles. Witness: **two `*Server` values over ONE memory ledger, both
   admitting into the last reserved slot — it must redden on HEAD** (both
   admitted), and it is built in BOTH completion orders, because a test that
   can only redden in one order is flaky rather than a witness. Second
   mutation: drop the release from the rollback, and the slot stays
   consumed. The existing floor tests run unchanged
   (`pkg/server/launch_gate_floor_test.go` on the gate's repo, concurrency
   and monthly-dollar axes, `pkg/server/cloudpublisher/budget_floor_test.go`
   on the credential walk's window reserve).
   The two adapters are decision 3's whole cost and land here, not later:
   `gateMonthlyCaps` reaches the atomic arm THROUGH `orgusage.AllowRun`.
   Their witness is the EXISTING suites run unchanged against the adapter —
   `runCounterSuite` over both `orgusage` twins (`orgusage_test.go`,
   `mongo_test.go`) and the broker tests that drive
   `credpool.NewMemoryLedger`, with `Limits.Deny` still the single copy of
   the donor rule; the mutation delegates one axis and leaves the old
   counter beside it, after which the two disagree and the cap is enforced
   twice. `credpool`'s Mongo ledger has no test of its own today
   (`grep -rln NewMongoLedger` names only the two commands and `mongo.go`),
   so the conformance suite is also its first twin-level coverage.
3. **The reconcile sweep** (`pkg/server`, `pkg/capacityledger`) — every
   replica, per-item CAS, bounded batch, no invented spend. Witness: acquire,
   abandon the holder without settling, one pass returns the slot and
   `Outstanding` drops to zero; the mutation drops the expiry filter. A
   second test asserts two concurrent passes produce exactly ONE close, and
   its observer takes a mutex.
4. **The runner honours the lowered policy** (`pkg/queue`,
   `pkg/server/cloudpublisher`, `pkg/runner`). Witness: a run stamped with a
   lowered five-hour ceiling stops at the LOWERED percentage; the mutation
   reverts the stamp read. Plus the unstamped arm, asserted on its SIGNAL
   and not merely on its fallback: the run is judged on the deployment
   policy AND the fallback is logged and counted, and the mutation drops
   that counter — a test that asserted only the fallback would pin the leak
   as intended behaviour. Plus the EMPTY-stamp row: a stamped but zero
   policy on a runner whose own source caps leaves the run CAPPED, and the
   mutation takes the stamp instead of the tighter of the two. The other
   direction — a stamped message on a runner too old to read it — cannot be
   exercised from inside the new binary; it is decision 10's reading, owed
   by unit 6's arms, and this unit only states that the reserve counts as
   unheld until the floor clears.
5. **The pool as an arbitrated tier** (`pkg/budgetfloor`,
   `pkg/server/cloudpublisher`, the CLI, `pkg/server/platform_settings.go`,
   `docs/credential-pool.md`). Witness: a run holding a healthy credential
   whose reservation is pool-eligible receives a grant and the preview
   reports the same tier — the GRANT it receives, never the plan bit. Two
   mutations: revert the plan arm; revert the tier BODY's guard, after which
   the walk hands out nothing while the preview still reports the tier
   active and `TestBudgetFloor_ThePreviewAnswersWhatTheWalkDoes` reddens. A
   second test pins the default: no opt-in, no pool.
6. **The meter carries the workload** (`pkg/credusage`, `pkg/runner`,
   `pkg/server`, `pkg/budgetfloor`, the studio's typed client). Witness: a
   repo capped at N% of `review-pr`'s reserve is NOT refused by
   `feature-dev`'s spend; a credential-month written under two bots reads as
   ONE row through `List`, `ListByFingerprint` and `ListByTier`, the
   mutation dropping the bot from the fold; a platform `review-pr` and a
   team's fork named `review-pr` stay two workloads in the share's
   consumption, the mutation keying the dimension on the bare name; the
   conformance suite extended
   for both twins; and the finding-6 case — an unverified fleet keeps the
   repository's total, so a runner that writes no bot cannot make the share
   read unconsumed. The mutation drops the engine-floor gate and the share
   reads as free — and one row per arm of decision 10: no observer wired in
   CLOUD mode, an `ObservedRunnerBuilds` error and an EMPTY observation each
   keep the repository's total and emit the warning, while the LOCAL row
   (`cfg.Mode != "cloud"`) ARMS the dimension — and every unverified one
   reads as CLEARED if the gate asks `engineFloor` for a build instead of
   asking the readiness accessor.
7. **Per-pool in-flight ceiling** (`pkg/queue/nats`, `pkg/config`,
   `pkg/platformcfg`). Witness: on a config whose global is **not** the
   compiled default (`MaxAckPending` set as
   `ITERION_NATS_MAX_ACK_PENDING` would set it), two pools' consumer configs
   carry their own ceilings while the shared consumer *and* an
   override-less pool both carry that configured global — asserting
   `DefaultMaxAckPending` instead would pass under either reading and
   witness nothing.
   `TestPoolConsumerConfig` already pins a non-default global
   (`MaxAckPending: 42`): extend that shape rather than write a new one, and
   add one row whose global is ABOVE `DefaultMaxAckPending` (512, the value
   `pkg/config/config_test.go` already sets the env var to), since 42 is
   below it and cannot exhibit a pool re-capped under a global the operator
   RAISED. Two mutations: drop the override, and resolve an absent override
   to `DefaultMaxAckPending` — the second reddens `TestPoolConsumerConfig`
   today, which is why the spelling matters.
8. **The chart exposes it** (`charts/iterion`, `docs/agents/ops/runbooks.md`)
   — the configmap key, the per-pool value, one `ScaledObject` per pool
   gated on its own `keda.enabled`. Witness: `helm lint` on dev and prod
   values, the chart render-diff gate, and a render test asserting the
   per-pool `ScaledObject` names the pool's consumer and that a KEDA-disabled
   pool renders nothing. **Operator gesture**: the prod numbers and the
   infra-apps commit.
9. **A named refusal and the end-to-end witness** (`pkg/server`,
   `pkg/cloud/metrics`, the alert rule, `docs/quotas-and-limits.md`) —
   finding 8. Witness: one test crossing publisher → runner for a
   reservation, allocated at admission and settled at the end; the mutation
   reverts the new reason token and the series can no longer tell the two
   causes apart. A deliberately false assertion must fail first: a test
   never collected proves nothing.
10. **Policy unavailable is not policy absent** (`pkg/platformcfg`,
    `pkg/server`, `pkg/server/cloudpublisher`) — decision 8: the readiness
    accessor, and the two arms at BOTH readers. Witness: one row per arm — a
    WARM cache whose next read fails holds the reserve on the last-known
    policy; a COLD start that never resolved admits with no allocation,
    logged and counted, and does NOT read as "no reservations written"; and
    the same pair on `capPolicyFor`, whose nil arm hands every bot the
    UNLOWERED ceiling. The mutation collapses the arms back to one nil
    check, after which the cold-start row stops telling them apart.
11. **The reservation's identity is tier-qualified** (#2980; `pkg/budgetfloor`,
    `pkg/server`, `pkg/server/cloudpublisher`, the CLI,
    `pkg/server/platform_settings.go`, `docs/quotas-and-limits.md`) —
    numbered last so no other unit's number moves, and landing second,
    right after unit 1. Decision 11: the
    tier beside the bot on `Reservation` and `RepoQuota.ShareOfBot`, a match
    that compares both, an untiered stored reservation read as `platform`,
    `Policy.Validate`'s `seen`/`byBot` maps, its `ShareOfBot` lookup and
    `Policy.Bots()` keyed on the qualified identity, and a gate whose
    reservation arms judge the resolved identity on every launch surface —
    the twelve `launchSubject` literals that name a bot at `70b5b1972` go
    through ONE constructor fed by a `launchBot` or a run document, and a
    static sweep in the shape of `bot_resolver_sweep_test.go` refuses a
    literal outside it. The credential walk's window reserve takes the same
    identity from the `LaunchSpec`'s `BotSourceTier`, or from the prior run
    on a resume, threaded through `resolveAndSealCredentials` to
    `capPolicyFor` — the walk runs before the `RunMessage` that carries the
    tier is built. The operator doc moves with it in three places: the
    gate's "six limits, in this exact order" puts the launch rate ahead of
    concurrency; "The workload is a bot id" becomes a tier-qualified
    identity, an untiered reservation read as `platform`; and "What is not
    covered" says `POST /api/runs` is judged on its resolved bot, no longer
    on the request's `bot_id`. Witness: a team bot homonymous with a reserved platform
    bot, launched by its team through the REST launch, is NOT judged as the
    reserved workload, and the same name launched by a team holding no row
    of it IS — both directions, or a gate that refuses everything passes.
    Two mutations: match on the folded name only, which reddens the homonym
    row; judge the reservation arms on the request's label before the
    resolution, which reddens whichever row the tierless label misreads —
    both rows are owed. A policy holding a platform and a `team`
    reservation of one name validates, and each matches only its own
    launch; the mutation keys `Validate` on the folded name. A viewer, a
    client out of launch-rate tokens and a launch over its repository quota
    are refused with no resolver call; the mutation moves the whole gate
    behind the resolution. A cloud launch refused on its concurrency arm
    takes no snapshot and gets its rate token back; the mutation resolves
    through `resolveBotTiered` ahead of the reservation arms. The migration's
    witness is the existing surface tests that write an untiered reservation
    on the baked catalog's `probe` bot and drive it through the resolution
    (`TestHandleLaunchRun_CarriesTheRequestsBotToTheGate`,
    `TestProcessBoardCard_CarriesTheCardsBotToTheGate`): they run unchanged,
    and the mutation that reads an untiered reservation as naming no tier
    reddens them.

Units 4, 5 and 6 sequence after the branches that hold their files; unit 7
and unit 8 sequence against #2029, whose revised plan targets the same NATS
and chart surface. Unit 10 lands with unit 2 — the same gate's other arm,
split out because its witness is the resolver's, not the ledger's. Unit 11
lands ahead of unit 2 and unit 6: the per-bot count and the meter's workload
dimension key on the identity it qualifies, and either one written under a
bare name would carry the homonym into the ledger and the meter.
