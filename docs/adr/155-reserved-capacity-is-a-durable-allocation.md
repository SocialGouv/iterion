# ADR-155: reserved capacity is a durable allocation — one ledger, one atomic acquire, a settle and a sweep

Status: accepted (design — nothing below is built; the units are tracked under #950)
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
  `hasCredentials()`, which live resolution states as
  `len(bundle.APIKeys) == 0 && len(bundle.OAuthCredentials) == 0`. A
  floor-HELD credential is never sealed, so the bundle *is* empty at the
  pool tier and an unreserved bot the reserve held out does reach the pool —
  deliberate, and the restore step refuses to hand the held credential back.
  What stays inexpressible is the ticket's own ask: Revi on a pool of
  forfaits *while Revi's team holds a healthy credential*.
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
second half stays open by decision, in the consequences.

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
   exactly what `pkg/credusage`, `pkg/orgusage`, `pkg/lease` and
   `pkg/usagecap` already are. So the launch gate and the credential walk
   import it without inverting the graph, and this decision does not
   contradict the Mongo twin it asks for below.
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
   atomic: `AllowRun`'s own words. It is deliberately NOT a *conditional*
   (filtered) update — `{outstanding: {$lte: cap - requested}}` yields no
   count on a miss and cannot tell a full subject from an absent one, which
   is why the two precedents filter on `_id` alone. `credpool.Ledger` and
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
   tightened mid-run still bites. Both ends have an unstamped arm and both
   fall back to the DEPLOYMENT policy — the UNLOWERED ceiling
   (`capPolicyFor` lowers `effective` only, over a `deployment` that stays
   `base`; `usageGuardFor` reads `r.cfg.UsageCapSource`) — so for that run
   the reserve is not held: the lax direction, and neither arm may be
   silent. A message with no stamp can only be one queued across a deploy of
   the single binary that publishes (`cmd/iterion/server.go`), a
   drain-length window: logged and counted, nothing else changes. A STAMPED
   message on a runner that predates the reader is the long window — a
   separately rolled image — and it is decision 10's case: the reserve
   counts as unheld on the runner side until the fleet's floor clears the
   release that reads the stamp. Both `claw` and `claude_code` lanes read
   the stamp, or it is typed-refused for the one that does not (backend
   parity).
6. **The pool becomes an arbitrated capacity tier.** A reservation may be
   marked pool-eligible (default OFF), and then `credentialTierPool` stays
   active for that workload even though it holds its own credential. One
   rule, in `walkCredentialPlan`, so live resolution and the preview cannot
   disagree. `budgetFloorWarnings` gains its third advisory shape: a
   reservation marked pool-eligible on a deployment where no enabled pool
   admits its tenant holds nothing.
7. **The meter carries the workload.** `credusage.Key` and `Spend` gain
   `BotID`, `docID` leaves an empty one out exactly as it does for `RepoID`,
   and a repo's share of a reservation resolves against **that
   reservation's** consumption. Enforcement of the new dimension is armed
   only on a fleet **verified** to write it, by decision 10's predicate. A
   fleet that is not cleared keeps the pre-dimension reading — the
   repository's **total** — never the bot-attributed subset, because the
   subset is the lax direction: money leaks and nothing alerts (finding 6).
8. **Policy unavailable is not policy absent.** The resolver keeps its
   process-local TTL cache, and the gate stops reading an unreadable policy
   as "no reservations" — but the two arms differ, and only one of them has
   a last-known value to hold a reserve on. A WARM cache already serves
   one: the resolver warns "settings read failed — serving last-known
   values" and returns the cached record, so the reserve is held on it and
   the conservative concurrency rule stands in while the ledger is
   unreachable. A COLD start that never resolved has NO policy: `cached`
   stays nil, `ready` stays false, and `Get` returns a bare `*T`, so today
   `budgetFloorPolicy` maps nil to the zero policy and cannot tell "nothing
   written" from "never read". Unit 10 exposes that distinction —
   `Resolver.ready` becomes readable — and takes the **admit** direction:
   refusing every launch on a deployment that may have configured no
   reserve at all is the worse failure (philosophy 1), so the launch is
   admitted with no allocation and the fact is logged AND counted under its
   own reason detail. Fail-open stays the doctrine for a degraded store
   read — `gateLaunch`'s doc comment owns it — made observable instead of
   silent.
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
    at or above the release that ships the field. `s.engineFloor` does NOT
    answer it — with no observer wired, on an `ObservedRunnerBuilds` error
    and on an EMPTY observation it returns this server's OWN build as the
    floor, which on a current server reads as cleared; the evidence of those
    arms is in its `sources`, not in its build — and its one existing
    consumer PASSES an unorderable floor with a warning
    (`requiresFloorGap`'s `EngineUnknown` arm), the polarity this gate
    inverts. So the unit owes a readiness accessor beside `engineFloor`,
    with each unverified arm witnessed. An unverified fleet is a WARNING and
    never a refusal (philosophy 1), and the conservative reading it falls
    back to is logged and counted, so a rollout is observable rather than
    silent.

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
- **A reservation's identity is still a bare bot NAME** (finding 2's second
  half): `Reservation.BotID` carries no tier qualifier, `sameID` folds the
  name only, and bot resolution keeps team-over-platform precedence by
  design — so a tenant that registers a team bot named `review-pr` is judged
  by the gate as the reserved workload. The gate's first call also takes the
  RAW request label, before the resolution that would refuse an unknown one.
  This record **does not close it**: it is an accepted bound with a
  greppable escape hatch (the operator re-points the reservation), and
  tier-qualifying the identity is its own unit under #950's net.
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
   as intended behaviour. The other direction — a stamped message on a
   runner too old to read it — cannot be exercised from inside the new
   binary; it is decision 10's reading, owed by unit 6's arms, and this unit
   only states that the reserve counts as unheld until the floor clears.
5. **The pool as an arbitrated tier** (`pkg/budgetfloor`,
   `pkg/server/cloudpublisher`, the CLI, `pkg/server/platform_settings.go`,
   `docs/credential-pool.md`). Witness: a run holding a healthy credential
   whose reservation is pool-eligible receives a grant and the preview
   reports the same tier; the mutation reverts the plan arm and the existing
   preview-matches-the-walk test reddens. A second test pins the default:
   no opt-in, no pool.
6. **The meter carries the workload** (`pkg/credusage`, `pkg/runner`,
   `pkg/server`, `pkg/budgetfloor`, the studio's typed client). Witness: a
   repo capped at N% of `review-pr`'s reserve is NOT refused by
   `feature-dev`'s spend; the conformance suite extended for both twins; and
   the finding-6 case — an unverified fleet keeps the repository's total, so
   a runner that writes no bot cannot make the share read unconsumed. The
   mutation drops the engine-floor gate and the share reads as free — and
   one row per unverified arm of decision 10 (no observer wired, an
   `ObservedRunnerBuilds` error, an EMPTY observation), each of which must
   keep the repository's total and emit the warning, and each of which reads
   as CLEARED if the gate asks `engineFloor` for a build instead of asking
   the readiness accessor.
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
    `pkg/server`) — decision 8: the readiness accessor, and the gate's two
    arms. Witness: one row per arm — a WARM cache whose next read fails
    holds the reserve on the last-known policy; a COLD start that never
    resolved admits with no allocation, logged and counted, and does NOT
    read as "no reservations written". The mutation collapses the arms back
    to one nil check, after which the cold-start row stops telling them
    apart.

Units 4, 5 and 6 sequence after the branches that hold their files; unit 7
and unit 8 sequence against #2029, whose revised plan targets the same NATS
and chart surface. Unit 10 lands with unit 2 — the same gate's other arm,
split out because its witness is the resolver's, not the ledger's.
