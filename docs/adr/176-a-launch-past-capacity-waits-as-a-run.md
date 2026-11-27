# ADR-176 — A launch past capacity waits as a run: every surface creates it, a scheduler admits it through the slot ledger, and no event is refused, lost or replayed for lack of room

- **Status**: Proposed. The principle is the operator's decision of 2026-11-27 (on [#3845](https://github.com/SocialGouv/iterion/issues/3845)): "a launch past capacity waits as a run; the event that asked for it is never refused for lack of capacity, never lost, never replayed". The mechanism below is not decided, and nothing in it is built. A cross-family review of the plan comes before any code. The [open questions](#open-questions-for-the-operator) are the operator's.
- **Date**: 2026-11-27
- **Deciders**: jo (the principle); Claude (this draft, the inventory of the readers and surfaces, and the reading of the #3845 review)
- **Amends**: [ADR-175](175-the-org-is-the-envelope-of-budget-and-capacity.md) D4. D4 is the mechanism for the team and org caps ("the gate chooses, the publisher enforces through `CredSlotStore`"). This record makes it the mechanism for **every** concurrency axis, and replaces D4's refusal contract with a wait: the gate's advisory 429 and the `failed_resumable` park both give way to one state.
- **Builds on**:
  - [ADR-167](167-a-shared-gateway-bounds-its-alive-runs-through-the-run-admission-ledger.md): the ledger's subjects, the transit guard (C1–C3), a tree is one place, a deferred launch runs the current bot, and observe-then-enforce behind an activation guard. Its park (`GATEWAY_SLOT_WAIT`, T4b) and its wake (T5) are re-scoped onto this record's state and scheduler; neither is built.
  - [ADR-155](155-reserved-capacity-is-a-durable-allocation.md), as amended on 2026-11-24: the slot axes are `CredSlotStore` subjects, with no `capacityledger`.
- **Relates to**:
  - [ADR-076 (pipeline hard blockers)](076-pipeline-hard-blockers-and-waiting-deps.md) and the local pipeline scheduler: the in-process precedent.
  - [ADR-168](168-an-event-source-is-a-connector-package.md): the connector intake already postpones for capacity, for free.
  - [ADR-170](170-an-issue-trigger-is-a-re-armable-lane.md): issue claims.
- **Serves**: [#3845](https://github.com/SocialGouv/iterion/issues/3845). Coordinated with [#3748](https://github.com/SocialGouv/iterion/issues/3748) (ADR-175) and [#3254](https://github.com/SocialGouv/iterion/issues/3254) (ADR-167).

## Context

### The incident

On 2026-11-26, between 16:30 and 18:30Z, a burst of Renovate webhooks in team iterion ran 8–9 `dep_update_guard` sandboxes at once on rok01.

- **The disk.** md0 is a two-disk RAID1 mounted on `/data`. It holds the container layers of the three kubelets and the openebs-hostpath volumes, valkey among them. It ran up to 99 % busy, at 1.5–3.1k IOPS against a baseline of 300–900.
- **The overlap.** Image pulls (10 + 8 on one node, 8 on another) landed in the same window.
- **The effects.** NATS heartbeat refreshes timed out, so runs were cancelled as resumable. Valkey probes and containerd execs timed out too.
- **Proof.** The investigation correlates the load with the sandboxes and the pulls. It could not attribute the I/O per process, because it had no host access.
- **The stopgap.** It capped the whole runner pool at 6 (appsets `c648bcf`, per #3845). Nothing is lost: the extra messages wait on NATS. But every team is throttled for one bot's burst.

That queue has a cost of its own. `DefaultStreamMaxRetry`'s comment ([pkg/queue/nats/nats.go:68-78](../../pkg/queue/nats/nats.go)) names two things that consume the redelivery budget (8 × the 10 min AckWait):

- a message that waits unclaimed past AckWait because every runner is busy;
- a genuine crash.

So a deep runner queue is a slow road to the DLQ.

Nothing bounds one bot. A bot's concurrency is bounded per webhook subject (`overlap`), per schedule (`schedgate`), per team (`MaxConcurrentRuns`) and per key (`max_concurrent_runs`). `budgetfloor.Policy` holds per-bot floors, not ceilings.

### The first attempt and its review

Commit `45ee66d7c` (branch `feat/bot-concurrency-ceiling-3845`, not merged) put a per-bot ceiling into `gateConcurrency` as a 429. It also parked the immediate-lane deliveries that the ceiling refused, as durable webhook retry rows. An adversarial review ran probes on the memory store and on Mongo, each against both the new code and the base. It found:

- **The ceiling never binds when the bot is spelled differently.**
  - The gate counted `bot_id` exactly, and the policy folded case only.
  - Runs store the canonical manifest name, `dep-update-guard`.
  - `botregistry.NormalizeName` ([pkg/botregistry/registry.go:530](../../pkg/botregistry/registry.go)) lowercases and folds `_` and space to `-`.
  - Labelled `dep_update_guard`, four launches against a ceiling of 1 answered `[202 202 202 202]`.
  - Two ceilings, `dep_update_guard` and `dep-update-guard`, passed validation as distinct bots.
- **Refusing at admission loses work on the immediate webhook lane.** This loss exists today for the team cap and the launch rate too.
  - A first-admission denial writes a terminal `launch_error` row under a random key, with no retry target ([pkg/server/webhooks_common.go:1376-1384](../../pkg/server/webhooks_common.go)).
  - The code says so itself: "GitHub does not retry — Redeliver to start them" ([pkg/server/webhooks_retry.go:248](../../pkg/server/webhooks_retry.go)).
  - A manual Redeliver after room frees does launch in the base code. Unattended, the event is gone.
- **Parking the refused delivery for replay** creates a second deferral system, with its own semantics, beside the run's. Each of these failed on the new code and passed on the base:
  - **P1.** A refused merge-queue auto-heal was parked. The PR went back into the queue, and `stopHealRunForRequeuedPR` had no run to stop, so the sweep then launched `branch-improve-loop` on that queued PR. This is the #682 force-push shape.
  - **P5, P7.** Once the attempt budget was spent, the event was lost behind a `200 duplicate`. Both GitHub's Redeliver and the operator's retry endpoint refused it.
  - **P4.** A generic sender that honoured `Retry-After` with a fresh key launched twice.
  - **P11.** In a fan-out, the CODEOWNERS `review_requested` read the parked row as not live and reviewed the same head twice.
  - **P2, P3, P6.** Withdrawn or stale work was launched:
    - a superseded head, under the default `overlap: allow`;
    - a PR that had since taken a hold label;
    - an issue whose label had been removed and which had been closed.
  - **P8.** The webhook retry sweeper is one deployment-wide FIFO, 50 rows every 20 s. With 200 rows parked by one tenant, another tenant's due retry waited 4 ticks.

### What the code does with a launch that finds no room today

Every surface reaches the one gate, `gateLaunch` ([pkg/server/launch_gate.go:491](../../pkg/server/launch_gate.go)). A capacity denial there is a 429 (`concurrency_cap_exceeded` at :845-849 and :873-878, `launch_rate_limited` at :911-916), and each surface handles it in its own way:

| Surface | On a capacity 429 today |
|---|---|
| REST launch and resume, WebSocket answer | 429 to the caller; nothing stored ([pkg/server/runs_launch.go:572](../../pkg/server/runs_launch.go), :1238) |
| Webhook, immediate lane; `/command`; fork gesture | the forge gets the 429; a terminal row with no retry. **Lost** unless someone redelivers by hand |
| Webhook, after admission (`confirmReplacement`, fan-out siblings) | a retry row, 8 attempts, then lost ([pkg/server/webhooks_retry.go:22-38](../../pkg/server/webhooks_retry.go)) |
| Webhook, debounced lane | re-armed with backoff for about 45 min, then a terminal "parked review abandoned" row ([pkg/server/webhooks_debounce.go:418-497](../../pkg/server/webhooks_debounce.go)) |
| Cloud schedule tick | the tick is consumed: "ordinary failures retain … terminal verdict for this slot" ([pkg/cloudsched/ticker.go:171-182](../../pkg/cloudsched/ticker.go)) |
| Trigger effects outbox | an attempt is spent; dead-lettered after 5 ([pkg/trigger/effects_worker.go:94-108](../../pkg/trigger/effects_worker.go)) |
| Connector intake (ADR-168) | postponed **without** spending an attempt (`PostponeFor`, [pkg/connector/intake/worker.go:378-388](../../pkg/connector/intake/worker.go)). The only surface that waits for free |
| Board dispatcher | the card is put back with backoff; filed `blocked` with `launch_given_up` after 8 refusals ([pkg/server/boarddispatch.go:429-446](../../pkg/server/boarddispatch.go)) |
| Retry sweeper (resumes) | an attempt is spent; re-armed 15 min out ([pkg/server/retry_sweeper.go:420-436](../../pkg/server/retry_sweeper.go), :529-553) |

**Seven deferral systems.** Each has its own backoff, attempt budget, ordering and loss point. The review showed that an eighth, bolted onto the immediate lane, inherits all of their classes at once.

**One surface already waits as a run.** That is the per-key cap.

- The publisher reserves the key's slot in `store.CredSlotStore` ([pkg/store/credslot.go:62](../../pkg/store/credslot.go)) after sealing and before writing ([pkg/server/cloudpublisher/publisher.go:4427-4444](../../pkg/server/cloudpublisher/publisher.go)).
- On a refusal it parks the run `failed_resumable` / `CRED_SLOT_WAIT`, with its retry armed ([pkg/server/cloudpublisher/cred_slots.go:198-235](../../pkg/server/cloudpublisher/cred_slots.go)): "A park, not a refusal: a webhook delivery has nobody to click again" (:23-27).
- Its limits are written beside it. It "keeps no place in line" (:30-36), and a parked run is re-asked only through the retry sweeper. That sweeper takes **5 runs a minute, across the whole deployment**, ordered by `retry_after` with no tenant key ([pkg/server/retry_sweeper.go:34-39](../../pkg/server/retry_sweeper.go), [pkg/store/mongo/runs_retry.go:531-536](../../pkg/store/mongo/runs_retry.go)).
- `failed_resumable` is `IsTerminal`, `CanAutoResume` and `RunDeletable` ([pkg/store/run.go:114](../../pkg/store/run.go), [pkg/store/lifecycle.go:549](../../pkg/store/lifecycle.go), [pkg/store/run.go:72-79](../../pkg/store/run.go)). So a run that never started reads as failed: in stats, in the studio's diagnostic, and in `webhookRunLive`.

**The local pipeline scheduler** is the in-process precedent ([pkg/runview/pipeline_scheduler.go](../../pkg/runview/pipeline_scheduler.go), [pkg/runview/pipeline_queue.go:151-215](../../pkg/runview/pipeline_queue.go)).

- An over-limit root is put in an in-memory FIFO and persisted as a plain `queued` doc.
- A signal wakes the scheduler, and a 5 s backstop poll catches a missed signal.
- A restart rebuilds the FIFO, oldest first.
- Its comments carry three lessons:
  - Enforcement was pulled **down** to the queue, because a gate upstream "would be bypassed every single time" (:172-183).
  - The persistence is best-effort, and a failed persist is "LOST if the server restarts" (:32-35).
  - The restart rebuilds the launch spec lossily, losing overrides (:126-129).
- It is single-process by construction.

## Decision

### 1. One state: `waiting_capacity`, a run that exists and has not been handed to the runners

- **Every surface creates the run.** It is created durably, at once, in a new `RunStatus`, `waiting_capacity`, whenever an admission finds an axis without room. The surface answers as for any accepted launch: 202 with the run id; for a webhook, an `accepted` delivery row bound to the run.
- **What the run carries.** A `capacity_wait` record that names the axis that refused, as a subject (§4), with:
  - its label;
  - `held` and `max` at the refusal;
  - `since`, the run's place: set once, kept across every refusal of the same wait;
  - the count of admissions it has been refused.
- **A new status, not `queued` plus a reason.** `queued` means "submitted to the NATS queue but not yet picked up" ([pkg/store/run.go:100-105](../../pkg/store/run.go)). Its readers act on that meaning:
  - it holds a credential slot and counts against the launch limit (`HoldsCredentialSlot`, `CountsAgainstLaunchLimit`, [pkg/store/lifecycle.go:415](../../pkg/store/lifecycle.go), :555; `CountActiveRunsByTenant`'s literal `$in`, [pkg/store/mongo/runs.go:1047-1050](../../pkg/store/mongo/runs.go));
  - the orphan sweeper flips a stale `queued` run to `failed_resumable` / `PROCESS_ORPHANED` ([pkg/server/queue_sweeper.go:257-261](../../pkg/server/queue_sweeper.go), :326-329);
  - it may start execution (`CanStartExecution`, [pkg/store/lifecycle.go:484](../../pkg/store/lifecycle.go)).

  A waiting run read as `queued` would count against the cap it waits on, so a full cap never drains. It would also be adopted by the ledger as a holder, and orphaned by the sweeper. Every reader would need a second field to be right, including every replica of the previous release.
- **A new status, not `failed_resumable` plus a code.** That is today's park, and the review's P11 shows the reading `failed_resumable` gets: not live, therefore launch again.
- **Why a new value is safe on old code.** An unknown status is inert there:
  - not terminal, not counted, not a holder, not auto-resumable, not startable;
  - cancellable (`CanBeCancelled` is "not finally settled", [pkg/store/lifecycle.go:566](../../pkg/store/lifecycle.go));
  - not deletable (`RunDeletable` refuses a non-terminal run).

  An older replica therefore ignores a waiting run without harming it (§7).
- **Predicates.** `waiting_capacity` holds no slot, does not count against any launch limit, is not terminal, is live for supersede and stop purposes, is cancellable, is not deletable until cancelled, and cannot start execution. One predicate, `RunStatus.AwaitsCapacity()`, names it. `TestLifecyclePredicateMatrix` and `TestAllRunStatusesIsExhaustive` ([pkg/store/lifecycle_test.go](../../pkg/store/lifecycle_test.go)) gain its row.
- **The readers that change, all in the slice that writes the first waiting run.** The inventory is grep-based, and the slice re-runs it:
  - **Liveness and stopping**:
    - `webhookRunLive` ([pkg/server/webhooks_common.go:1117](../../pkg/server/webhooks_common.go));
    - `runIsStoppable` (:1977, which `stopHealRunForRequeuedPR` at :1826 and `stopRunsForDeadPR` read);
    - `forge_gate_relaunch.go`'s live test;
    - `lastRunForbidsFresh` and the hold on live runs in the dispatcher ([pkg/dispatcher/retry.go:158-165](../../pkg/dispatcher/retry.go), [pkg/dispatcher/loop.go:573](../../pkg/dispatcher/loop.go));
    - `stuckcard.go`;
    - the board's ticket admission ([pkg/server/pipeline_admission.go:211-221](../../pkg/server/pipeline_admission.go));
    - the schedule overlap gate (`schedgate.EvaluateOverlap`, not yet traced).

    Each must read a waiting run as live.
  - **Views**:
    - the studio's `RunStatus` union ([studio/src/api/runs/types.ts:10](../../studio/src/api/runs/types.ts)) and status classes;
    - the diagnostic ([pkg/runview/diagnostic.go:260-290](../../pkg/runview/diagnostic.go)): waiting is Active, with no action but Cancel;
    - the board's columns ([pkg/server/pipeline_boards_progress.go:307](../../pkg/server/pipeline_boards_progress.go));
    - `runs_stats.go`;
    - the global run discovery.
  - **Cancel**: the publisher's CAS over `CanBeCancelled` already covers the status. A waiting run is cancelled by a store write alone: no runner holds it and no message exists.
  - **Delete**: refused while waiting, as for `queued`.
  - **Unchanged by construction**, because they select `queued`, `running` or `failed_resumable` by literal:
    - the runner's admission and its epoch and execution fences ([pkg/queue/admission.go:55-98](../../pkg/queue/admission.go), [pkg/store/execution_authority.go:47-61](../../pkg/store/execution_authority.go));
    - the orphan sweeper;
    - the retry scans;
    - `CountActiveRunsByTenant`.

### 2. What stays a refusal, and what becomes a wait

**A wait** is something only time resolves: an axis that frees when other runs end.

- **Concurrency.** The per-bot ceiling, the team cap, the org cap (ADR-175 D1), the per-key cap, the gateway's bound (ADR-167), and the floor arithmetic on the team cap (§4).
- **The launch rate** (`LaunchRatePerMin`, team then org) also becomes a wait. It becomes a pace on **admissions**: the scheduler starts at most the bucket's rate per team, and a run past it waits on the axis `rate:team:<id>`, woken at the bucket's refill.
  - The 2026-11-26 incident is the case for it: the rate protects what a start costs (an image pull, a sandbox), not what a row costs.
  - A refused launch rate on the immediate lane loses the event today, exactly as the cap does.

**A refusal** is something that does not resolve by waiting, or that the operator must decide. It stays as it is today:

| Refusal | Why it stays |
|---|---|
| Monthly budget (402: run quota, cost cap, team ceiling of ADR-175 D2) | It resolves at the month's turn at best. A wait of weeks on an event is a stale launch, not a deferral |
| Policy and identity: no workspace, role rung, suspended or read-only team or org, unknown trust | Not time-bound; an operator decides |
| Fork funding and the fork-author bounds (ADR-162) | Fail-closed by their own decision, monthly |
| Repository quota (402) | Monthly |
| "All N concurrency slots are reserved for other workloads" ([pkg/server/launch_gate.go:840-850](../../pkg/server/launch_gate.go)) | No run ending frees it; the lever is the reservation. Its own detail already says so |
| Maintenance fence (503) | The fence exists to stop writes. The lanes that already hold across it (debounce, schedules) keep their hold; the immediate lane's loss under maintenance is unchanged here and named under Consequences |

**The refusals are taken twice.** The gate takes them at creation, as today. The scheduler takes the ones that can change during a wait again before each admission: suspension, the monthly cost cap, maintenance (which keeps the run waiting). A run refused while waiting ends `failed`, with the denial's token in its outcome. That is visible on the run and in the forge's check, never silent.

**The monthly run quota is charged at creation.** The run exists from then on. A waiting run cancelled or superseded before it started gives the charge back, through ADR-175 D2's release helper (nothing started, in the sense of `RunMayHaveStarted`).

### 3. The scheduler

- **One road to the runners.** Every launch and every resume goes through the publisher's admission.
  - It first tries the **fast path**: re-resolve, seal, then reserve every subject all-or-nothing, from the narrowest row to the widest (keys, `bot:`, `team:`, `org:`, `gw:`). It then persists `queued` and publishes, exactly as the per-key cap does today.
  - When a subject refuses, the run is written `waiting_capacity` instead, with its sealed launch, and nothing is published.
  - When a subject already has waiters, a fresh launch takes the waiting road even if a slot is momentarily free, so it does not overtake them. That read is a hint: a race here is an overtake, counted (§6), never a double admission.
- **The scheduler** turns waiting runs into queued ones. It runs on every replica under one lease, `capacity-scheduler` ([pkg/lease](../../pkg/lease/lease.go)). The lease is the package's own kind: "a load-reduction election, not a mutual-exclusion guarantee" (:1-11). Correctness does not rest on it:
  - **the slot** is the ledger's conditional write: of N admissions on K free places, exactly K win, whatever replica each ran on;
  - **the hand-over** is a CAS `waiting_capacity → queued`, conditioned on the run's `capacity_wait.since` and its status.

  Two schedulers that overlap during a stalled term therefore admit a run once. The loser's reservation is the same run's in-place re-admission (`ReserveCredSlot` never takes a second slot for a holder), and is released by its `At` (`ReleaseCredSlot`).
- **The admission of one waiting run** is the publisher's launch path run again on the stored run.
  - **Without a checkpoint**, it re-resolves on the current bot, revalidates the inputs and re-seals (ADR-167 §6). A wait of an hour must not dial with a token sealed an hour ago.
  - **With a checkpoint** (a resume that waited), it keeps the hash check and publishes as a resume.
  - Then it reserves the subjects, CASes the run to `queued` and publishes.
  - If the publish fails, the run is CASed back to `waiting_capacity` with its place kept, and its reservation is released.
  - If the replica dies between the CAS and the publish, a `queued` run with no checkpoint and no claim is left. The orphan sweeper returns it to `waiting_capacity` with its place, instead of to `failed_resumable`. If a message did go out, the runner drops it as a superseded attempt (`Superseded` compares `QueuedAt` with the message's publication instant, [pkg/queue/admission.go:56](../../pkg/queue/admission.go), :86-98).
- **Order.**
  - **Within a subject**, admission is FIFO on `(capacity_wait.since, run id)`. The run id breaks ties, so the order is a stable key, never the order of a scan.
  - **Across tenants**, a pass admits round-robin over teams, one admission per team per turn, each team in its own FIFO order. A burst of 200 runs from one team does not delay another team's first waiting run by more than one turn (the review's P8).
  - **Within a pass**, a subject that refuses is marked full. No later candidate that needs it is tried, so nobody overtakes on it. Candidates that do not need it continue, so one full bot does not block a team's other bots.
- **Starvation.** A run that waits on two subjects (a bot at its ceiling inside a full team) can be overtaken, on the subject that frees first, by a run that needs only that subject. v1 accepts this, counts each overtake (`iterion_capacity_overtakes_total{kind}`) and shows the run's age. A strict head-of-subject hold, the freed slot kept for the head for a bounded time, is built on its trigger: overtakes measured despite the round-robin. This is the same posture as ADR-167's strict FIFO.
- **Wakes.**
  - The fast path's refusal and every terminal transition of a run publish a wake on the event bus. That bus is `NATSBus` in cloud and `InProcBus` locally, the same code; it is "at-least-once and lossy" ([pkg/eventbus/bus.go](../../pkg/eventbus/bus.go)).
  - A backstop poll runs every `ITERION_CAPACITY_SCHEDULER_INTERVAL`, 10 s by default. It catches a lost wake, a slot freed by the ledger's lazy eviction (a holder is evicted at the next admission that finds it stale, `CredSlotHolderStale`, [pkg/store/credslot.go:216](../../pkg/store/credslot.go)), a run killed with no event, and a restart.
  - A pass with no waiting run is one indexed count.
- **A policy that changes during a wait.** The scheduler reads every limit through its resolver, at each pass:
  - **A cap raised** admits the waiters at the next pass.
  - **A cap lowered** below its holders admits nobody until enough holders end. This is the ledger's existing rule for a holder's re-admission ([pkg/store/credslot.go:62-80](../../pkg/store/credslot.go)).
  - **A cap removed** (0 or −1, per ADR-175 D0) drops the subject. Its waiters are admitted at the next pass, in order.
  - **A ceiling removed on a bot** frees that bot's waiters the same way.
- **A wait that never ends.** A run's retry policy `max_wait`, when it sets one, bounds the whole wait (ADR-167 §8). The run then ends `failed` with code `CAPACITY_WAIT_EXPIRED`, naming the subject: a visible end, not a lost event. Without `max_wait`, a run waits until it is admitted or cancelled (open question 2).
- **Restart and replica loss.** The waiting set is the run documents, so there is no in-memory queue to rebuild. A new lease holder resumes at its first pass. A run whose admission was cut short is either still `waiting_capacity` (the CAS never happened) or a `queued` orphan handled as above.
- **The runner side is unchanged.** A message is published only once its slots are held. NATS's redelivery budget (MaxDeliver 8, [pkg/queue/nats/nats.go:68-78](../../pkg/queue/nats/nats.go)) is therefore never spent by capacity, and the runner pool cap goes back to bounding the physical fleet, not one bot's burst. The runner keeps its own claim and ADR-167's transit guard:
  - C1: a run in redelivery keeps its place;
  - C2: the runner re-asserts every stamped subject before a direct resume;
  - C3: the launch re-asserts after its write.

  C2's refusal ("one missing → no resume, a free retry is armed") becomes: the run goes back to `waiting_capacity`, with its place.

### 4. The ledger: typed subjects on `CredSlotStore`

- **One ledger.** `store.CredSlotStore` stays the one ledger, on both twins: [pkg/store/mongo/cred_slots.go](../../pkg/store/mongo/cred_slots.go) and [pkg/store/credslot_fs.go](../../pkg/store/credslot_fs.go). Its rows gain typed subject kinds beside the key fingerprints (hex or `account:`, unchanged: renaming live rows would only cost a re-adoption) and `gw:<host>` (ADR-167):
  - `bot:<name>`, where `<name>` is `botregistry.NormalizeName(run.BotID)`;
  - `team:<id>` (ADR-175 D4);
  - `org:<id>` (ADR-175 D1, D4).

  One helper composes them from the run at stamp time. ADR-175 D4 already names it for `team:` and `org:`: it is shared by the launch stamp and both `SetRunCredStamp` twins, and it never reads the subjects from the sealed bundle.
- **The bot identity is folded at both ends.** The ceiling's key is folded at **validation**, so two spellings of one bot are one entry and a duplicate is refused. The run's id is folded at **stamp**. The review's F0 is the reason: one spelling at each end is the defect, not the spelling.
- **An axis is a seam, not an `if`.** Each axis implements one interface (philosophy §2, the Nth-variant test):
  - the subjects a run needs;
  - the `max` it may use under each subject;
  - the label and the refusal's words;
  - whether it is a slot (held until the run stops spending) or a pace (`rate:`, a token taken at admission).

  The bot ceiling, the team, the org, the key, the gateway and the rate are six implementations. A seventh, a per-pool or per-node axis, is a new implementation.
- **Budget floors stay arithmetic on the team subject.** A floor is not a row of its own. `ReserveCredSlot` takes `max` per call, so the team row is asked with `cap − OtherReservedSlots(bot)` for a bot no reservation names, and with `cap` for the reserved bot. That is today's arithmetic ([pkg/server/launch_gate.go:838-852](../../pkg/server/launch_gate.go)), now judged against an atomic row instead of a count. ADR-175 D1 then moves it to `org:<id>`.
- **Exactness.** `CountActiveRunsByTenant` is a count read and then compared, with no reservation: two replicas both admit into the last slot. The ledger's conditional write closes that window. The gate's count is no longer a bound; it can stay a cheap hint for the fast path.
- **Releases** are what releases a slot today. A run releases when it stops spending under the holder predicate: it goes terminal, is cancelled, is deleted (its document gone past `CredSlotGrace`) or is lost (the orphan sweeper settles it), with C1's redelivery hold in between. The holder is then evicted at the next admission that meets it. The only release a caller performs is the undo of an admission that provably handed nothing on (`ReleaseCredSlot` with its `At`).
- **Runs that predate the ledger.** Adoption counts the alive runs a row does not name, but only through their stamp. A run launched before its subject existed is invisible, so each subject is observed first and then enforced behind ADR-167 §7's activation guard. The guard refuses `enforce` while alive runs whose stamp lacks the subject exist; `accept_unstamped: true` is the audited escape.
- **Cloud and local, the same code.**
  - The Mongo twin and the filesystem twin pass the same conformance rows.
  - The scheduler takes its lease from `lease.MongoStore` or `lease.MemoryStore`, and its wakes from `NATSBus` or `InProcBus`.
  - The local pipeline scheduler's in-memory FIFO retires onto this state and the filesystem ledger in the last slice (§8), with its best-effort persist and its lossy spec rebuild.

  The four questions of philosophy §3, for the new durable state:
  1. **Three replicas**: the lease owns the work, and the ledger and the run CAS own the correctness.
  2. **A pod dies mid-admission**: the run is either still waiting or a recoverable orphan.
  3. **A rolling deploy**: nothing waits in memory.
  4. **The cloud twin**: in each slice.

### 5. The webhook lanes, on a live run instead of a stored event

Every immediate-lane delivery now answers 202 with a run. The webhook-side machinery then acts on a **run** it already knows how to stop, not on an event it must replay.

- **Overlap and supersede.** `supersededRunIDs` already selects the runs to replace by delivery rows that point at a run, `StatusLaunched` ([pkg/server/webhooks_common.go:972-1005](../../pkg/server/webhooks_common.go)). A waiting run is such a run, and cancelling it is a store write.
- **Collapse of waiting runs.** A forge run that has not started is superseded by a newer run of the same bot on the same subject, whatever the overlap policy. Nothing started, so the newer head is the only one worth running. This closes P2: today's `overlap: allow` keeps both, because by then both have started.
- **Debounce** is unchanged: it holds the event before any launch. When it fires, it gets a run, and its `deferRetryWait` for capacity is no longer reached.
- **Holds.** A hold label applied after the launch cancels the subject's waiting runs. It does not cancel its started ones, which is today's rule for a running run. This closes P3.
- **Closed or withdrawn work.** A dead PR already stops every bot on the subject (`stopRunsForDeadPR`, :1897). An unlabelled or closed issue cancels the waiting runs of its claim (ADR-170's sequence). This closes P6.
- **Auto-heal.** `stopHealRunForRequeuedPR` stops the heal run by its key's delivery row. The run exists from the first delivery, and `runIsStoppable` reads waiting as stoppable, so the re-queue cancels it. This closes P1: there is no stored event left to replay.
- **Fork gestures** go through the same tail, pinned to one SHA (ADR-162). They get a waiting run, and the per-author bounds stay refusals (§2).
- **Fan-out.** `webhookRunLive` reads a waiting sibling as live, so the CODEOWNERS re-request collapses onto it instead of salting a second key. This closes P11.
- **Redelivery.** The delivery row is `accepted` with a run id from the first attempt, so a Redeliver is answered with that run, whatever it is doing. There is no attempt budget to exhaust and no sender retry to double. This closes P4, P5 and P7.
- **Fairness.** The scheduler's round-robin over teams replaces the shared sweepers for capacity. This closes P8.
- **What still could happen.** A waiting run reads its forge state when it is admitted, not when the event was sent. A fact the lane does not observe, such as a label set by hand in the UI without a webhook subscription, is not seen. That is a reading of the forge, the same as for a run that started a second after the event.

### 6. Visibility

- **The run.**
  - `capacity_wait` is on the run's API and in the studio: "waiting for capacity — bot `dep-update-guard`, 3/3, 4th in line, 6 min". The chip is not red, and the only action is Cancel.
  - The pull request gets ADR-167 T6's pause notice, once per wait: no time and no "quota".
  - The text names this team's own position and the counts. It names **no other team's run**.
  - The event stream gets `run_capacity_wait` (subject kind, label, held, max, position) and `run_capacity_admitted` (waited for).
- **The position.** The position is per subject, computed on `(since, id)`. The publisher's queue position counts every `queued` run deployment-wide ([pkg/server/cloudpublisher/publisher.go:5617-5620](../../pkg/server/cloudpublisher/publisher.go)) and stays the runner-queue reading.
- **Metrics.** Labelled by subject **kind**, never by tenant:
  - `iterion_capacity_waiting_runs{kind}`;
  - `iterion_capacity_wait_seconds{kind}`, a histogram;
  - `iterion_capacity_admissions_total{kind,path=fast|scheduled}`;
  - `iterion_capacity_overtakes_total{kind}`;
  - `iterion_capacity_scheduler_pass_seconds`;
  - `iterion_capacity_wait_expired_total{kind}`.

  The starter alert pack gains a rule on the oldest wait and one on a scheduler without a lease holder.
- **The operator.** `GET /api/admin/capacity/subjects[/{subject}]` lists each row's holders and waiters, with every limit and its source. It is exposed as `iterion remote admin capacity ls|show`, and in the OpenAPI. A team admin reads the same view filtered to their team. ADR-167's `gateway_alive_runs` route becomes one of its kinds.

### 7. Mixed versions during a rollout

- **An older server**:
  - never writes `waiting_capacity`, and answers its own launches with today's 429;
  - ignores a waiting run in every count and sweep, by §1's inert reading;
  - may cancel one, which is correct.

  Its `SaveRun` keeps the unknown fields (ADR-167, Consequences). It could only harm a waiting run by re-saving a stale copy of the same document, which the scheduler's CAS on `since` and the status detects.
- **An older runner** never sees a waiting run: none is published.
- **The switch.** `capacity_scheduler`, a `platformcfg` switch: `off`, then `observe`, then `enforce`, per subject kind. Its `PUT` to `enforce` is refused until every server replica runs the release, read from the deployed digests (ADR-167 §7). Until then the old gate behaviour holds.
- **The rollback.** Set `observe` or `off`. The waiting runs are admitted unbounded at the next pass. None is lost, and none is left waiting with nobody to admit it.

### 8. Rollout: what builds when

| Slice | Built | Trigger |
|---|---|---|
| **S0 — measure**, no code | now | Per-bot alive runs over a week; the starts per minute against md0's busy time; the overtakes and P8-type delays the sweepers produce today |
| **S1 — the state** | now | The operator's decision of 2026-11-27. No writer yet: the status, `AwaitsCapacity`, `capacity_wait`, both twins, the predicate matrix, the readers of §1, the studio union, and the inert reading proven on the previous release's binary |
| **S2 — the scheduler and `bot:`** | now, after S1 | #3845: the per-bot ceiling is the first subject, carrying over the policy shape, the CLI and the canonical fold of `45ee66d7c`, but not its gate check or its parking. The publisher's fast path and the waiting write, the scheduler with its lease, wakes and pass, the §5 lane changes, and §6's run-level visibility. Then `dep-update-guard` at a measured ceiling (about 3) in `observe`, then `enforce`; once it holds, the runner pool goes back to 8–10 and md0 is watched |
| **S3 — `team:` moves from the gate to the ledger** | after S2 and #3254 T4a | ADR-175 D4's own trigger: the first team with a concurrency cap above 0 in production. The gate's 429 retires for the team cap, and the floors move with it (§4) |
| **S4 — `gw:`** | on ADR-167's activation | ADR-167 T4b and T5 are built on this state: no `GATEWAY_SLOT_WAIT` park is ever written |
| **S5 — keys** | after S3 has run a week in `enforce` | `CRED_SLOT_WAIT` parks migrate to the state. The retry sweeper stops carrying capacity waits, and the free-deferral code goes |
| **S6 — `org:`** | ADR-175 D1's trigger | |
| **S7 — `rate:`** | after S3, on a measured start burst | The launch rate becomes an admission pace; its 429 retires |
| **S8 — local** | after S5 | The pipeline scheduler retires onto the state and the filesystem ledger |

S2 enforces `bot:` before #3254's transit guard (T4a) is on main. A run in its redelivery window (`failed_resumable`, `redelivery_pending`) then holds no `bot:` place, so the ceiling can be exceeded by the runs in transit during a drain. For a host-protection ceiling that leak is bounded and named, and the counter `iterion_capacity_unheld_redeliveries_total` measures it. S3 waits for T4a because a tenant cap is a promise.

Each slice owes:

- `task check`; CI adds `race`, `vendor-check` and `mongo-conformance`;
- every test seen red under its named mutation;
- Mongo protocol tests run on a real replica set;
- a review loop with its trailers;
- its documentation in [quotas-and-limits.md](../quotas-and-limits.md) and [webhooks.md](../webhooks.md), in the same change.

## Consequences

- **One deferral system for capacity instead of seven.** The capacity arms of these go:
  - the webhook retry rows;
  - the debounce `deferRetryWait`;
  - the board's launch-refusal backoff;
  - the trigger outbox's attempts;
  - the schedule's lost tick;
  - the connector's postpone;
  - the retry sweeper's re-arm and `CRED_SLOT_WAIT`.

  Their non-capacity arms stay: maintenance, monthly refusals and errors.
- **ADR-175 D4 changes contract.** Its "429 `concurrency_cap_exceeded` with `Retry-After`" and its "parked `failed_resumable` under `TENANT_SLOT_WAIT`" are replaced by a waiting run. Its order of rows, its single stamping helper, its tree rule and its dependency on #3254 stand. The token `concurrency_cap_exceeded` stays only for the "all slots reserved" refusal, until that refusal gets a token of its own (`concurrency_slots_reserved`).
- **The caller sees a run where it saw a 429.** A CLI or API caller that relied on the 429 to back off now gets a run, which it can cancel. That is a behaviour change, written into the API reference.
- **New durable state, each with its twin:**
  - the status;
  - `capacity_wait` on runs;
  - the `bot:`, `team:`, `org:` and `rate:` rows (pace rows hold no holders);
  - the `capacity_scheduler` platform switch;
  - one lease;
  - an index on `(status, capacity_wait.since)`.
- **Admission costs more for a waiting run.** It is re-resolved and re-sealed at admission. Its fast-path resolution is spent for nothing when it waits. That is the price of not dialling with stale credentials.
- **Contention.** `org:<id>` and a hot `bot:` row are written by every admission under them. Under a burst, the bounded rounds (`credSlotReserveRounds = 32`, [pkg/store/mongo/cred_slots.go:161](../../pkg/store/mongo/cred_slots.go)) can be exhausted. On the fast path the store error makes the run wait; in the scheduler it leaves the run waiting. Either way it fails closed without a refusal, which is gentler than ADR-175 D4's refusal on a ledger error.
- **A wait without `max_wait` is unbounded.** A run waiting behind a bot whose ceiling the operator never raises waits until it is cancelled. The alert on the oldest wait is the operator's signal.
- **Residual loss, out of scope.** The immediate lane still loses an event refused by the maintenance fence, or by a store outage before the run is written. Neither is a capacity refusal.

## Alternatives considered

- **Refuse at the gate and park each surface's refused event (`45ee66d7c`).** Rejected by its review: P1–P8 and P11, each red on the new code and green on the base. A refusal is the loss, and a parked event replayed later is a second launch system that every stop, supersede, hold and dedupe of the lanes would have to learn. The review proved that, each time, they had not learnt it.
- **Defer at the runner, with a delayed nak** (`handleAdmissionMismatch`, [pkg/runner/loop_nats.go:195-240](../../pkg/runner/loop_nats.go)). A nak counts against MaxDeliver: the handler compares `NumDelivered` with `MaxDeliver` and parks on the DLQ once the budget is spent (:198-203, :225-240). The constants' comment says a bare nak "can burn the whole redelivery budget in seconds" ([pkg/queue/nats/nats.go:97-107](../../pkg/queue/nats/nats.go)).
  - Each redelivery takes a runner's fetch and claim, and the runner would need to read every axis's configuration.
  - ADR-167 rejected the place taken at the runner's claim for that reason.
  - Whether a message under a delayed nak counts against `MaxAckPending` (256 by default) during its delay was not verified here.
  - Rejected: a wait measured in deliveries is a loss with a delay.
- **Keep only the global runner pool cap.** It loses nothing (NATS holds the messages) and costs no code. But it throttles every team for one bot, and its waiting messages burn the redelivery budget past AckWait (above). It also says nothing about a key, a gateway, a team or an org. It stays the physical bound of the fleet, not an allocation.
- **`queued` with a reason field.** Rejected in §1: every reader of `queued`, and every replica of the previous release, would count, adopt or orphan a waiting run.
- **`failed_resumable` with a park code, as `CRED_SLOT_WAIT` does.** Rejected in §1:
  - terminal and deletable;
  - read as not live by the lanes;
  - re-asked at 5 runs a minute deployment-wide.
- **An in-memory FIFO per replica, as the local pipeline scheduler has.** It is not cloud-native: a replica's FIFO dies with it, and its persistence is best-effort by its own comment.
- **A per-subject CAS scheduler with no lease, every replica scanning.** Correct, since the ledger and the run CAS hold, but N replicas scan N times and fight over the same rows. The lease is a load reduction on a design that is already correct.

## Open questions for the operator

1. **The scope of a bot ceiling.**
   - Deployment-wide on the canonical name, as #3845 designed it, protects the host. It would also cap a team's own bot that bears a catalogue bot's name.
   - Per (team, bot) protects nothing shared.
   - Proposed: deployment-wide on the name, with a per-team share later on its trigger, like ADR-167's per-team share of a gateway.
2. **An unbounded wait by default?** Proposed: yes, `max_wait` bounds it where a bot sets one. The alternative is a platform default horizon, with the run ending `CAPACITY_WAIT_EXPIRED`.
3. **The launch rate as a pace (S7).** It changes what `LaunchRatePerMin` means, from creations to starts. Should creation keep a separate abuse bound? Proposed: a team-level `MaxWaitingRuns` (ADR-175 D0 convention, unbounded by default), whose refusal (429 `capacity_queue_full`) is the one capacity-flavoured refusal, explicit and counted.
4. **An interactive opt-out.** Proposed: none in v1; Cancel is the opt-out. The alternative is a `wait: false` on the REST launch, so a CLI caller can ask for the old 429.
5. **Collapse of waiting runs by a newer head under `overlap: allow` (§5).** It changes `allow`'s meaning for runs that have not started. Is that wanted for every bot, or opt-in per bot?
6. **S2 before T4a.** Accept the bounded redelivery leak on `bot:` (§8), or wait for #3254 T4a?
