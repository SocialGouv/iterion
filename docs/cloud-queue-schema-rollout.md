# Queue schema rollout runbook

How to ship a `queue.RunMessage` schema bump (`pkg/queue/types.go`,
`SchemaVersion`) without executing a payload with silently dropped semantics,
and without losing runs while the server and runner fleets run mixed
versions. Read it before every schema bump.

This runbook is **mandatory** for every schema bump, and ordering is the first
decision it makes for you: **server-first by default**, because that puts any
parked message on the replayable side. Runner-first is an optimization with a
precondition — see *Deploy ordering* — and the drain/replay paths below are
what you fall back to when neither ordering can spare the queue.

## Wire compatibility policy

- **Explicit compatibility window.** A consumer accepts only
  `[MinSchemaVersion, SchemaVersion]` and rejects anything outside it in both
  directions. The current v12 consumer accepts v10/v11 backlog explicitly;
  this is not implicit forward compatibility.
- **Server first by default.** Both orders can park a message; only one park is
  replayable. Old runners rejecting the new version park messages a DLQ replay
  fixes once the fleet is upgraded; new runners rejecting a version below their
  `MinSchemaVersion` park messages a replay can never fix. Server-first keeps the
  risk on the recoverable side, which is why it shipped WITH the window
  (`bdafa72a0`) rather than before it. Roll the runners first only when nothing
  below `Min(new)` can still be queued — the precondition, its check, and the
  measured case are in *Deploy ordering* below.
- **Additive field whose omission changes operator intent = breaking
  change.** If a new field carries a decision the caller explicitly made
  (budget caps, skills, auto-memory, loop guard, model pins…), a stale runner
  that silently falls back from it is a correctness bug, not a cosmetic one.
  Such a field REQUIRES a schema bump. History: v4 `Budget`, v5
  `Contributions`, v6 `AutoMemory`, v7 `LoopBudgetGuard` — each exists
  because dropping it would quietly re-make the operator's choice on the pod.
- **Known historical debt.** `ModelOverrides` was added during v7 without a
  bump (commit `427a9f44e`), so an earlier v7 runner could accept and ignore
  those pins. The later v8 bump cannot retroactively repair that window; this
  rule records the lesson so the next intent-bearing field ships atomically.
- **The exemption, and how to tell.** A field is exempt only when a runner
  that ignores it cannot fail OPEN. `BudgetOverrides.cap_imposed` was added
  inside v8 with no bump on exactly that ground: it carries no operator
  choice (the publisher *derives* it from a clamp — see
  [credential-pool.md](credential-pool.md)), and the only consumer that would
  act on it, the runtime's budget exit grace, shipped in the same commit. A
  runner old enough to drop the field is old enough to have no grace to
  refuse, so ignoring it costs nothing. Apply the test in that direction: not
  "is the field new?" but "what does a runner that never sees it do instead,
  and is that safe?"
- **Until the bump ships, reject — never drop.** A launch that carries a
  field the current wire version cannot transport must fail loudly at publish
  time. Once the carrier and version bump ship together, the rejection can be
  removed. Schema v8's `Supervisors` kill switch is the reference transition.

## What a mixed fleet does to a mismatched message

Since #481, a version mismatch is transient and recoverable:

1. The consumer **Naks with a 30s delay**
   (`nats.SchemaMismatchNakDelay`, configurable with
   `ITERION_RUNNER_SCHEMA_MISMATCH_DELAY` or
   `runner.schema_mismatch_delay`), so the MaxDeliver budget (default 8)
   stretches over ~4 minutes of wall clock instead of being burned in
   seconds — enough for a rolling restart of the runner Deployment to
   schedule a pod that speaks the new version.
2. If the budget is exhausted anyway, the consumer **parks the payload
   verbatim on the DLQ**, Terms the queue entry, and flips the run document
   from `queued` to `failed_resumable` with an actionable error pointing at
   this runbook and `/api/admin/dlq`. The run is never silently dropped and
   never left `queued` with no recovery path.
3. A DLQ replay re-publishes the **exact original bytes**, so a parked v8
   message replays correctly once runners run v8.

## v12 runner-epoch bootstrap

Schema v12 adds `runner_epoch`. A stale v11 runner must reject v12; if it
ignored the field it could execute work after its generation was superseded.
A v12 runner treats v10/v11 messages with no field as epoch 0.

Do not combine the schema bump and first non-zero epoch:

1. **Release A:** deploy v12 with `config.rollout.runnerEpoch: 0`. v11 → v12
   leaves `MinSchemaVersion` at 10, so the runner-first precondition holds and
   neither Path A nor Path B applies — see *Deploy ordering* below. Verify all
   server and runner probes show `epoch: 0`. (Done in prod on 2026-09-02:
   runner fleet, then server, DLQ untouched throughout — the measurement at
   the end of *Deploy ordering*.)
2. **Release B:** set the epoch to 1. New publishers stamp epoch 1. Old v12
   runners delayed-Nak those messages; new runners accept both epoch 0 and 1.

Epoch refusals use their own `ITERION_RUNNER_EPOCH_MISMATCH_DELAY` (default
2m), but share the final DLQ + `failed_resumable` disposition. Both mismatch
delays feed `RedeliveryWindow()` so the orphan sweeper cannot race a message
legitimately waiting for a compatible runner.

The persistent rollout high-water mark makes an epoch decrease non-ready and
blocks run publication/consumption. A rollback is therefore a roll-forward:
re-release the previous fence-aware image with an epoch greater than the
current high-water mark. Never restore a lower-epoch Helm revision directly.

> **This section describes runner builds from v8 onward.** A pre-#481 (v7
> or older) runner answers a mismatch with an immediate bare `Nak()`: it
> burns the MaxDeliver budget in seconds, JetStream drops the message, and
> nothing is parked to replay. That asymmetry drives the path choice below.

## Deploy ordering: which side rolls first

Both orders can park a message. They differ in **whether the park is
recoverable**, and that — not "which side rejects less" — is what decides.

| roll first | what gets rejected | recovery |
|---|---|---|
| **server** | old runners reject the new vN+1 | **replayable**: once every runner is new, `/api/admin/dlq/$SEQ/replay` re-publishes the same bytes and they are accepted |
| **runners** | new runners reject anything still queued *below* `Min(new)` | **not replayable**: a replay re-publishes the same old bytes, the new fleet rejects them identically and re-parks (Path B step 5) — recovery is a per-run resume plus a DLQ delete |

Server-first is therefore the safe default, and deliberately so: it was
authored together with the compatibility window (`bdafa72a0` adds both
`MinSchemaVersion = 8` and the rule), not before it. The window did not make
it obsolete — it put the parking risk on the side that can be undone.

**Runner-first is an optimization, valid under one condition:** that nothing
below `Min(new runner)` can still be in the stream. Then new runners admit
everything — including what the old server is still publishing — and *nothing
is rejected in either direction*, so neither Path A nor Path B is needed.

### Deciding which you are in

`MinSchemaVersion` unchanged by the bump ⇒ runner-first is safe with no
further check: the new window is a superset of the old one, so no resident
message can fall below it.

`MinSchemaVersion` raised ⇒ **check the queue**, or take server-first. A
raised `Min` is the common shape, not the exception:

    8 (v9) → 9 (v10) → 10 (v11) → 10 (v12)

Three of the four bumps since the window shipped raised it, each by exactly
one. That shape is a trap for the naive test `Min(new) <= SchemaVersion(old
server)`: it passes, while a message *one version older* still queued is
rejected — unrecoverably.

What "still in the stream" means here is narrower than it sounds. The runs
stream is created with `Retention: WorkQueuePolicy`
([pkg/queue/nats/nats.go](../pkg/queue/nats/nats.go)), so an ACKed message is
removed immediately; `MaxAge` (24h) bounds only how long an **unacked** one
may linger. Residue is therefore not "everything published in the last 24h" —
it is what is still pending or still being redelivered: a run whose deliveries
are bouncing, or a backlog the fleet has not reached. `iterion_nats_pending_messages`
= 0 is the clean signal; a non-zero pending count on a `Min`-raising bump is
the case to take seriously.

### The cost of server-first, stated honestly

A vN+1 server publishing into a vN-only fleet gets every delivery
delayed-Naked until a new-version runner is Ready. That is a race, not a
certainty of loss: the 30s delay exists precisely to stretch the 8-delivery
budget over ~4 minutes and cover a rolling restart (see *What a mixed fleet
does* above and the constant's own justification), and under the chart's
`maxSurge: 100%` / `maxUnavailable: 0` the first new runner is usually Ready
early in the roll. What races the budget is time-to-first-Ready-new-runner,
not the full roll — which under `drainMode: complete` can take hours.

A fleet slow to become Ready spends the budget and parks the run
`failed_resumable` — replayably. Runner-first removes that race; it is worth
taking **when the condition above holds**, and it is what the v11 → v12
cutover used (see the measurement below).

### Sequencing the two Deployments

A plain `helm upgrade` rolls both together: the server and runner Deployments
resolve to the same image tag (`include "iterion.image" .`). Use the chart's
per-runner image override to sequence the phases — here in runner-first order:

```bash
# Phase 1 — pin the runners to the NEW image, leaving the server on the old tag.
helm upgrade iterion ./charts/iterion \
  --set image.tag=<old-tag> \
  --set runner.image=ghcr.io/socialgouv/iterion:<new-tag>

# Phase 2 — once every runner is Ready on <new-tag>, roll the server.
helm upgrade iterion ./charts/iterion --set image.tag=<new-tag>
```

**The snippet above is the runner-first order — the optimization, not the
default.** Path A and Path B below are server-first, and that is a different
command, not a swap of the two values (`image.tag` takes a tag,
`runner.image` a full reference, so exchanging them renders an invalid image):

```bash
# SERVER-FIRST — Path A step 3 and Path B step 1 only.
# Phase 1 — roll the server, holding the runners on the OLD image.
helm upgrade iterion ./charts/iterion \
  --set image.tag=<new-tag> \
  --set runner.image=ghcr.io/socialgouv/iterion:<old-tag>

# Phase 2 — once the server is Ready on <new-tag>, release the runners.
helm upgrade iterion ./charts/iterion --set image.tag=<new-tag>
```

Getting that backwards on Path B is the destructive direction: it rolls the
runners into a queue full of vN messages they reject, and per Path B step 5
those parked messages are **unreplayable** — recovery is a per-run resume
plus a DLQ delete.

Both snippets assume the install moves its image via `image.tag`. If your
values pin the image some other way, `--set image.tag=` may not move it at
all — check that the rendered PodTemplate actually changed before treating a
phase as done. Note `runner.image`, when set in a values file, already
overrides `iterion.image` for the runner container, so phase 1's
`--set runner.image=` must name a full reference and phase 2 must clear or
advance it rather than rely on the shared tag.

If your deploy pipeline sequences Deployments itself, use it instead — the
requirement is only that the two Deployments do not roll together.

**Both snippets sequence the IMAGE only.** A release that also moves
`config.rollout.runnerEpoch` does not get sequenced by them: that is a single
value rendered as a literal `ITERION_RUNNER_EPOCH` into BOTH PodTemplates
(server-deployment.yaml and runner-deployment.yaml), so phase 1 rolls both
Deployments. Move the epoch in its own release — which is what the two-release
Release A / Release B protocol above already prescribes — not in the same one
as a schema bump.

> Measured on the v11 → v12 cutover (2026-09-02, prod: 12 runner pods, 3 server
> pods, live traffic including a run executing across the runner roll).
> Runner-first left the DLQ byte-identical — depth 30, `last_seq` 220, newest
> message hours older than the rollout — with no
> `iterion_runner_admission_rejected_total` sample anywhere in the fleet.

## Rollout procedure for a schema bump vN → vN+1

These two paths accompany the **server-first** default. Choose **one** of
them: ordering alone is not a third option, because a server-first roll still
parks the new version on the old fleet if no upgraded runner becomes Ready
inside the delivery budget — Path A avoids that by draining first, Path B
accepts it and replays afterwards.

They are unnecessary **only** when the runner-first precondition holds —
nothing below `Min(new runner)` can still be queued, which is automatic when
the bump leaves `MinSchemaVersion` alone. Then nothing is rejected in either
direction and there is nothing to drain or replay. See *Deploy ordering*
above; do not infer that case from the old server's version alone.

### Path A — drained queue before cutover

**Mandatory for v7 → v8** (see below); the recommended default for every
later bump as well.

1. Stop new launches (maintenance window) or accept that launches during the
   window follow Path B — which, for v7 → v8, they cannot: keep the window.
2. Wait for the queue to drain: `iterion_nats_pending_messages` = 0 and no
   `queued` runs older than the AckWait window.
3. Deploy the server (vN+1), then roll the runners (vN+1) — the **SERVER-FIRST
   snippet** above, not the runner-first one. (If step 1 held a real
   maintenance window and step 2 reached zero, ordering is moot here: nothing
   is left for either side to reject — and a drained queue is exactly the
   precondition that also makes runner-first safe, so either order works. It is
   not moot when step 1 was skipped and launches continued, which the step
   explicitly allows — so follow the order regardless.)
4. Sanity-check: one launch end-to-end, DLQ depth 0.

### Path B — DLQ identification + replay after cutover

**Valid only from v8 → v9 onward.** Path B leans on the delayed Nak, the
DLQ park and the status flip — and those ship *with* schema v8. For the
v7 → v8 cutover itself the outgoing runners are pre-#481 builds: they bare-
`Nak()` a v8 message, burn its MaxDeliver budget in seconds, and leave an
empty DLQ and a run stuck `queued` — the exact loss #481 closes. **Do not
run Path B for v7 → v8; use Path A.**

1. Deploy the server (vN+1) first, then roll the runners — the **SERVER-FIRST
   snippet** above, not the runner-first one. This is the path where the wrong
   order parks messages that step 5 cannot replay. During the window, stale
   runners hold vN+1 messages via delayed
   Naks; after MaxDeliver they park them on the DLQ and flip the runs to
   `failed_resumable`.
2. Once **all** runners run vN+1, list the DLQ and identify the parked
   messages from the transition — the `Iterion-DLQ-Reason` header reads
   `queue: schema version: N+1 unsupported (want N)`:

   ```bash
   curl "https://iterion.example.com/api/admin/dlq?limit=200"
   curl "https://iterion.example.com/api/admin/dlq/$SEQ"   # peek: check the reason + run_id
   ```
3. Replay each transition message; the replay re-publishes the exact payload
   and a vN+1 runner picks it up:

   ```bash
   curl -X POST "https://iterion.example.com/api/admin/dlq/$SEQ/replay"
   ```
4. Verify each replayed run leaves `failed_resumable`: the runner treats the
   replayed launch payload as a resume and transitions it directly to
   `running`. Confirm the DLQ returns to its pre-rollout depth.
5. The reverse direction does **not** replay. If the queue still held vN
   messages when the vN+1 runners came up, they parked with reason
   `N unsupported (want N+1)` — and a replay re-publishes those exact
   bytes, which the vN+1 fleet rejects identically and re-parks: an
   operator replay loop that never succeeds. Recover those runs by
   **resuming** them instead (`POST /api/runs/{id}/resume`: the publisher
   stamps the CURRENT `SchemaVersion` and a resume-specific `Nats-Msg-Id`,
   so JetStream cannot mistake it for the original launch inside its
   five-minute deduplication window), then discard the stale parked copies
   with `DELETE /api/admin/dlq/$SEQ`.

## Checklist: v7 → v8 (Supervisors and #481 safety)

The v8 bump belongs to the launch-time `Supervisors` kill switch. The v8 wire
also carries `model_overrides`, but those had already entered v7 without a
bump (the known debt above); do not read this transition as retroactively
making every v7 build safe for model pins. The rollout preconditions are:

- [x] Delayed mismatch Nak, final DLQ park and actionable status flip.
- [x] `ModelOverrides` on `queue.RunMessage`, set by the publisher and
      applied by the runner executor; resumes preserve the same pins.
- [x] `SchemaVersion = 8` for `Supervisors`, so a stale v7 runner rejects a v8
      payload instead of re-deciding the operator's supervisor kill switch.
- [x] The live-JetStream mixed-fleet integration test covers both version
      directions and the recovery paths.
- [ ] Operators roll out per **Path A** (drained queue) — Path B is not an
      option for v7 → v8: the outgoing v7 runners predate the delayed-Nak /
      DLQ-park mechanics this release introduces, so only a drained queue
      protects in-flight messages during this specific cutover.

## If something went wrong

- **Runs stuck `queued` after a rollout**: check the DLQ (they parked there
  if this runbook's mechanics were deployed) and the orphan sweeper; replay
  per Path B.
- **A run executed with dropped semantics** (e.g. wrong model): that means a
  field crossed the wire without a schema bump. Treat as an incident, add
  the field to the additive-field rule above, and bump.
