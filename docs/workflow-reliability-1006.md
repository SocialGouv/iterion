# Workflow reliability rollout (#1006)

This document is the compatibility ledger for the reliability work. Each
tranche is independently deployable and is stacked on the previous one; an
operator can stop at any boundary without rewriting existing run records.

## Context contract (tranche A)

Every newly created in-process run carries `execution_context.version = 1`.
The record identifies:

- the Iterion run store (`run_store`), separately from business stores;
- the declared workspace isolation mode and logical workspace id;
- the compiled workflow revision, source root and optional bundle revision;
- root/parent run and parent-node lineage;
- the policy (`legacy`, `report`, or the future `enforce` gate).

The record contains identities and revisions only. It never stores credentials,
prompts, model output or business payloads. Older run documents omit the field
and remain readable; resume keeps the legacy behaviour until an admission
policy is explicitly enabled.

## Launch-surface matrix

| Surface | Context persisted | Admission owner | Planned tranche |
| --- | --- | --- | --- |
| local/API in-process launch | yes | runview service | A/B |
| local resume | existing context preserved | runview + runtime | B/C |
| detached launch | compatibility stamp | detached runner | B |
| cloud queued launch | queue/document propagation | publisher/runner | B |
| subbot child | parent/root reconciliation | runview subbot runner | B |
| watcher/Copi observation | diagnostic projection | watcher authority | F/G |

This matrix is intentionally explicit: a green unit test in one launch path is
not evidence that a queued or nested launch has the same context.

## Bounded invalid-output correction (tranche D)

Schema-invalid output is corrected only when the executor implements the
optional `runtime.OutputCorrector` capability. The engine allows at most the
configured budget (two calls by default, or `WithOutputCorrectionBudget(0)` to
disable it; `ITERION_OUTPUT_CORRECTION_BUDGET` sets the process default) for each node episode. Artifacts, events and outgoing edges are
written only after a corrected payload validates.

The episode ledger is persisted on the run document. It records the attempt
count, output/violation fingerprints and a terminal status (`succeeded`,
`exhausted` or `unchanged`). A corrector returning the same invalid payload is
stopped immediately, and a resume continues the existing budget rather than
starting a new loop. Executors must keep correction side-effect free; publish
and external-effect nodes remain behind the validation boundary.

## Coordinated retries and circuit breaker (tranche E)

Cloud runners retain the per-run retry budget, but usage-window failures also
update a tenant-scoped durable circuit keyed by workflow revision. Once the
shared failure threshold is reached, the next retry wave is delayed until the
breaker cooldown instead of starting one pod per run against the same provider
wall. A successful run clears the streak.

**The breaker moves a wake-up; it never overrides the run's own policy.** The
adopted cooldown is spread by the policy's `jitter` and clamped to its
`max_wait`, exactly like every other instant the retry path computes. Both
matter more here than elsewhere, because `open_until` is ONE durable instant
every run behind the breaker reads: unspread it would arm them all for the
same moment, and a breaker that replaces a storm of pods with a storm of pods
one cooldown later has bought nothing. The clamp is the same precedence the
platform ceiling obeys — an authority outside the run may only ever *lower* a
policy. A cooldown the ceiling clamps back is reported as not having
contributed, so the `run_retry_scheduled` event's `reset_source` never claims
a wait the run did not take.

**The streak decays.** A failure more than one cooldown after the previous one
restarts the count at 1. Without that, `consecutive_failures` could only ever
be cleared by a *successful* run of the same workflow revision — which a
revision that always fails never produces — so a single old storm would arm a
tenant-wide cooldown on the next isolated failure, forever. The decay is also
what keeps the threshold meaning failures *across runs, close together*
rather than one lonely run's retries, which are floored minutes to hours
apart, adding up over a week.

The circuit is an optional Mongo capability; local/filesystem stores keep
their existing retry behaviour. A circuit-store outage falls back to the
durable per-run retry rather than dropping work — and because a store outage
usually presents as latency rather than a prompt error, the circuit update
gets its own slice of the arming's store budget so a wedged collection cannot
starve the write that persists the retry.

Threshold and cooldown are controlled by `ITERION_RETRY_CIRCUIT_THRESHOLD`
(a positive integer) and `ITERION_RETRY_CIRCUIT_COOLDOWN` (a Go duration, e.g.
`15m`). Neither is an off switch: a value that is not usable keeps the default
and logs one line on stderr saying so, rather than reading as configured.
