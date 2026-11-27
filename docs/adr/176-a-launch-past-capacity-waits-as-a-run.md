# ADR-176 — A launch past capacity waits as a run: every surface creates it, a scheduler admits it through the slot ledger, and no event is refused, lost or replayed for lack of room

- **Status**: Proposed. The principle is the operator's decision of 2026-11-27 (on [#3845](https://github.com/SocialGouv/iterion/issues/3845)): "a launch past capacity waits as a run; the event that asked for it is never refused for lack of capacity, never lost, never replayed". The mechanism below is not decided, and nothing in it is built. A cross-family review of the plan (GLM-5.4, 25 findings) is folded in; its [disposition](#adversarial-review-disposition-glm-54-via-claude-code-max--25-findings) closes this record. The operator answered its ten questions on 2026-11-27: see [Decisions taken](#decisions-taken-operator-2026-11-27). The record stays Proposed until it is accepted as a whole.
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

- **Every surface creates the run.** It is created durably, at once, in a new `RunStatus`, `waiting_capacity`, whenever an admission finds an axis without room. The surface answers as for any accepted launch: 202 with the run id.
  - **For a webhook**, the delivery row goes to `launched` with the run id at creation, whether the run waits or not.
  - That is what the tail writes as soon as a launch returns an id ([pkg/server/webhooks_common.go:1606-1611](../../pkg/server/webhooks_common.go)), and what `supersededRunIDs` (:998-1000), the debounce lane and the retry lane read.
  - `accepted` keeps its meaning: a launch in progress, read as live for 10 minutes only (`acceptedLaunchWindow`, :1044-1049, :1112).
- **What the run carries.** A `capacity_wait` record with:
  - **the axes that refused**, one or several, each as a subject (§4) with its label, and its `held` and `max` at the refusal;
  - **`since`**, the run's place in line. It is set once and kept across every refusal of the same wait. A replacement inherits it from the run it replaces (§5);
  - **the count of refused admissions**;
  - **`admission_pending`**, the instant of an admission in flight (§3). It is empty while the run only waits.
- **A new status, not `queued` plus a reason.** `queued` means "submitted to the NATS queue but not yet picked up" ([pkg/store/run.go:100-105](../../pkg/store/run.go)), and its readers act on that meaning:
  - it holds a credential slot and counts against the launch limit (`HoldsCredentialSlot`, `CountsAgainstLaunchLimit`, [pkg/store/lifecycle.go:415](../../pkg/store/lifecycle.go), :555; `CountActiveRunsByTenant`'s literal `$in`, [pkg/store/mongo/runs.go:1047-1050](../../pkg/store/mongo/runs.go));
  - the orphan sweeper flips a stale `queued` run to `failed_resumable` / `PROCESS_ORPHANED` ([pkg/server/queue_sweeper.go:257-261](../../pkg/server/queue_sweeper.go), :326-329);
  - it may start execution (`CanStartExecution`, [pkg/store/lifecycle.go:484](../../pkg/store/lifecycle.go)).

  A waiting run read as `queued` would count against the cap it waits on, so a full cap would never drain. It would also be adopted by the ledger as a holder, and orphaned by the sweeper. Every reader would need a second field to be right, including every replica of the previous release.
- **A new status, not `failed_resumable` plus a code.** That is today's park, and the review's P11 shows the reading `failed_resumable` gets: not live, therefore launch again.
- **What an unknown status does on old code, today.** The previous release has no `waiting_capacity`, and it reads one as follows:
  - **Inert:** it is not terminal, not counted, not a holder, not auto-resumable and not deletable (`RunDeletable` refuses a run that is not terminal).
  - **Not cancellable.** The publisher's cancel guard reads `CanBeCancelled()`, which is true for an unknown status. But its CAS expects a set built from the explicit list `store.AllRunStatuses` ([pkg/server/cloudpublisher/publisher.go:4694-4738](../../pkg/server/cloudpublisher/publisher.go), [pkg/store/lifecycle.go:394-398](../../pkg/store/lifecycle.go)). It therefore matches nothing and answers "cancel raced … — retry". Every stop of §5 then fails on an old binary.
  - **Started by an old runner.** An old runner's `Admit` lets an unknown status proceed ([pkg/queue/admission.go:50-80](../../pkg/queue/admission.go), [pkg/runner/loop.go:435-438](../../pkg/runner/loop.go)). The start is then refused further down (`CanStartExecution`, [pkg/store/run_invocation_reserved.go:44](../../pkg/store/run_invocation_reserved.go)). The message is nak'd up to MaxDeliver and parked on the DLQ. `BuildRunOutcome` has no branch for the status, so it reports `run.failed` ([pkg/trigger/runoutcome.go:96-97](../../pkg/trigger/runoutcome.go), :134-143).
  - **Rejected by the team cutover.** `TeamCutoverStatusAlive` returns an error on any unknown status ([pkg/store/lifecycle.go:501-511](../../pkg/store/lifecycle.go)).

  S1 therefore ships the cancel CAS and these readers **before any writer**, and proves them on the release that precedes S2 (§7, §8).
- **Predicates.** One predicate names the state: `RunStatus.AwaitsCapacity()`. `TestLifecyclePredicateMatrix` and `TestAllRunStatusesIsExhaustive` ([pkg/store/lifecycle_test.go](../../pkg/store/lifecycle_test.go)) each gain a row for it. A waiting run:
  - holds no slot;
  - counts against no launch limit;
  - is not terminal;
  - is live for supersede and stop purposes;
  - is cancellable;
  - carries its pause pointer, as a waiting resume (decision Q1(b));
  - is not deletable until cancelled;
  - cannot start execution.
- **The reader matrix.** S1 covers every predicate and every reader below, with a row each. S1 re-runs the grep, and a test walks every value of `AllRunStatuses` through each predicate, so that a predicate that errors on a status fails the test.

  | Reader | Change |
  |---|---|
  | The publisher's cancel CAS ([pkg/server/cloudpublisher/publisher.go:4708-4713](../../pkg/server/cloudpublisher/publisher.go)) | expects `CanBeCancelled` as a predicate (a `$nin` of the settled statuses), not a list built from `AllRunStatuses`. Ships in S1 |
  | `TeamCutoverStatusAlive` ([pkg/store/lifecycle.go:501-511](../../pkg/store/lifecycle.go)) | waiting is alive, and an unknown status reads as alive instead of erroring. Ships in S1 |
  | The runner's `Admit` and precondition ([pkg/queue/admission.go:55-80](../../pkg/queue/admission.go), [pkg/runner/loop.go:435-438](../../pkg/runner/loop.go)) | a `waiting_capacity` run is dropped (`DropAwaitingCapacity`): acked, with no nak, no DLQ and no event, and counted as a defect (`iterion_runner_waiting_delivery_total`). A message for it should not exist. Ships in S1 |
  | `BuildRunOutcome` ([pkg/trigger/runoutcome.go:134-143](../../pkg/trigger/runoutcome.go)) | waiting has no outcome; no event is emitted. Ships in S1 |
  | `SaveRun` pause-pointer scrub ([pkg/store/mongo/runs.go:444-450](../../pkg/store/mongo/runs.go)), `CarriesPausePointer`, `CanReclaimResumePauseHandoff` ([pkg/store/lifecycle.go:430-444](../../pkg/store/lifecycle.go)) | `waiting_capacity` carries the pause pointer and may reclaim the handoff (decision Q1(b)), so the scrub keeps a waiting resume's pointer |
  | `WatchDeliverable` ([pkg/store/run.go:147](../../pkg/store/run.go)), `IsPaused` (:126), `IsQueued` ([pkg/store/lifecycle.go:466](../../pkg/store/lifecycle.go)) | false; written into the matrix |
  | `webhookRunLive` ([pkg/server/webhooks_common.go:1117](../../pkg/server/webhooks_common.go)), `runIsStoppable` (:1977, read by `stopHealRunForRequeuedPR` at :1826 and by `stopRunsForDeadPR`), `forge_gate_relaunch.go`'s live test | live |
  | Dispatcher: `lastRunForbidsFresh`, the hold on live runs, `stuckcard.go` ([pkg/dispatcher/retry.go:158-165](../../pkg/dispatcher/retry.go), [pkg/dispatcher/loop.go:573](../../pkg/dispatcher/loop.go)); the board's ticket admission ([pkg/server/pipeline_admission.go:211-221](../../pkg/server/pipeline_admission.go)) | live |
  | The schedule overlap gate ([pkg/schedgate/live_runs.go:67-69](../../pkg/schedgate/live_runs.go)) | unchanged: anything not terminal already counts as live |
  | Views: the studio's `RunStatus` union ([studio/src/api/runs/types.ts:10](../../studio/src/api/runs/types.ts)) and status classes; the diagnostic ([pkg/runview/diagnostic.go:260-290](../../pkg/runview/diagnostic.go)); the board's columns ([pkg/server/pipeline_boards_progress.go:307](../../pkg/server/pipeline_boards_progress.go)); `runs_stats.go`; global discovery | waiting is Active, and its only action is Cancel |
  | Delete (`RunDeletable`) | refused while waiting, as for `queued` |
  | The epoch and execution fences ([pkg/store/execution_authority.go:47-61](../../pkg/store/execution_authority.go)), the retry scans, `CountActiveRunsByTenant`, the ledger's holder predicate | unchanged: they select `queued`, `running` or `failed_resumable` by literal |
  | The orphan sweeper's `queued` pass ([pkg/server/queue_sweeper.go:257-278](../../pkg/server/queue_sweeper.go)) | gains a pass for a `queued` run whose `admission_pending` has outlived its grace (§3). That pass is not skipped under backlog |

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

**The refusals are taken twice.** The gate takes them at creation, as today. The scheduler takes the ones that can change during a wait again before each admission: suspension, the monthly cost cap, maintenance (which keeps the run waiting). A run refused while waiting ends `failed_resumable` with the denial's token in its outcome (decision Q4(a)). The end is visible on the run and in the forge's check, never silent, and an operator can still resume the run.

**The monthly run quota is charged at creation**, because the run exists from then on.

- **No refund primitive exists for a run that waits.** Giving the charge back is today `launchAdmission.rollback`, a closure held in memory ([pkg/server/launch_gate.go:427](../../pkg/server/launch_gate.go)). It is called only when a launch returns an error that proves nothing started (`RunMayHaveStarted`, [pkg/runview/run_persisted.go:42](../../pkg/runview/run_persisted.go)). Nothing can give back the charge of a run that waited and was then cancelled, refused or expired.
- **What S2 builds.** A durable link between a charge and its run: the charged subjects, recorded on the run, and a release by run id that is idempotent.
- **Which ends refund** (decision Q5(a)): every end of a run that never started gives the charge back. That covers cancelled, superseded, collapsed, expired, and refused during the wait.
  - **"Never started" has a precise meaning:** a waiting run with no checkpoint and no `resume_spec`.
  - **A waiting resume has started.** It already ran an attempt, so its charge is not refunded.
- **A bound on creation, from S2.** A per-team `MaxWaitingRuns` follows the ADR-175 D0 convention. Its proposed default is 200, to be measured in S0. Past it, a launch is refused with 429 `capacity_queue_full`, counted: the one capacity-flavoured refusal, explicit. A storm on a capped bot therefore cannot turn a capacity wait into an exhausted monthly quota.

### 3. The scheduler

- **The fast path, for a launch.** The publisher reserves every subject before it writes the run, exactly as the per-key cap does today. The order is all-or-nothing, from the narrowest row to the widest: keys, `bot:`, `team:`, `org:`, `gw:`.
  - **All granted:** it persists the run `queued` and publishes. The reservation made before the document exists is covered by `CredSlotGrace` ([pkg/store/credslot.go:124-130](../../pkg/store/credslot.go)).
  - **One refused:** the grants already made are released, and the run is written `waiting_capacity`, naming every refused axis. Nothing is published.
  - **The subject already has waiters:** a fresh launch takes the waiting road, even if a slot is momentarily free, so it does not overtake them. That read is a hint, so a race here is an overtake, counted (§6), never a double admission.
- **Nothing sealed rests on a waiting run.** A launch seals its credentials into the queue message, not the run document (`SecretsRef`, `BundleDEK`, [pkg/server/cloudpublisher/publisher.go:4486-4487](../../pkg/server/cloudpublisher/publisher.go)). A waiting run without a checkpoint stores no sealed material and no DEK. Its admission re-resolves and re-seals (ADR-167 §6).
  - **What the run must store** is the non-secret part of the launch request that is not on the run today: the webhook's `key_overrides` and `secret_overrides` references, the repository ref, and the launch flags.
  - S2 names these fields with a test that launches from a waiting run and compares the result with a direct launch.
- **Resumes wait too** (decision Q1(b)). A resume past capacity becomes `waiting_capacity` like a launch, and it never takes the `failed_resumable` / `CRED_SLOT_WAIT` park.
  - **Why a spec must be stored.** A resume carries request fields that are never persisted on the run ([pkg/runview/service.go:563-638](../../pkg/runview/service.go)). `SubmitResume` consumes them, for example `CheckResumeAdmitted(prior, spec.Admitted)` (publisher.go:4815) and the answers check (:4853).
  - **The record.** The waiting resume therefore stores a durable `resume_spec` on the run. It holds every field `SubmitResume` consumes that cannot be re-derived from the run:

    | Field | Content | Care |
    |---|---|---|
    | `Force`, `AcceptScratchLoss` | the operator's consents (booleans) | none: no secret. Each is an audited decision, kept as given |
    | `ExpectOrigin` | the origin a checked surface admitted | none: an identifier |
    | `Admitted` (`GateAdmission{Trust, ForkAuthorSubject}`, [pkg/runview/gate_admission.go:16-19](../../pkg/runview/gate_admission.go)) | the gate's verdict at creation | no secret. The scheduler crosses the gate again (§2) and compares; a trust that changed during the wait refuses the admission, never widens it |
    | `AutoMemory`, `AmbientContext`, `LoopBudgetGuard`, `Supervisors` | override enums (`""`, `on`, `off`, `none`) that the service says are "not persisted on the run" | none: no secret |
    | `Codemode`, `CodemodeBounds` | an explicit re-statement only; the launch's choice is already on the run | none: no secret. Absent means the launch's choice, as today |
    | `Answers` | the answers a human typed into a gate | **user content**. It has the sensitivity of the `human_answers_recorded` record it becomes after the resume. It is stored in the run's collection under the same tenant filter, never logged or copied into an event, and it counts toward the document's size bound (a large answer set is refused at the resume's request, not at admission) |
    | `HostInputs` | host-attested projections merged into the paused node's output | **never persisted.** The field keeps its contract: "ephemeral … kept out of `human_answers_recorded` and published artifacts … not a second durable authority" (service.go). A resume that carries `HostInputs` and finds no capacity therefore does not wait. It keeps today's `failed_resumable` / `CRED_SLOT_WAIT` park, the documented exception. `iterion_capacity_hostinputs_parks_total` counts these parks so their frequency is visible |

  - **Fields left out.**
    - `ExpectedStatus` and `ReceiptID`, a mission's CAS guard and correlation id, are kept so that a mission's guard still decides.
    - `FilePath`, `Source`, `BundleDir`, `BundleEntry` and `BotBundle` are re-resolved at admission, as a cloud resume already re-resolves its bundle. With a checkpoint, the workflow-hash check holds unless `Force`.
    - `Timeout` is kept as given.
  - **Cleared at admission.** The spec is cleared by the write that settles the admission, after the publish succeeded: from then on the queue message carries it. A refused admission, a failed publish or a crash keeps it with the run. A cancel, an expiry or a deletion clears it.
  - **The resume keeps its place.** A waiting resume inherits a `since` equal to the run's first `QueuedAt` (its attempt's instant), not the moment it waited. A run that already started is therefore ahead of every fresh launch on the same subject.
  - **What the flip to `waiting_capacity` preserves:** the checkpoint and its episode; `continuation_state`; the attempt marker that `RevertQueuedFlip` restores (publisher.go:4964-4972); `RetryState`; and the pause pointer. `waiting_capacity` therefore joins `CarriesPausePointer` and `CanReclaimResumePauseHandoff`, so the `SaveRun` scrub ([pkg/store/mongo/runs.go:444-450](../../pkg/store/mongo/runs.go)) keeps the pointer.
  - **Retiring the park.** The park for capacity, `CRED_SLOT_WAIT`, is retired as a writer **for the key kind once it is in `enforce`** (§7), from S2, the slice that ships waiting resumes. From then on no publisher path writes it for an enforced kind, neither a launch's key refusal (§8) nor a resume's.
  - **While the key kind is `off`**, on an S2 binary as on today's, a key refusal keeps today's publisher key admission and its `CRED_SLOT_WAIT` park. That is the proven path the rollback falls back to (§7).
  - **S5 removes the park writer for good.** There is one exception: a resume that carries `HostInputs` (table above) keeps the park, and it is counted. The retry sweeper keeps re-asking the parks written before S2 until none remains. Its capacity code goes in S5.
  - **The rollback stays proven.** S2 proves, on its own binary:
    - `off` admits every waiting resume, with its spec, at the drain pace;
    - the drain to zero (§7) includes waiting resumes.

    Below S2, a binary cannot read a `resume_spec`, so the procedure of §7 holds unchanged: drain to zero before the roll.
- **The scheduler** turns waiting runs into queued ones.
  - **Ownership.** It runs on every replica under one lease, `capacity-scheduler` ([pkg/lease](../../pkg/lease/lease.go)). The lease is the package's own kind: "a load-reduction election, not a mutual-exclusion guarantee" (:1-11). Correctness does not rest on it.
  - **Order of an admission, on the resume model.** The ledger evicts any holder whose run does not spend ([pkg/store/credslot.go:141-146](../../pkg/store/credslot.go), :216-217; [pkg/store/mongo/cred_slots.go:135-150](../../pkg/store/mongo/cred_slots.go)). A reservation taken while the run is still `waiting_capacity` would therefore be evicted by the next refused admission on the same row. So the order is:
    1. **CAS** `waiting_capacity → queued`, conditioned on the status and on `capacity_wait.since`. The run is marked `CredAdmissionPending`, with `admission_pending` set to the flip's instant, and `QueuedAt` is that instant. A queued run spends, so it is not evicted. A run that awaits its own admission is not adopted by another admission (`AwaitsCredSlotAdmission`, [pkg/store/credslot.go:197-200](../../pkg/store/credslot.go)).
    2. **Re-check** the refusals that can change during a wait (§2).
    3. **Re-resolve and re-seal** (no checkpoint, ADR-167 §6).
    4. **Reserve** every subject, all-or-nothing.
    5. **Granted:** `SettleCredSlotAdmission`, then publish.
    6. **Refused, or a store error:** CAS back `queued → waiting_capacity` on the flip's instant, with `since` and the axes updated. The grants already made are released by their `At`.
  - **Two schedulers that overlap.** The one that loses the CAS reserves nothing and releases nothing. Today's hazard cannot occur: a loser's release, by the `At` of its in-place re-admission, removing the winner's live entry ([pkg/store/mongo/cred_slots.go:163-192](../../pkg/store/mongo/cred_slots.go), :294-309). A conformance row on both twins pins that interleaving.
  - **A publish that fails** after the settle releases the grants and CASes the run back, as in step 6.
  - **A replica that dies mid-admission** leaves a `queued` run whose `admission_pending` is older than `CredSlotGrace`. The orphan sweeper's new pass returns it to `waiting_capacity` with its `since`, whatever the backlog: the marker, not the backlog, tells it apart from a published run. If a message did go out, the runner drops it in either of two ways:
    - **superseded:** `Superseded` compares `QueuedAt` with the message's publication instant ([pkg/queue/admission.go:56](../../pkg/queue/admission.go), :86-98);
    - **while the run is back in waiting:** the S1 `DropAwaitingCapacity` branch.
- **Order.**
  - **Within a subject**, admission is FIFO on `(capacity_wait.since, run id)`. The run id breaks ties, so the order is a stable key, never the order of a scan.
  - **Across tenants**, a pass admits round-robin over teams: one admission per team per turn, each team in its own FIFO order. A burst of 200 runs from one team therefore delays another team's first waiting run by at most one turn (the review's P8).
  - **Within a pass**, a refusal marks the pair (subject, effective `max`) full, not the subject alone. The `max` a candidate asks for depends on its bot (§4's floors). A later candidate that needs the same subject at the same or a lower `max` is skipped, so nobody overtakes on it. A candidate of a reserved bot, which asks with a higher `max`, is still tried. A candidate that needs neither continues, so one full bot does not block a team's other bots.
- **Drain pace, from S2.** No path admits the whole waiting set at once.
  - **The bound.** A pass admits at most `ITERION_CAPACITY_ADMIT_PER_PASS` runs (default 3), and at most `ITERION_CAPACITY_ADMIT_PER_MIN` per minute deployment-wide (default 6).
  - **Where it applies.** The same bound covers the paths an operator takes under incident: an activation, a cap raised or removed, a lease that changes hands, and a rollback to `observe` or `off`.
  - **Its meaning.** It is the start pace that the 2026-11-26 incident lacked. For comparison, today's park release is `retrySweepBatch = 5` per 60 s pass ([pkg/server/retry_sweeper.go:34-39](../../pkg/server/retry_sweeper.go)).
  - **What follows.** S7's per-team `rate:` axis later refines it.
- **Starvation.** A run that waits on two subjects (a bot at its ceiling inside a full team) can be overtaken by a run that needs only the subject that frees first.
  - v1 accepts this. Each overtake is counted (`iterion_capacity_overtakes_total{kind}`), and the run's age is shown.
  - A strict head-of-subject hold, keeping a freed slot for the head for a bounded time, is built on its trigger: overtakes measured despite the round-robin. This is ADR-167's posture on strict FIFO.
- **Wakes.**
  - The fast path's refusal and every terminal transition of a run publish a wake on the event bus. That bus is `NATSBus` in cloud and `InProcBus` locally, with the same code. It is "at-least-once and lossy" ([pkg/eventbus/bus.go](../../pkg/eventbus/bus.go)).
  - A backstop poll runs every `ITERION_CAPACITY_SCHEDULER_INTERVAL`, 10 s by default. It catches a lost wake, a slot freed by the ledger's lazy eviction (`CredSlotHolderStale`, [pkg/store/credslot.go:216](../../pkg/store/credslot.go)), a run killed with no event, and a restart.
  - A pass with no waiting run costs one indexed count.
- **A policy that changes during a wait.** The scheduler reads every limit through its resolver at each pass, within the drain pace:
  - **a cap raised** admits waiters;
  - **a cap lowered** below its holders admits nobody until enough holders end, which is the ledger's existing rule ([pkg/store/credslot.go:62-80](../../pkg/store/credslot.go));
  - **a cap or ceiling removed** (0 or −1, per ADR-175 D0) drops the subject, and its waiters are admitted in order.
- **A refusal or an expiry during a wait** (decision Q4(a)). A distinct `capacity_wait_deadline`, unbounded by default and set per bot or platform, ends the wait. The run then ends `failed_resumable` with `CAPACITY_WAIT_EXPIRED`, or with the token of the refusal that ended it, so an operator can resume it.
  - **The same write disarms `RetryState.retry_after`.** The retry sweeper selects every `failed_resumable` run whose `retry_after` is due (`ListRunsDueForRetry`, [pkg/store/mongo/runs_retry.go:524-536](../../pkg/store/mongo/runs_retry.go)). Disarmed, the run is never auto-resumed back into the wait, so the deadline actually ends it, and a run refused for the cost cap spends no further attempt.
  - **Only an operator's resume restarts it.**
  - **A conformance row on both twins pins this.** A run ended by expiry, and one ended by a refusal during the wait, are each absent from `ListRunsDueForRetry` at any later instant.
  - `max_wait` does not bound it. In `retrypolicy` that field is a clamp on how far ahead one retry is scheduled, 8 days by default ([pkg/retrypolicy/policy.go:40-45](../../pkg/retrypolicy/policy.go), :72-75, :91-92), not a deadline on a first launch.
- **The ledger is unavailable** (decision Q3(a)). A ledger error, or rounds exhausted under contention, admits nothing.
  - On the fast path, the run is created `waiting_capacity` on the axis `ledger_error`. In the scheduler, the run stays waiting with that axis.
  - While the error persists, the fast path skips the resolution and the seal, so it does no wasted work.
  - No pull-request notice is posted for `ledger_error`.
  - `iterion_capacity_ledger_errors_total{kind}` counts it, and a dedicated alert fires on it.
  - The runner's C2 is not concerned before S5: on the runner side, a ledger error stays ADR-167's "deferred nak" (§3, runner side).
- **Restart and replica loss.** The waiting set is the run documents, so there is no in-memory queue to rebuild. A new lease holder resumes at its first pass. A run whose admission was cut short is either still `waiting_capacity` or a `queued` run with a stale `admission_pending`, handled above.
- **The runner side.** Only a run whose slots are held is published, so NATS's redelivery budget (MaxDeliver 8, [pkg/queue/nats/nats.go:68-78](../../pkg/queue/nats/nats.go)) is never spent on capacity. The runner pool cap goes back to bounding the physical fleet. The runner itself changes in two ways only:
  - the S1 drop branch for a `waiting_capacity` delivery;
  - nothing else in S2–S4: **the runner never writes `waiting_capacity`**.

  ADR-167's transit guard (C1–C3, #3254 T4a) is not on main. `HoldsCapacity(` and `ReassertCredSlot(` match nothing in `pkg/`. When T4a lands, its C2 refusal keeps its own form, "a free retry is armed". Making it write the wait instead, with the runner's release added to the rollout guard, is decided at S5, not before.

### 4. The ledger: typed subjects on `CredSlotStore`

- **One ledger.** `store.CredSlotStore` stays the one ledger, on both twins: [pkg/store/mongo/cred_slots.go](../../pkg/store/mongo/cred_slots.go) and [pkg/store/credslot_fs.go](../../pkg/store/credslot_fs.go). Its rows gain typed subject kinds beside the key fingerprints (hex or `account:`, unchanged: renaming live rows would only cost a re-adoption) and `gw:<host>` (ADR-167):
  - `bot:<name>`, where `<name>` is `botregistry.NormalizeName(run.BotID)`;
  - `team:<id>` (ADR-175 D4);
  - `org:<id>` (ADR-175 D1, D4).

  A `bot:` count is aggregated across every team, so its `held` and `max` show the load of all teams, never another team's runs (§6).

  One helper composes them from the run at stamp time. ADR-175 D4 already names it for `team:` and `org:`: it is shared by the launch stamp and both `SetRunCredStamp` twins, and it never reads the subjects from the sealed bundle.
- **The bot identity is folded at both ends.** The ceiling's key is folded at **validation**, so two spellings of one bot are one entry and a duplicate is refused. The run's id is folded at **stamp**. The review's F0 is the reason: one spelling at each end is the defect, not the spelling.
- **An axis is a seam, not an `if`.** Each axis implements one interface (philosophy §2, the Nth-variant test):
  - the subjects a run needs;
  - the `max` it may use under each subject;
  - the label and the refusal's words;
  - whether it is a slot (held until the run stops spending) or a pace (`rate:`, a token taken at admission).

  The bot ceiling, the team, the org, the key, the gateway and the rate are six implementations. A seventh, a per-pool or per-node axis, is a new implementation.
- **Budget floors stay arithmetic on the team subject.** A floor is not a row of its own. `ReserveCredSlot` takes `max` per call, so the team row is asked with `cap − OtherReservedSlots(bot)` for a bot no reservation names, and with `cap` for the reserved bot. That is today's arithmetic ([pkg/server/launch_gate.go:838-852](../../pkg/server/launch_gate.go)), now judged against an atomic row instead of a count. ADR-175 D1 then moves it to `org:<id>`.

  The `max` is read from the reservation policy before the conditional write. The write is atomic, but the read of the policy is not: a reservation changed between the two applies at the next admission. A conformance row pins two things: a reserved bot is admitted into the room between `cap − reserve` and `cap` after an unreserved bot was refused, and §3's (subject, `max`) rule lets it through within the same pass.
- **Exactness.** `CountActiveRunsByTenant` is a count read and then compared, with no reservation: two replicas both admit into the last slot. The ledger's conditional write closes that window. The gate's count is no longer a bound; it can stay a cheap hint for the fast path.
- **Releases** are what releases a slot today. A run releases when it stops spending under the holder predicate: it goes terminal, is cancelled, is deleted (its document gone past `CredSlotGrace`) or is lost (the orphan sweeper settles it), with C1's redelivery hold in between. The holder is then evicted at the next admission that meets it. The only release a caller performs is the undo of an admission that provably handed nothing on (`ReleaseCredSlot` with its `At`).
- **A tree is one place.** The bot ceiling counts trees, not sandboxes (ADR-167 §5, ADR-175 D4). A bot invoked as a sub-bot of another root holds no `bot:` place. No catalogue bot calls `dep-update-guard` as a sub-bot: `grep -rn 'dep.update.guard' bots/` finds only its own bundle and a comment in `feed-watch`. A sub-bot ceiling is a separate axis, built on its trigger.
- **Runs that predate the ledger.** Adoption counts the alive runs a row does not name, but only through their stamp, so a run launched before its subject existed is invisible. Each subject is therefore observed first and enforced afterwards, behind an activation guard. The guard refuses `enforce` while alive runs whose stamp lacks the subject exist, and `accept_unstamped: true` is its audited escape. **Neither exists today.** ADR-167 designs them for its T3 and T4b, and `grep -rn 'accept_unstamped\|would_park' pkg/` matches nothing outside tests. S2 builds both, for `bot:` (§8).
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

- **Overlap and supersede.** `supersededRunIDs` selects the runs to replace by delivery rows in `StatusLaunched` that point at a run ([pkg/server/webhooks_common.go:972-1005](../../pkg/server/webhooks_common.go)). A waiting run's row is `launched` (§1), so it is selected, and cancelling it is a store write.
  - **The timing.** Today a replacement cancels its predecessors after its own admission and before its launch (:1539, then :1545-1558): "Only an admitted replacement may cancel" (:1007). With no capacity refusal at the gate, a replacement that waits would destroy its predecessor before it holds a place. **A replacement therefore cancels its predecessor at its own admission** (decision Q6(a)): in the step that publishes it, never at its creation. Its predecessor keeps running while it waits.
  - **The place.** A replacement inherits the `since` of the run it replaces, so a busy subject under sustained pushes is not sent to the back of the line at each push.
- **Collapse of waiting runs.** A waiting forge run that has never started is superseded by a newer run of the same bot on the same subject, whatever the overlap policy, and the newer run inherits its `since`. "Never started" means no checkpoint and no `resume_spec`. Nothing started, so the newer head is the only one worth running. This closes P2: today's `overlap: allow` keeps both, because by then both have started. It applies to every forge-subject bot, whatever its overlap policy (decision Q9(a)).
  - **How it combines with Q6.** The collapse cancels a predecessor that is waiting and has never started, at the newer run's creation.
  - **Q6's "at admission" rule governs every predecessor that has started.** That includes a **waiting resume** (one with a checkpoint or a `resume_spec`): it is cancelled only at the replacement's admission, so its progress and its human `Answers` are never destroyed by a replacement that has not run.
  - **Why the collapse is free.** The collapsed predecessor has started nothing, and the newer run inherits its `since`, so nothing is destroyed and no place is lost.
  - **If the newer run never starts.** Under Q4 a newer run that expires or is refused ends `failed_resumable`, so the newest head stays resumable and nothing is lost. This reading of Q6 × Q9 is confirmed (2026-11-27).
- **Debounce** is unchanged: it holds the event before any launch. When it fires, it gets a run, and its `deferRetryWait` for capacity is no longer reached.
- **Holds.** A hold label applied after the launch cancels the subject's waiting runs. It does not cancel its started ones, which is today's rule for a running run. This closes P3.
- **Closed or withdrawn work.** A dead PR already stops every bot on the subject (`stopRunsForDeadPR`, :1897). An unlabelled or closed issue cancels the waiting runs of its claim (ADR-170's sequence). This closes P6.
- **Auto-heal.** `stopHealRunForRequeuedPR` stops the heal run by its key's delivery row. The run exists from the first delivery, and `runIsStoppable` reads waiting as stoppable, so the re-queue cancels it. This closes P1: there is no stored event left to replay.
- **Fork gestures** go through the same tail, pinned to one SHA (ADR-162). They get a waiting run, and the per-author bounds stay refusals (§2).
- **Fan-out.** `webhookRunLive` reads a waiting sibling as live, so the CODEOWNERS re-request collapses onto it instead of salting a second key. This closes P11.
- **Redelivery.** The delivery row is `launched` with a run id from the first attempt, so a Redeliver is answered with that run, whatever it is doing. There is no attempt budget to exhaust and no sender retry to double. This closes P4, P5 and P7.
- **Fairness.** The scheduler's round-robin over teams replaces the shared sweepers for capacity. This closes P8.
- **What still could happen.** A waiting run reads its forge state when it is admitted, not when the event was sent. A fact the lane does not observe, such as a label set by hand in the UI without a webhook subscription, is not seen. That is a reading of the forge, the same as for a run that started a second after the event.

### 6. Visibility

- **The run.**
  - `capacity_wait` is on the run's API and in the studio: "waiting for capacity — bot `dep-update-guard`, 3/3, 4th in line, 6 min". The chip is not red, and the only action is Cancel.
  - The pull request gets ADR-167 T6's pause notice, once per wait: no time and no "quota". No notice is posted for a `ledger_error` wait (decision Q3).
  - The text names this team's own position and the counts. On a subject shared across teams (`bot:`, `gw:`), the counts are an aggregate. It names **no other team's run**.
  - The event stream gets `run_capacity_wait` (subject kind, label, held, max, position) and `run_capacity_admitted` (waited for).
- **The position.** The position is per subject, computed on `(since, id)`. The publisher's queue position counts every `queued` run deployment-wide ([pkg/server/cloudpublisher/publisher.go:5617-5620](../../pkg/server/cloudpublisher/publisher.go)) and stays the runner-queue reading.
- **Metrics.** Labelled by subject **kind**, never by tenant:
  - `iterion_capacity_waiting_runs{kind}`;
  - `iterion_capacity_wait_seconds{kind}`, a histogram;
  - `iterion_capacity_admissions_total{kind,path=fast|scheduled}`;
  - `iterion_capacity_overtakes_total{kind}`;
  - `iterion_capacity_scheduler_pass_seconds`;
  - `iterion_capacity_wait_expired_total{kind}`;
  - `iterion_capacity_would_wait_total{kind}`, counted in `observe` when an admission would have made the run wait (the twin of ADR-167's `iterion_gateway_would_park_total`);
  - `iterion_capacity_ledger_errors_total{kind}`.

  The starter alert pack gains a rule on the oldest wait and one on a scheduler without a lease holder.
- **The operator.** `GET /api/admin/capacity/subjects[/{subject}]` lists each row's holders and waiters, with every limit and its source. It is exposed as `iterion remote admin capacity ls|show`, and in the OpenAPI. A team admin reads the same view filtered to their team. ADR-167's `gateway_alive_runs` route becomes one of its kinds.

### 7. Mixed versions during a rollout, and the way back

- **The release floor is S1.**
  - **Readers before writers.** S1's readers (§1: the cancel CAS, `TeamCutoverStatusAlive`, the runner's drop, `BuildRunOutcome`) are deployed on every server replica **and every runner** one release before any writer exists.
  - **What that floor makes safe.** A release at or above S1 can cancel, delete after a cancel, count and ignore a waiting run. A runner at or above S1 drops a stray message for one without spending the DLQ.
  - **Proof.** It is shown on the S1 binary itself, before S2 is merged: a waiting run written by a test fixture is cancelled, read by the cutover, and dropped by the runner.
- **A server older than S1 must never meet a waiting run.**
  - It cannot cancel one: its CAS expects a list without the status (§1).
  - It would 409 every stop of §5.
  - Its runner would nak a stray message to the DLQ.
- **A server between S1 and S2**:
  - never writes `waiting_capacity`, and answers its own launches with today's 429;
  - ignores a waiting run in every count and sweep;
  - cancels one correctly.

  Its `SaveRun` keeps the unknown fields (ADR-167, Consequences). It could harm a waiting run only by re-saving a stale copy of the same document, which the scheduler's CAS on `since` and on the status detects.
- **The switch.** `capacity_scheduler` is a `platformcfg` family with one mode per subject kind:
  - **`off`**: nothing of this record, on any binary, S2's included. A kind's limit is enforced as it is today, by the gate's 429 or, for the key kind, by the publisher's key admission and its `CRED_SLOT_WAIT` park. A `bot:` ceiling in `off` bounds nothing.
  - **The key kind has no `observe`**, because keys are already enforced today: an `observe` mode would weaken them. Its modes are `off` and `enforce`.
  - **Q1(b) and Q3 apply to the kinds in `observe` or `enforce`.** With keys `off`, a resume, or an already-waiting run, refused by a key is parked as today. A ledger error on a key is refused as today. That keeps the rollback on the proven path.
  - **`observe`**: the kind is stamped and counted. Nothing is reserved and no run waits on it, and `iterion_capacity_would_wait_total{kind}` counts each admission that would have waited.
  - **`enforce`**: the kind's subjects are reserved, and the run waits.
- **The guard on `enforce`.** A `PUT` that sets `enforce` is refused (409) in two cases:
  - **The fleet is below S2.** S2 builds a fleet version guard: each server replica and each runner records its release in a heartbeat, and the guard reads them. **This does not exist today.** The "deployed digests" check of ADR-167 §7 is a design, not code: `grep -rni 'deployed.digest' pkg/` matches nothing. If S2 cannot build it, the `PUT` takes an audited attestation instead, `fleet_release: <tag>`, and the operator's checklist names the commands that prove it.
  - **Alive runs are unstamped.** Some alive runs carry no stamp for the kind (§4). `accept_unstamped: true` is the audited escape.
- **The way back, named.**
  1. Set the kind to `observe`, or `off`. The waiting runs are then admitted at the drain pace (§3), with no bound left on them. None is lost.
     - For the key kind, set `off`. Its key refusals go back to today's `CRED_SLOT_WAIT` park, which the retry sweeper drains, and the S2 binary keeps that writer for exactly this case.
  2. To roll the **binary** back below S2, the procedure is first:
     - `off` for every kind;
     - the waiting set drained to zero: `iterion remote admin capacity ls --waiting` prints 0;
     - then the roll, down to a release no older than S1.
  3. A roll below S1 is refused by the checklist while any `waiting_capacity` document exists.

### 8. Rollout: what builds when

| Slice | Built | Trigger |
|---|---|---|
| **S0 — measure**, no code | now | Per-bot alive runs over a week; the starts per minute against md0's busy time; the overtakes and P8-type delays the sweepers produce today |
| **S1 — the readers, no writer** | now | The operator's decision of 2026-11-27. Delivers the status and `AwaitsCapacity`; `capacity_wait`, on both twins; the full reader matrix of §1; the cancel CAS as a predicate; `TeamCutoverStatusAlive`; the runner's `DropAwaitingCapacity`; `BuildRunOutcome`; the studio union. It is deployed to every server and runner, and proven on that binary, one release before S2 |
| **S2 — the scheduler and `bot:`** | after S1 is the fleet's floor | #3845. The per-bot ceiling is the first subject. S2 carries over the policy shape, the CLI and the canonical fold of `45ee66d7c`, but not its gate check or its parking. **Prerequisites built in S2:** (a) the admission order of §3, with `admission_pending` and the orphan pass that runs under backlog; (b) the persisted launch request (§3); (c) the durable charge–run release (§2); (d) the drain pace; (e) `observe` and its would-wait counter; (f) the activation guard, `accept_unstamped`, and the fleet version guard (§7); (g) webhook rows `launched` at creation, and §5's changes; (h) §6's run-level visibility. (i) waiting resumes with `resume_spec`, and the retirement of the `CRED_SLOT_WAIT` writer for the key kind in `enforce` (kept while that kind is `off`, §7), except for a resume carrying `HostInputs`, which keeps the park under `iterion_capacity_hostinputs_parks_total` (§3); (j) `MaxWaitingRuns` and `capacity_queue_full` (§2); (k) the `ledger_error` axis and its alert (§3); (l) `capacity_wait_deadline` (§3). Then `dep-update-guard` runs at a measured ceiling (about 3), first in `observe`, then in `enforce`. The runner pool stays at 6 until `iterion_capacity_unheld_redeliveries_total` shows the pre-T4a leak is small (decision Q10(a)). It then goes back to 8–10, and md0 is watched |
| **S3 — `team:` moves from the gate to the ledger** | after S2 and #3254 T4a | ADR-175 D4's own trigger: the first team with a concurrency cap above 0 in production. The gate's 429 retires for the team cap, and the floors move with it (§4) |
| **S4 — `gw:`** | on ADR-167's activation | ADR-167 T4b and T5 are built on this state: no `GATEWAY_SLOT_WAIT` park is ever written |
| **S5 — keys, and the runner's C2** | after S3 has run a week in `enforce` | The last pre-S2 `CRED_SLOT_WAIT` parks are drained (`capacity ls` shows none), and the retry sweeper's capacity code (`deferCredSlotRetry`) goes. T4a's C2 may then write the wait, with the runner's release in the guard; ADR-167's C2 ledger-error clause (:44) is amended then |
| **S6 — `org:`** | ADR-175 D1's trigger | |
| **S7 — `rate:`** | after S3, on a measured start burst | The launch rate becomes a per-team admission pace, refining S2's global drain pace. Its 429 retires, and the bucket's key and its Valkey twin across replicas are specified here (today's bucket is per replica without Valkey, [pkg/server/launch_gate.go:903-918](../../pkg/server/launch_gate.go)) |
| **S8 — local** | after S5 | The pipeline scheduler retires onto the state and the filesystem ledger |

**Between S2 and S5, a key refuses a waiting run.** The outcome depends on the key kind's mode, and S2 tests both cases:

- **Key kind `enforce`:** the key is reserved in the same all-or-nothing loop (§3), so the run stays `waiting_capacity` and names the key among its axes. It is not parked `CRED_SLOT_WAIT`.
- **Key kind `off`, the default until enforced:** the run is parked `CRED_SLOT_WAIT` as today (§7).

**The S2 leak, named in full.** Before #3254 T4a (C1–C3) lands, a `failed_resumable` run holds no place (`HoldsCredentialSlot`, [pkg/store/lifecycle.go:415-417](../../pkg/store/lifecycle.go)). A NATS redelivery of that run resumes it with no admission (ADR-175 D4, ADR-167 Context). Every source of `failed_resumable` opens a window in which the scheduler can give the place away:

- the runner's nak paths: interruption or drain, sandbox timeout, sandbox capacity, attachments, orphan adoption;
- the server's orphan sweeper.

The leak is therefore wider than a drain. On a capped bot under contention, every nak can let one more run start. `iterion_capacity_unheld_redeliveries_total` counts it. S3 waits for T4a because a tenant cap is a promise. `bot:` enforces before T4a (decision Q10(a)), with the leak counted. The runner pool stays at 6 until the counter shows the leak is small.

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
- **ADR-175 D4 changes contract.** Its "429 `concurrency_cap_exceeded` with `Retry-After`" and its "parked `failed_resumable` under `TENANT_SLOT_WAIT`" (:88) are replaced by a waiting run. Its ledger-error clauses (:89, :144, :146) are amended by decision Q3(a): a ledger error makes the run wait on `ledger_error` instead of refusing it. So is ADR-167 §7's launch-side clause (:59). ADR-167 §4 C2's runner-side "deferred nak" (:44) stays until S5, when the runner may write the wait. Both records carry an "Amended by" pointer. Its order of rows, its single stamping helper, its tree rule and its dependency on #3254 stand. The token `concurrency_cap_exceeded` stays only for the "all slots reserved" refusal, until that refusal gets a token of its own (`concurrency_slots_reserved`).
- **The caller sees a run where it saw a 429.** A CLI or API caller that relied on the 429 to back off now gets a run, which it can cancel. That is a behaviour change, written into the API reference.
- **New durable state, each with its twin:**
  - the status;
  - `capacity_wait` on runs;
  - the `bot:`, `team:`, `org:` and `rate:` rows (pace rows hold no holders);
  - the `capacity_scheduler` platform switch;
  - one lease;
  - an index on `(status, capacity_wait.since)`.
- **Admission costs more for a waiting run.** It is re-resolved and re-sealed at admission, and the fast-path resolution is spent for nothing when the run waits. That is the price of not dialling with stale credentials, and of keeping no sealed material at rest on a waiting run.
- **Contention.** `org:<id>` and a hot `bot:` row are written by every admission under them. Under a burst, the bounded rounds (`credSlotReserveRounds = 32`, [pkg/store/mongo/cred_slots.go:161](../../pkg/store/mongo/cred_slots.go)) can be exhausted. The store error then makes the run wait on `ledger_error` (§3), counted and alerted. It is never an admission on a guess.
- **A wait is unbounded unless `capacity_wait_deadline` is set** (decision Q4(a)). A run waiting behind a bot whose ceiling the operator never raises waits until it is cancelled, and the alert on the oldest wait is the operator's signal.
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

## Decisions taken (operator, 2026-11-27)

The operator answered the ten questions of the reviewed draft. Each entry below records the decision, then the options that were weighed: the record of what was set aside.

1. **Resumes wait too: (b), "the most durable".** (F3, F4) The run persists a durable `resume_spec`; its fields, their care, and when it is cleared are in §3. A waiting resume inherits its attempt's `since`, and the pause pointer survives `SaveRun`. The `CRED_SLOT_WAIT` park is retired as a writer from S2 for the key kind in `enforce`. It stays the key kind's behaviour while that kind is `off` (§7), and S5 removes it. The rollback is proven in S2 (§3, §7).
   - Weighed:
     - (a) only launches without a checkpoint wait, and resumes keep the park and its `RevertQueuedFlip` rollback;
     - (c) resumes wait with their request re-derived from the run, which cannot rebuild `Admitted` or `Answers`.
2. **The bot ceiling's scope: (a), deployment-wide on the canonical name.** Teams are shown the aggregate. (F23)
   - Weighed:
     - (b) per (team, bot), which bounds nothing shared: 13 teams × 3 is 39 sandboxes;
     - (c) both, with a per-team share built later.
3. **The ledger is unavailable: (a), the run waits on a `ledger_error` axis.** It has its own counter and alert, and no pull-request notice. ADR-175 D4's ledger-error clauses and ADR-167 §7's launch-side clause are amended explicitly; ADR-167 C2's runner clause is amended at S5. (F14)
   - Weighed: (b) refuse, as both records said, which loses the immediate lane's event during an outage.
4. **A wait's deadline: (a), a distinct `capacity_wait_deadline`, unbounded by default.** Expiry, or a refusal taken during the wait, ends the run `failed_resumable`. (F18)
   - Weighed:
     - (b) the same, but ending `failed`, which is terminal and not auto-resumable;
     - (c) reusing `retrypolicy.MaxWait`, a retry clamp of 8 days by default.
5. **The monthly charge: (a), charged at creation and refunded on every end that never started**, through S2's durable release. A per-team `MaxWaitingRuns` bounds creation from S2 (200 proposed, measured in S0), with a 429 `capacity_queue_full` beyond it. (F17)
   - Weighed:
     - (b) charge at admission, which neither refunds nor bounds creation;
     - (c) refund only on cancel and supersede, with no bound until S7.
6. **A replacement cancels its predecessor at its own admission: (a).** (F10) A waiting predecessor that has never started, collapsed by Q9, is the stated exception; a waiting resume follows this rule (§5).
   - Weighed: (b) cancel at creation and accept the loss.
7. **The launch rate: (a), a per-team pace at admission, in S7.** Q5's creation bound takes over the rate's role against abuse.
   - Weighed: (b) keep the rate a refusal.
8. **No interactive opt-out: (a).** Cancel is the opt-out.
   - Weighed: (b) `wait: false` on the REST launch, giving back the old 429.
9. **The collapse of waiting runs: (a), on a newer head, for every forge-subject bot, whatever the overlap policy.** (§5; F11's inherited place)
   - Weighed: (b) opt-in per bot.
10. **`bot:` may enforce before #3254 T4a: (a).** The leak is counted. The runner pool stays at 6 until the counter shows the leak is small, and then goes back to 8–10. (F16)
    - Weighed: (b) wait for T4a.

**Settled after the decisions (2026-11-27).**

- **`HostInputs` keeps its contract.** It is not persisted in `resume_spec`. A resume that carries it and finds no capacity keeps today's `failed_resumable` park, a counted exception (§3).
- **Q6 × Q9 is read as §5 writes it.** A waiting predecessor that has never started (no checkpoint and no `resume_spec`) is collapsed into the newer run at the newer run's creation, and the newer run inherits its `since`. A **waiting resume** counts as started. A **started** predecessor is cancelled at the replacement's admission. Under Q4, a newer run that expires or is refused ends `failed_resumable`, so the newest head stays resumable.

### Coordination with ADR-175/ADR-167 owners

These go to the owning sessions through the PR and the tickets, and this record does not decide them.

- **[#3254](https://github.com/SocialGouv/iterion/issues/3254) (ADR-167, T4a): Q1(b) widens S2.** The fast path reserves keys in the same all-or-nothing loop. From S2, a key refusal therefore makes a launch or a resume wait, and no `CRED_SLOT_WAIT` is written: work this draft had put in S5.
  - T4a's arbitrations of 2026-11-26 (frozen-IR re-admission, and C2 parks charged as attempts) are built on the retry sweeper's parks.
  - **ADR-176 needs from #3254:** T4a's frozen-IR re-admission should also accept a run admitted from `waiting_capacity`. The removal of the sweeper's capacity code (`deferCredSlotRetry`, S5) should be sequenced after T4a's own use of it.
  - **#3254 needs from ADR-176:** C2 keeps "free retry armed" until S5, and the runner writes no `waiting_capacity` before then.
- **#3254 (ADR-167) and [#3748](https://github.com/SocialGouv/iterion/issues/3748) (ADR-175): Q3 against ADR-167 C2.** A ledger error waits on `ledger_error`. That amends now ADR-175 D4's ledger-error arm and its Consequences (:89, :144, :146), and ADR-167 §7's launch-side clause (:59). ADR-167 §4 C2's runner-side "deferred nak" (:44) is amended only at S5, because the runner does not write the wait before then.
  - **ADR-176 needs from #3748:** acceptance that D4's fail-closed refusal becomes a fail-closed wait.
  - **ADR-176 needs from #3254:** the same for §7, and C2 left as written until S5.
  - Both records carry only an "Amended by" pointer from this session.

## Adversarial review disposition (GLM-5.4 via Claude Code, max — 25 findings)

Each finding was checked against the code at `8b001cdc6` before its verdict, and none was refuted. The review itself: `~/.claude/plans/rvp-176/review.md`, operator-local.

Adopted into the plan:

- **F1** reserving while still `waiting_capacity` gets the entry evicted: the run must `SpendsCredential`. Fix: CAS to `queued` with `admission_pending` first, then reserve (§3).
- **F2** an overlapping loser's `ReleaseCredSlot` removes the winner's refreshed entry. Fix: the CAS loser neither reserves nor releases, with a conformance row (§3).
- **F5** the cancel CAS expects a list built from `AllRunStatuses`, so an old binary cannot cancel. Fix: a predicate CAS in S1, S1 as the release floor, and a named way back (§1, §7).
- **F6** activation, a raised cap and a rollback each drained everything at once. Fix: a per-pass and per-minute admission bound from S2 (§3).
- **F8** "full" is keyed by (subject, effective `max`), so a reserved bot is still tried (§3, §4).
- **F9** the delivery row goes `launched` at creation; `accepted` is a 10-minute in-flight state (§1, §5).
- **F11** a replacement, and a collapse, inherits `since` (§1, §5).
- **F13** `off` / `observe` / `enforce` are defined, with `iterion_capacity_would_wait_total` (§6, §7).
- **F16** the leak names every source of `failed_resumable` (§8). Under Q10(a), the runner pool stays at 6 until the counter shows the leak is small.
- **F19** the reader matrix is completed: `SaveRun`'s pause scrub, `CanReclaimResumePauseHandoff`, `WatchDeliverable`, `IsQueued`, `IsPaused`, and `TeamCutoverStatusAlive`, which errored (§1).
- **F20** nothing sealed rests on a waiting run (§3).
- **F21** a key refusal between S2 and S5 keeps the run waiting, with several axes (§8).
- **F22** the orphan pass on `admission_pending` is not skipped under backlog (§1, §3).
- **F23** shared-subject counts are shown as an aggregate (§4, §6), under Q2(a).
- **F3** under Q1(b): resumes wait, with a durable `resume_spec` (§3).
- **F4** under Q1(b): a waiting resume inherits its attempt's `since`, and the preserved fields are listed (§3).
- **F10** under Q6(a): a replacement cancels its predecessor at its own admission (§5).
- **F14** under Q3(a): the run waits on a `ledger_error` axis, and the clauses of ADR-175 and ADR-167 are amended explicitly (§3, Consequences).
- **F17** under Q5(a): the durable refund on every end that never started, and `MaxWaitingRuns` from S2 (§2).
- **F18** under Q4(a): a distinct `capacity_wait_deadline`, ending `failed_resumable` (§3).
- **F24** a tree is one place, and `dep-update-guard` is never a sub-bot in the catalogue (§4).

Adjusted:

- **F7** (confirmed: `Admit` lets an unknown status proceed, and the delivery then naks to the DLQ and reports `run.failed`. The drop branch and the `BuildRunOutcome` branch ship in S1, not S2: readers before writers.)
- **F12** (the fleet version guard, `accept_unstamped` and the activation guard are stated as non-existent and built in S2, with an audited attestation as the fallback if the guard cannot be built in S2.)
- **F15** (the runner never writes `waiting_capacity` before S5. C2 keeps "free retry armed", and T4a is named as not on main.)

Deferred follow-ups (documented):

- **F25** the `rate:` bucket key and its cross-replica twin are specified in S7 (§8).
