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

1. Capture a baseline with `iterion reliability report` — finished, failed,
   failed-resumable, paused and queued runs, plus retry-armed and ledger-use
   counts, over the store the working directory resolves to (`--store-dir`
   overrides it, `--json` makes it diffable). The same command with
   `--run-id <id>` answers the per-run question instead: legacy or contract
   context, admission recorded, how many nodes published, and whether the run
   is rollback-safe. Both are strictly read-only — reporting on a legacy run
   never upgrades its policy. The report's first lines are the RESOLVED mode
   and context policy, so the baseline always states which of the two
   variables below was in effect when it was taken.
2. Start a pilot in `report` mode with `ITERION_RELIABILITY_MODE=report`.
   Admission decisions are recorded and surfaced, but legacy contexts remain
   runnable; correction and watcher ledgers are observational evidence that
   can be compared with the baseline.
3. Promote only the selected tenant/workflow revisions to `enforce` after
   queued, resumed, nested and watcher paths show matching context and
   artifact contracts. The retry circuit stays bounded by
   `ITERION_RETRY_CIRCUIT_THRESHOLD` / `ITERION_RETRY_CIRCUIT_COOLDOWN`.
4. Roll back by setting `ITERION_RELIABILITY_MODE=legacy`
   (`iterion reliability rollback` prints the plan verbatim, including which
   variable wins — it prints, it does not mutate: the variable is set where
   the launch surfaces read it, in a pod spec or a service unit, not by a
   one-shot CLI process). Do not delete the
   ledgers: they are the evidence needed to explain the pilot and make a later
   resume safe.

The output correction budget is deliberately **not a rollback lever** for this
pilot. `ITERION_OUTPUT_CORRECTION_BUDGET` does exist: it sets the process-wide
default (two attempts when unset), while `runtime.WithOutputCorrectionBudget`
overrides it for one engine. Correction still runs only when output validation
is enabled and the executor implements `runtime.OutputCorrector`; no production
path currently satisfies both conditions because `ClawExecutor` validates and
retries upstream of this optional engine path. Changing the variable therefore
does not disable the other reliability controls listed above. The
`output_corrections` ledger stays readable either way — pre-pilot runs simply
have none.

### The two variables, and which one wins

`ITERION_RELIABILITY_MODE` (`legacy|report|enforce`) is the operator-facing
switch for this rollout and it is **authoritative**.
`ITERION_EXECUTION_CONTEXT_POLICY` — the narrower, pre-existing switch this
rollout generalises — takes the same three values and is read when
`ITERION_RELIABILITY_MODE` is unset or invalid. Both resolve through one
implementation
([`reliability.ContextPolicyFromEnv`](../pkg/reliability/rollout.go), which
`runview.ExecutionContextPolicyFromEnv` delegates to), so what a report names
is what the launch surfaces apply.

The precedence is what makes step 4 an emergency lever rather than a
suggestion: a deployment that already carries
`ITERION_EXECUTION_CONTEXT_POLICY=enforce` rolls back by setting
`ITERION_RELIABILITY_MODE=legacy` alone — it does not also have to find and
unset the older variable. An unrecognised value emits a one-time warning; an
invalid new mode falls back to the older valid policy, while an invalid or
absent value in both variables resolves to `legacy`.

Legacy documents are always readable. Missing context, admission, correction
or watcher fields mean “pre-pilot”, not “successful”; the compatibility report
marks that state explicitly so a green status cannot be inferred by accident.
