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
wall. A successful run clears the streak. The circuit is an optional Mongo
capability; local/filesystem stores keep their existing retry behaviour, and a
circuit-store outage falls back to the durable per-run retry rather than
dropping work. Threshold and cooldown are controlled by
`ITERION_RETRY_CIRCUIT_THRESHOLD` and `ITERION_RETRY_CIRCUIT_COOLDOWN`.

## Progress-sensitive watchers (tranche F)

Supervisors keep a durable cursor per `(run, supervisor, watched-node set)`.
The cursor stores the last progress fingerprint, evaluation/action trigger and
next evaluation time. A repeated event therefore cannot bypass the cooldown
by being redelivered, and a process restart restores the same suppression
window before it can enqueue another corrective message. Failed evaluator
calls clear only the trigger fingerprint and remain bounded by the existing
consecutive-failure cap. Launch surfaces that expose a run store opt in via the
capability method; other observers retain the existing in-memory behaviour.

## Pilot, compatibility and rollback (tranche G)

The rollout is deliberately reversible:

1. Capture a baseline with `reliability.Summarize` (or the equivalent
   operator report) for finished, failed, failed-resumable, paused and queued
   runs, including retry-armed and ledger-use counts.
2. Start a pilot in `report` mode with `ITERION_RELIABILITY_MODE=report`.
   Admission decisions are recorded and surfaced, but legacy contexts remain
   runnable; correction and watcher ledgers are observational evidence that
   can be compared with the baseline.
3. Promote only the selected tenant/workflow revisions to `enforce` after
   queued, resumed, nested and watcher paths show matching context and
   artifact contracts. Keep the per-run retry and correction budgets bounded.
4. Roll back by setting `ITERION_RELIABILITY_MODE=legacy` (and, if required,
   `ITERION_OUTPUT_CORRECTION_BUDGET=0`). Do not delete the ledgers: they are
   the evidence needed to explain the pilot and make a later resume safe.

### The two variables, and which one wins

`ITERION_RELIABILITY_MODE` (`legacy|report|enforce`) is the operator-facing
switch for this rollout and it is **authoritative**.
`ITERION_EXECUTION_CONTEXT_POLICY` — the narrower, pre-existing switch this
rollout generalises — takes the same three values and is read only when
`ITERION_RELIABILITY_MODE` is unset. Both resolve through one implementation
([`reliability.ContextPolicyFromEnv`](../pkg/reliability/rollout.go), which
`runview.ExecutionContextPolicyFromEnv` delegates to), so what a report names
is what the launch surfaces apply.

The precedence is what makes step 4 an emergency lever rather than a
suggestion: a deployment that already carries
`ITERION_EXECUTION_CONTEXT_POLICY=enforce` rolls back by setting
`ITERION_RELIABILITY_MODE=legacy` alone — it does not also have to find and
unset the older variable. An unrecognised value in either resolves to
`legacy`; it never falls through to the other variable, so a typo in a
rollback cannot leave enforcement on.

Legacy documents are always readable. Missing context, admission, correction
or watcher fields mean “pre-pilot”, not “successful”; the compatibility report
marks that state explicitly so a green status cannot be inferred by accident.
