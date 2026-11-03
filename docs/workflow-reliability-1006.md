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
   counts (`--json` makes it diffable). **Over the store the pilot runs on:**
   the plain command reads the filesystem store the working directory
   resolves to (`--store-dir` overrides which one), which is a developer's
   local runs, not the population a cloud promotion is decided on;
   `--cloud` reads the Mongo run store through the same configuration the
   server and runner use (`ITERION_MONGO_URI` plus `ITERION_S3_*`, which
   that store requires to open at all), and `--tenant <id>` scopes the
   baseline to one tenant. The report's `store` line names the source it
   actually read, so a before/after pair cannot silently compare two
   different populations. The same command with `--run-id <id>` answers the
   per-run question instead: legacy or contract context, admission
   recorded, how many nodes published, and whether the run is
   rollback-safe. All of them are strictly read-only — reporting on a
   legacy run never upgrades its policy. The report's first lines are the
   RESOLVED mode and context policy, so the baseline always states which of
   the two variables below was in effect when it was taken.
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

### The tiers above those variables: a level, and the env as a floor

The promotion step is `enforce` **by level**: a run-affecting setting belongs
to a database level above the bot, not to a process variable (ADR-132,
[#2699](https://github.com/SocialGouv/iterion/issues/2699)). That resolver
does not exist yet, so what ships here is the half that can be true today —
the resolution is TIERED, and the process environment is its documented
FLOOR:

1. an explicit `ITERION_RELIABILITY_MODE=legacy` is a **cap**, not a floor.
   No level overrules it. Step 4 above promises an emergency lever, and
   under plain floor arithmetic a level resolving to `enforce` would raise
   the gate straight back over the operator pulling that lever;
2. otherwise the stricter of the level and the floor wins
   (`legacy` < `report` < `enforce`): a level may **raise** the gate above
   what the operator set on the process, and may not lower it — a database
   row must not quietly un-enforce a host an operator set to `enforce`;
3. a level value the resolution cannot parse is not an answer. The floor
   stands, exactly as it does for an unparseable variable;
4. `ITERION_EXECUTION_CONTEXT_POLICY=legacy` is **not** the lever. That
   variable is the narrower switch this rollout generalises, the rollback is
   performed through the authoritative one, and treating it as a cap would
   make every deployment still carrying it un-promotable by a level.

Today every caller passes `reliability.ModeUnset` for the level, so the
floor is the whole answer and the resolved mode is exactly what the two
variables said before this tier existed. `ModeUnset` is the greppable seam:
when the level resolver lands, its caller passes the level-resolved mode to
`ModeFloor.Resolve` instead of adding a second resolution the launch
surfaces could disagree with.

`report` is the hatch this ordering makes usable: a scope promoted too far
is walked back by lowering its level to `report` — admission decisions keep
being recorded and surfaced, and not one run is refused — without touching
the process floor, and the floor itself remains the one place an operator
stops everything.

Legacy documents are always readable. Missing context, admission, correction
or watcher fields mean “pre-pilot”, not “successful”; the compatibility report
marks that state explicitly so a green status cannot be inferred by accident.

## The divergence baseline (tranche 8 close-out)

The eight implementation tranches landed in September 2026. What the design
owes after them is this: state, against the code and not against the plan,
which of its eleven acceptance criteria the tree actually meets. A criterion
is `met` only when a named witness refuses its violation; anything else is
`partial` or `open` and says what is missing. `verdict` is the measurement,
not an intention.

| # | Acceptance criterion | Owner | Witness | Verdict |
| --- | --- | --- | --- | --- |
| 1 | A missing prerequisite is diagnosed before any model call | [`pkg/runtime/admission.go`](../pkg/runtime/admission.go) (`admitRun`, reached from `engine_run.go` before attachments, workspace setup and the first executor call) | `TestAdmissionEnforceDeniesBeforeExecution` | met, for the contract the gate owns — see row 3 for what the gate does not compare; the BEFORE itself is the call site's order (`engine_run.go`, `resume.go`) and no cited test observes a node that did not run, so that qualifier is an owed witness |
| 2 | CLI, Studio and Copi show the same structured cause for a parent or child failure | [`pkg/runview/diagnostic.go`](../pkg/runview/diagnostic.go) (one versioned projection) | `TestBuildDiagnostic_ProjectsNestedNodeOutputDetail`, `TestSnapshotBuilder_IncludesTheSameDiagnosticProjection` | met |
| 3 | A deliberately isolated subbot is accepted; an UNDECLARED store divergence is blocked before fan-out | `ExecutionContext.BusinessStores` is declared and normalised ([`pkg/store/execution_context.go`](../pkg/store/execution_context.go)); the child keeps the parent's store declarations ([`pkg/runner/subbot.go`](../pkg/runner/subbot.go)) | first half: the subbot resolution; second half: **none** | **open** — `admitRun` names neither `RunStore` nor `BusinessStores`: the declaration is recorded and never compared with the effective stores, so no divergence can be blocked by it |
| 4 | An eligible invalid output is corrected at its producer within a defined limit | the producer: the verified-action ladder for a `tool` node whose recipe is a command or a script ([`pkg/backend/model/executor_verified_action.go`](../pkg/backend/model/executor_verified_action.go), ADR-044's bound `max_repair_attempts` / `max_agent_attempts`, postcondition as the only oracle; `ir.ToolRecipeSelfRepairable` is the gate, and a registry recipe that budgeted repairs alone reaches NO rung and is warned C179) and the schema re-ask for model nodes (`validateAndRetry`, [`pkg/backend/model/executor_build_task.go`](../pkg/backend/model/executor_build_task.go)); the engine's optional loop ([`pkg/runtime/node_output.go`](../pkg/runtime/node_output.go) + `WithOutputCorrectionBudget`) is the seam a schema-level corrector would implement | `TestVerifiedAction_SelfRepair`, `TestVerifiedAction_OnlyARepairableRecipeEntersTheSelfRepairRung`, `TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent`, `TestVerifiedAction_RecoverExhaustedFails`, `TestValidateAndRetry_InjectsSchemaFeedback`, `TestValidateAndRetry_BooksTheClawRecoveryCall`; engine seam: `TestSchemaValidation_BoundedCorrectionPersistsSuccess` | **partial**, on TWO named gaps in the producer's bound ([ADR-152](adr/152-the-reliability-invariants-have-owning-layers.md), decision 3): the ladder's limit is the one the workflow author declares, and the tree witnesses HALF of it — rung 4 spends exactly the declared attempt (`TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent` reddens when that loop is inflated: ten agent calls for a budget of one), rung 3 is entered only for a repairable recipe, and exhausting the rungs fails the node instead of looping. What no test refuses is rung 3's attempt COUNT — inflate `max_repair_attempts` and the whole producer package stays green — so the verdict stays `partial` on that gap and the box stays unticked until the count has a witness. The re-ask is exactly one call, and THAT half is witnessed: `TestSchemaReask_AsksOnceThenFails` counts the delegations and reddens on a second re-ask. Its ELIGIBILITY is the second gap — the two classes it serves (parse fallback, missing required field) and the type and enum violations refused without a re-ask answer to NO test: force the gate open and the whole producer package stays green, so an ineligible shape buying a call is the other owed witness beside rung 3's count. The re-ask's attempt is durable as `delegate_retry` / `delegate_finished` while no correction budget survives a restart. The engine's loop needs FOUR conditions (validation enabled, an executor implementing `runtime.OutputCorrector`, a positive budget, a correction store) and two of them have no production caller; it stays a seam, neither retired nor wired |
| 5 | An unchanged payload and unchanged violations cannot loop forever | the episode fingerprint comparison of the engine seam, [`pkg/runtime/node_output.go`](../pkg/runtime/node_output.go); the production ladder's rungs, [`pkg/backend/model/executor_verified_action.go`](../pkg/backend/model/executor_verified_action.go) | `TestSchemaValidation_UnchangedCorrectionStopsImmediately`, `TestSchemaValidation_UnchangedSemanticPayloadChargesFailedCorrection` | **partial**, on ONE named gap ([ADR-152](adr/152-the-reliability-invariants-have-owning-layers.md), decision 4): every production correction is BOUNDED by construction — the re-ask at one call, the ladder at its declared attempt budget, witnessed on rung 4 by `TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent` and on neither rung by `TestVerifiedAction_RecoverExhaustedFails`, which refuses a success after the rungs are exhausted and counts no attempt (the loop tripled, the whole producer package stays green) — so an unbounded loop is impossible in the code while the self-repair COUNT stays unwitnessed, and the ladder has no unchanged-fingerprint STOP: the self-repair rung carries the previous stdout, stderr and resolved command forward and compares no fingerprint, so an identical corrected command spends every attempt. The stop exists only on the engine seam of row 4 |
| 6 | A resume explicitly refuses incompatible artifacts and destroys no effect without authorisation | [`pkg/runtime/artifact_contract.go`](../pkg/runtime/artifact_contract.go) (`ValidateArtifactContractsPreflight`, `ValidateResumeArtifacts`) + `Service.PreflightResume` | `TestValidateArtifactContractsRefusesIncompatibleRevisionInEnforce`, `TestForcedArtifactCompatibilitySurvivesWorkflowRestamp` | met |
| 7 | A shared outage stops new waves without blocking a healthy scope | [`pkg/retrycoord`](../pkg/retrycoord) (durable circuit, tenant-scoped, keyed by workflow revision) | `TestSweepDueRetries_OpenCircuitDelaysWithoutSpendingAttempt`, `TestKeyPrefersWorkflowRevision`, `TestRetryCircuitFailureOpensAndSuccessCloses` | met on the DELAY and on the key's composition — an open circuit delays the due retry without spending an attempt, the key is derived from the workflow revision, and a circuit-store outage falls back to the per-run retry rather than dropping work; the ISOLATION half is witnessed by nothing outside Mongo, because every circuit-store double in the tree answers one circuit whatever the key, so collapsing the sweeper's key to a single constant keeps `pkg/server` and `pkg/retrycoord` green — an owed witness (two runs, two revisions, one open circuit). The durable half (`TestRetryCircuitFailureOpensAndSuccessCloses`) runs under `ITERION_TEST_MONGO_URI`, in CI's Mongo conformance job |
| 8 | Budgets and attempt identities survive checkpoints and restarts without double counting | [`pkg/runner`](../pkg/runner) usage-window retry | `TestArmUsageWindowRetry_ALegacyRowGainsItsWallWithoutASecondBudget`, `TestArmUsageWindowRetry_AWindowDurationWindowIsOneWall` | met |
| 9 | Watchers do not repeat the same action without new evidence | [`pkg/supervise`](../pkg/supervise) durable cursor per (run, supervisor, watched set) | `TestCoordinatorPersistsCursorAndSuppressesDuplicateWake`, `TestCoordinatorCursorLoadFailureStopsActionsAndWrites` | met — per launch surface: a coordinator persists only where one wired a cursor store, which is why the baseline reports `runs_with_watcher_cursors` per run instead of a global flag |
| 10 | The automated scenarios produce zero duplicated effect, zero unauthorised destructive resume and zero unbounded identical loop | per-lot suites across `pkg/runtime`, `pkg/runview`, `pkg/runner`; `pkg/botreplay` for node contracts | **no single suite** | **open** — the coverage exists lot by lot, but the tranche-8 scenario suite (launch, fan-out, crash, resume, rewind, quota, repair over simulated `NodeExecutor`s, in one place) does not exist; botreplay is deliberately not a graph simulator |
| 11 | The pilot report includes the BLOCKED admissions and compares cost, delay and interventions with the baseline | `reliability.Baseline` — `admissions_denied`, readable against `admissions_recorded`, with `legacy_context_runs` | `TestSummarizeCountsBlockedAdmissions`, `TestSummarizeDoesNotReadAnUnknownVerdictAsAllowed` | **partial** — the artifact can no longer hide a blocked admission (a denied run never executes and lands in `failed`, where the status counters cannot tell it apart from a run that failed on its own work); the comparison itself needs a pilot that has RUN |

### What the baseline leaves open, named

[ADR-152](adr/152-the-reliability-invariants-have-owning-layers.md) is the
record that rules on these: it maps every invariant to its owning layer and
its witness, and names the slices the tree still owes.

1. **The declared stores are inert, and not yet comparable.**
   `ExecutionContext.RunStore` and `BusinessStores` are filled by the launch
   authority and compared by nobody; `admitRun` compares the workflow
   revision, the lineage and the runner's expected context only — while its
   denial hint already says "align the run store". Adding the comparison
   needs a store identity that can diverge: `Root()` is empty for the Mongo
   store by design, so today's derivation is ONE constant for every Mongo
   database, and the kind comes from `%T`, which changes under a store
   decorator that changes nothing operationally. The escape hatch is a field
   with no reader (`ContextRef.Required`, stamped `true` over any caller
   declaration). So the seam comes first, then the comparison, and promoting
   it to a denial still belongs to the promotion decision rather than to
   this close-out.
2. **Correction is bounded at the producer, and one stop is missing.** Two
   production paths correct — the verified-action ladder under the author's
   declared budget, and the schema re-ask's single call for two eligible
   classes — which is why row 4 reads `partial` rather than open: the bound
   exists and half of it is witnessed. What the ladder does NOT have is a
   witness for the self-repair rung's attempt count: inflate that loop and
   the whole producer package stays green, so the count is one owed slice
   that keeps row 4 off the tick. The re-ask owes the other: its single call
   is witnessed, its ELIGIBILITY is not — force the gate open and the same
   package stays green. The re-ask's own attempt is durable
   (`delegate_retry`, `delegate_finished` with its mode and marginal cost);
   what neither producer keeps is a correction EPISODE with a budget that
   survives a restart.
   The engine's ledgered loop is a seam with no production implementor: it is
   neither retired (it is where a durable episode, a restart-surviving budget
   and the fingerprint stop already live) nor wired (that would reconcile
   three correction layers and bill a third generation). The residual is row
   5's named gap: the ladder has no unchanged-fingerprint stop.
3. **The scenario suite of row 10 was never written.** The per-lot tests are
   not a substitute for the end-to-end matrix the tranche names. It
   implements the `scenarioExecutor` stubs already in `e2e/`, and its rows
   are in [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md) as
   `uncovered` with the reason written.
4. **"Business stores" has no producer.** Outside tests the field appears
   only in its declaration, `Clone` and `Normalize`; the wire type is live
   and no DSL keyword or launch field fills it. Until a declaration surface
   exists no gate may read it, and inferring business stores from the files a
   run touches is what this design forbids.

Rows 1, 2, 6, 7, 8 and 9 are ticked on the ticket, each on the witness named
above; rows 1, 7 and 9 also name the qualifier their witness does not cover,
while rows 2, 6 and 8 read a bare `met`. Row 4 reads `partial` under
ADR-152, so its box stays unticked until the self-repair rung's attempt
count and the re-ask's eligibility have witnesses. Rows 3, 4, 5, 10 and 11
stay unticked, each with an owner and a slice: a criterion ticked on a path
nothing reaches is the false green this document's own compatibility rule
refuses, and a criterion left open with nothing behind it is the state the
record ends.
