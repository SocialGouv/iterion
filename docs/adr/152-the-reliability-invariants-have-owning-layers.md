# ADR-152: The reliability invariants have owning layers — one invariant, one owning layer, one witness named by TEST; the correction seam stays a seam and the rollout mode stays an operator floor

- **Status**: Accepted (2026-11-03) — on the clauses below, each backed by a measurement in this tree. What is named under *What is not promised* is **not** decided here: each axis is owed its own slice, taken when the work that needs it starts. On the shape [ADR-143](143-sandbox-read-back-contract.md) set and [ADR-144](144-shared-skills-are-bundle-dependencies.md) / [ADR-145](145-shared-fragments-are-bundle-dependencies.md) applied: decide what the tree backs, NAME what it does not, and list the owed slices with a witness each.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude Opus 5), on the ruling the debt-leap lane took for the five unticked acceptance criteria of [#1006](https://github.com/SocialGouv/iterion/issues/1006)
- **Landed by this change**: this record; rows 4 and 5 of the published ledger, restated on the bounds the tree actually ships and cited by test name ([workflow-reliability-1006.md](../workflow-reliability-1006.md)); the `reliability` family of [e2e-coverage-matrix.md](../e2e-coverage-matrix.md) with its true statuses, and the plan table that is derived from it ([e2e-stabilisation-plan.md](../e2e-stabilisation-plan.md)); the agents-tree pointers that owed it ([dsl-errors-resume.md](../agents/engine/dsl-errors-resume.md)). **No production code changes with it.** Owed by the slices behind it, each with its own witness: the scenario matrix (criterion 10), the unchanged-fingerprint stop of the verified-action ladder (criterion 5), the self-repair rung's attempt count (criterion 4), the store-identity seam and only then the admission comparison (criterion 3), the business-stores declaration surface, the pilot (criterion 11).
- **Relates to**: [ADR-044](044-adaptive-recovery-for-deterministic-action-nodes.md) (bounded repair at the producer — the decision this record CITES rather than retakes) · [ADR-132](132-a-run-reads-no-environment.md) and [#2699](https://github.com/SocialGouv/iterion/issues/2699) (the database level the rollout mode waits for) · [workflow-reliability-1006.md](../workflow-reliability-1006.md) (the published ledger and the three guards that keep it honest) · [#1403](https://github.com/SocialGouv/iterion/issues/1403) (the runtime epic the owed slices land under)

## Context

The close-out of #1006 published the ledger its eight tranches owed.
Those tranches landed in September 2026
([#1006](https://github.com/SocialGouv/iterion/issues/1006)), and the ledger
states, against the code rather than the plan, which of the eleven acceptance
criteria the tree meets: one row each, with the owner, the witness that
refuses the criterion's violation, and a verdict that is the measurement
rather than an intention. Six were met when it published; five were not, and
they have stayed that way because what each of them needs is a decision, not
a commit.

Three guards make that table self-policing: the row count is pinned at
eleven, every witness the ledger names must be a declared `func Test*`
somewhere in the tree, and every owner link must resolve
(`TestDivergenceLedgerCoversEveryAcceptanceCriterion`,
`TestDivergenceLedgerWitnessesExist`, `TestDivergenceLedgerLinksResolve`, all
in [`pkg/reliability`](../../pkg/reliability/divergence_ledger_test.go)).
They are why a verdict in that table can be trusted, and why dropping a row
to simplify it is not available.

What no record said until now is which layer OWNS each invariant. The
measurements that answer it:

- **Correction happens on three paths, not one.** The engine's ledgered
  correction loop (`correctAndValidateNodeOutput`,
  [`pkg/runtime/node_output.go`](../../pkg/runtime/node_output.go)) needs
  FOUR conditions at once: output validation enabled, an executor
  implementing `OutputCorrector` or `OutputCorrectorWithUsage`, a positive
  budget, and a store answering `store.AsOutputCorrectionStore`. Two of the
  four have no production caller at all — every call site of
  `WithOutputValidation(` is a `_test.go` file, beside its own definition in
  [`pkg/runtime/engine_options.go`](../../pkg/runtime/engine_options.go);
  the only `CorrectOutput` implementors are the two doubles in
  `pkg/runtime/validation_test.go`. The store half is NOT the blocker:
  Mongo implements it
  ([`pkg/store/mongo/output_correction.go`](../../pkg/store/mongo/output_correction.go)).
  Meanwhile the single production `NodeExecutor`, `ClawExecutor`, corrects
  twice over on its own: `validateAndRetry`
  ([`pkg/backend/model/executor_build_task.go`](../../pkg/backend/model/executor_build_task.go))
  re-asks the model ONCE and only for two eligible classes — a parse
  fallback or a missing required field — refusing type and enum violations
  with no re-ask, then falls back to one cross-backend extraction on the
  already-failing path, keeping no correction EPISODE: the attempt itself is
  durable — `delegate_retry` then `delegate_finished`, marked attempt 2 with
  its re-ask mode and priced at what it added — while no budget survives a
  process restart; and the verified-action ladder
  ([`pkg/backend/model/executor_verified_action.go`](../../pkg/backend/model/executor_verified_action.go))
  runs ADR-044's rungs under an operator-declared bound
  (`max_repair_attempts`, `max_agent_attempts`; under `policy: recover` the
  compiler ensures a spec exists and defaults to ONE self-repair attempt
  when the author left both bounds unset, agent recovery staying opt-in at
  zero), with the postcondition as the only success oracle and a durable
  `node_verified_action` event behind it. Rung 3 is reachable for a command
  or a script recipe only: a registry recipe has no corrected command to
  re-run, so `ir.ToolRecipeSelfRepairable` skips the rung, and a node that
  budgeted repairs alone on a bare tool name is warned C179 that no recovery
  rung is reachable at all.
- **The admission gate advertises a comparison it does not perform.**
  `admitRun` ([`pkg/runtime/admission.go`](../../pkg/runtime/admission.go))
  compares the normalisation error, the workflow revision against the run
  hash, the lineage, and the runner-expected revision/parent/policy triple.
  It names neither `RunStore` nor `BusinessStores` — yet its denial hint
  reads "align the run store, workflow revision and parent/workspace
  context", prose over ground the gate never reads.
- **The store identity is not comparable yet.** The only non-test derivation
  of a store `ContextRef` is inline in `ResolveExecutionContext`
  ([`pkg/runview/execution_context.go`](../../pkg/runview/execution_context.go)):
  `fmt.Sprintf("%T", runStore)` as the kind, `runStore.Root()` as the
  namespace, hashed by `store.StableContextID`. `Root()` returns the empty
  string for the Mongo store BY DESIGN
  ([`pkg/store/mongo/store.go`](../../pkg/store/mongo/store.go)), so that
  identity collapses to ONE constant for every Mongo database, URI, cluster
  and tenant — exactly the divergence criterion 3 is about. The kind, taken
  from `%T` and never unwrapped, varies under a decorator
  ([`pkg/dispatcher/heartbeat_store.go`](../../pkg/dispatcher/heartbeat_store.go)),
  although the unwrap seam exists
  ([`pkg/store/capability.go`](../../pkg/store/capability.go)). A comparison
  added today would be blind where the criterion aims and would deny where
  nothing diverged.
- **The escape hatch the bound would need does not exist.**
  `ContextRef.Required` has zero readers in the tree, and
  `ResolveExecutionContext` stamps `Required: true` unconditionally over
  whatever a caller declared — deliberately, so a forged declaration cannot
  become the value an enforce gate trusts. So "declared, not compared" is a
  field today, not a behaviour.
- **`BusinessStores` has no producer.** Outside tests the field appears only
  in its own declaration, in `Clone` and in `Normalize`
  ([`pkg/store/execution_context.go`](../../pkg/store/execution_context.go));
  the wire surface is live (`openapi.json`,
  [`studio/src/api/runs/types.ts`](../../studio/src/api/runs/types.ts)). No
  DSL keyword and no launch field fills it, and the ticket's own principle
  forbids inferring business stores from the files a run touches.
- **The rollout mode is a floor because its level does not exist.**
  `ITERION_RELIABILITY_MODE` resolves in tiers with the process environment
  as a documented floor and `=legacy` as a cap
  ([`pkg/reliability/rollout.go`](../../pkg/reliability/rollout.go)); the
  database level above it is ADR-132 / #2699 territory. The variable is
  operator-grade refused to bots and to `.env` files
  ([`internal/envtrust/operator.go`](../../internal/envtrust/operator.go)),
  so no repo-provided environment can set it.
- **The repo's own coverage inventory had no reliability row.**
  `docs/e2e-coverage-matrix.md` is the single feature×coverage inventory, and
  it carried no `reliability.*`, `runtime.admission` or execution-context
  row: criterion 10's gap was visible only inside #1006's own document.

## Decision

1. **One invariant, one owning layer, one witness — and the witness is named
   by TEST, never by line.** A criterion is `met` only when a named test
   refuses its violation; anything else is `partial` or `open` and says what
   is missing. Line citations are banned from the ledger and from this
   record: concurrent branches edit
   `pkg/backend/model/executor_build_task.go` by the dozen, so a `:978` is
   wrong after the next landing train and no guard says so, while a test
   name and an owner link are both pinned by the ledger's guards. What those guards
   pin is EXISTENCE: `TestDivergenceLedgerWitnessesExist` reads declarations,
   never assertions, so a name that refuses nothing passes it. A citation in
   this record is therefore chosen by MUTATION — the test named is one that
   reddens when the clause it covers is broken — and a clause no test can
   observe is written as owed, not as covered. A verdict answers the
   criterion's SUBJECT: a qualifier the code honours and no test refuses is
   named in its own row as an owed witness and leaves the verdict scoped
   (`met, for the contract the gate owns`), while a SUBJECT with no witness
   makes the criterion `partial`. That is why criterion 4 — "corrected at its
   producer within a defined LIMIT" — cannot read `met` while rung 3's attempt
   count answers to no test, and why criteria 1 and 7 stay `met` with their
   unwitnessed qualifier named in the row rather than implied.

2. **The ownership table is the record.** One row per invariant, the layer
   that owns it, the witness, and what remains:

   | Invariant | Owning layer | Witness (test) | State |
   |---|---|---|---|
   | A missing prerequisite is diagnosed before any model call | `pkg/runtime` admission gate, at launch and at resume | `TestAdmissionEnforceDeniesBeforeExecution`, `TestAdmissionResumeDenialDoesNotDestroyResumableRun`, `TestAdmissionLegacyRunIsRecordedAndAllowed` | met for the refusal, its durability and the resumable run it leaves intact; the BEFORE is the call site's order (`engine_run.go`, `resume.go`) and no test observes a node that did not run — an S slice |
   | One structured cause across CLI, Studio and Copi | `pkg/runview` versioned projection | `TestBuildDiagnostic_ProjectsNestedNodeOutputDetail`, `TestBuildDiagnostic_ProjectsResumableFailureAndRetryEvidence`, `TestSnapshotBuilder_IncludesTheSameDiagnosticProjection` | met on the projection's single source — its two producers agree on the version, the node, the outcome, the next action and the recoverability — while no test enters through the CLI, Studio or Copi: a per-surface witness is an owed qualifier |
   | A deliberately isolated subbot is accepted | `pkg/runner` subbot resolution: the child keeps the parent's policy and store declarations, its workflow revision and lineage being authority-owned | none named — the resolution is exercised by the subbot suites and no test refuses the violation | unwitnessed — the code does it, no test refuses the violation: an S slice |
   | An UNDECLARED store divergence is blocked before fan-out | `pkg/store` run-store identity, then the admission gate | none | open — owed the seam first (decision 6) |
   | An eligible invalid output is corrected at its producer within a defined limit | `pkg/backend/model`: the verified-action ladder for a `tool` node whose recipe is a command or a script, the schema re-ask for model nodes | `TestVerifiedAction_SelfRepair`, `TestVerifiedAction_OnlyARepairableRecipeEntersTheSelfRepairRung`, `TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent`, `TestVerifiedAction_RecoverExhaustedFails`, `TestValidateAndRetry_InjectsSchemaFeedback`, `TestValidateAndRetry_BooksTheClawRecoveryCall` | partial — rung 4's declared attempt is witnessed and so is the re-ask's single call, rung 3's generation count is not and neither is the re-ask's eligibility gate (decision 3): the criterion's subject IS the limit, so two unwitnessed halves of it keep the row off `met`; two S slices |
   | An unchanged payload and unchanged violations cannot loop | the same ladder; the fingerprint comparison of the engine seam | `TestSchemaValidation_UnchangedCorrectionStopsImmediately` (engine seam only) | partial — the named gap of decision 4 |
   | A resume refuses incompatible artifacts and destroys no effect without authorisation | `pkg/runtime` artifact contracts | `TestValidateArtifactContractsRefusesIncompatibleRevisionInEnforce`, `TestForcedArtifactCompatibilitySurvivesWorkflowRestamp` | met |
   | A shared outage stops new waves without blocking a healthy scope | `pkg/retrycoord` durable circuit, swept by `pkg/server` | `TestSweepDueRetries_OpenCircuitDelaysWithoutSpendingAttempt`, `TestKeyPrefersWorkflowRevision` | met on the delay and on the key's composition; the healthy-scope half is unwitnessed — every circuit-store double answers one circuit whatever the key, so collapsing the sweeper's key keeps the package green — an S slice |
   | Budgets and attempt identities survive checkpoints and restarts | `pkg/runner` usage-window retry wall | `TestArmUsageWindowRetry_ALegacyRowGainsItsWallWithoutASecondBudget` | met |
   | Watchers do not repeat an action without new evidence | `pkg/supervise` durable cursor | `TestCoordinatorPersistsCursorAndSuppressesDuplicateWake`, `TestCoordinatorCursorLoadFailureStopsActionsAndWrites` | met |
   | The automated scenarios produce zero duplicated effect, zero unauthorised destructive resume, zero unbounded identical loop | `e2e/` over the existing `scenarioExecutor` stubs | none | open — the suite is a named slice |
   | The pilot report counts blocked admissions and compares with the baseline | `pkg/reliability` baseline; the deployment for the run | `TestSummarizeCountsBlockedAdmissions`, `TestSummarizeDoesNotReadAnUnknownVerdictAsAllowed` | partial — the artifact ships, the comparison needs a pilot that RAN |
   | The rollout mode is an operator floor with an emergency cap | `pkg/reliability` resolver; `internal/envtrust` refuses it to bots and `.env` | `TestResolveKeepsTheRollbackLeverAgainstALevel`, `TestTheAliasIsNotTheRollbackLever` | met, until #2699 adds the level |

3. **Criterion 4 is PARTIAL on TWO named gaps inside ADR-044's declared
   limit — this record cites that decision, it does not retake it.** The bound
   an operator declares (`max_repair_attempts`, `max_agent_attempts`) with the
   postcondition as the only oracle IS "corrected at its producer within a
   defined limit", and it is accepted, implemented and witnessed where
   dispatch can reach it: rung 3 only for a recipe a corrected command can
   replace (`ir.ToolRecipeSelfRepairable` — a registry recipe that budgets
   repairs ALONE reaches no rung and is warned C179; with
   `max_agent_attempts` budgeted the same recipe reaches rung 4, which takes
   the recipe as it finds it, and C179 stays silent, as
   `TestVerifiedAction_C179AgentRungBudgeted` pins), and rung 4's declared
   attempt spent exactly once under
   `TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent`. One bound of that
   ladder is witnessed by NOTHING: no test counts the self-repair
   generations, so an inflated `max_repair_attempts` loop keeps the whole
   producer package green. That witness is an owed slice, named with the
   others, and until it exists the criterion reads `partial` and its box
   stays unticked — the subject of criterion 4 is the LIMIT, and half of the
   limit answers to no test. The ledger says exactly that instead of
   implying the count is pinned. For model nodes the second producer bound
   is the schema re-ask, and it splits the same way: the single call is
   witnessed — `TestSchemaReask_AsksOnceThenFails` counts the delegations
   and reddens when a second re-ask fires — while the ELIGIBILITY that
   serves two classes and refuses a type or an enum violation without a
   re-ask answers to NO test: force the gate open and the whole producer
   package stays green, the only test feeding a type mismatch asserting the
   bill and never the call count. So the ledger writes that half as owed,
   not as covered, and it is this criterion's second S slice. Reading rows
   4 and 5 as "nothing corrects" was the error this
   record closes: two production paths correct, one of them under a bound
   the author chose.

4. **Criterion 5 is PARTIAL on ONE named gap: the verified-action ladder is
   bounded by a budget and has no unchanged-fingerprint stop.** The
   self-repair rung carries the previous stdout, stderr and resolved command
   forward into the next proposal and compares no fingerprint, so a model
   that returns the same corrected command spends every attempt before the
   policy rung fails the node. The engine seam HAS that stop and no
   production path reaches it. The gap is a slice with its own witness —
   an identical proposal must stop the ladder at the second rung — not a
   tick, and not a licence to call the criterion met.

5. **The engine's `OutputCorrector` seam stays a SEAM: neither retired nor
   wired.** It is the documented hatch for a schema-level corrector, and the
   modularity rule is what keeps it: an executor that can correct implements
   the interface rather than earning an engine `if`. Wiring `ClawExecutor`
   into it today would mean reconciling THREE correction layers, not two —
   the ladder, the re-ask and the engine loop — and every schema-invalid
   node would pay a third generation. Retiring it would delete the only
   place where a durable correction episode, a budget that survives a
   restart and a fingerprint stop already exist.

6. **Criterion 3's second half is an owed SEAM, not a guard, and no guard
   lands before it.** A comparable store identity belongs on the `RunStore`
   seam itself — a store declares its own identity, probed through
   `store.capability`/`Unwrapper` so a decorator cannot change the answer,
   implemented by the filesystem store from its root and by the Mongo store
   from host and database name: identities only, never the URI credentials,
   which `ExecutionContext` already forbids carrying. Until that exists,
   there is nothing honest to compare: a test built on today's derivation
   would compare a constant with itself on the very path the criterion aims
   at — a run stamped by the server and executed by a runner pod — and would
   be green while proving nothing. When the comparison does land it owes
   THREE things, not one: the seam, a reader of `ContextRef.Required` in
   `admitRun`, and `ResolveExecutionContext` ceasing to force `Required:
   true` over a caller's declaration. The published close-out's ruling
   stands: promoting this to a denial belongs to the promotion decision.

7. **The business-stores declaration surface is named, not retired.** The
   field and its wire type stay — a typed surface Studio already reads, and
   the shape any declaration will serialise into — and the declaration site
   is a named slice: a workflow or launch declares the business stores it
   expects, the engine never infers them from files. The bound: until a
   producer exists, no gate may read `BusinessStores`, and criterion 3's
   business-store half is not claimable. If the promotion decision arrives
   with that slice still untaken, the half is retired rather than left open —
   an unproduced field is not an open criterion, it is a field.

8. **`ITERION_RELIABILITY_MODE` remains the operator's floor and emergency
   cap.** It is refused to bots and to `.env` files by the operator-grade
   inventory, its `=legacy` value is a cap no level overrules, and `report`
   is the hatch: admission decisions are recorded and surfaced while not one
   run is refused. Its database level is owed to #2699, not to this record.
   The pilot's prod gesture is the operator's own and needs zero code — one
   `config.extraEnv` line reaches the server, the runner and the runner pool
   through the chart's existing passthrough.

9. **Criterion 10's suite implements the existing seam, and its coverage is
   inventoried where every other family's is.** The scenario matrix drives
   `scenarioExecutor` stubs in `e2e/`, which the gate every session owes
   already runs; `botreplay` stays out, being deliberately not a graph
   simulator. The `reliability` family lands in
   `docs/e2e-coverage-matrix.md` with its TRUE statuses, so the gap is
   counted by the repository's own inventory and not only by #1006's
   document.

## Rejected alternatives

- **Wire `ClawExecutor` to `runtime.OutputCorrector` now.** It closes
  criteria 4 and 5 on their own terms and is the right end state for a node
  whose correction must survive a pod restart. Refused for this record: it
  is a production cost and latency change (a third billed generation on the
  failing path), and it needs the two in-executor paths subordinated or
  stood down first. Filed as a slice, not decided by a doc.
- **Retire criteria 4 and 5 as describing a capability the product does not
  ship** (the option the close-out itself offered). Refused: it is false.
  Two production paths correct, one under an operator-declared bound in an
  accepted ADR. Retiring them would publish a weaker claim than the tree
  deserves and would lose the real residual — the missing fingerprint stop.
- **Add the store comparison to `admitRun` in this change.** Refused on
  three counts: the value is a constant for every Mongo store, it varies
  under a decorator that changes nothing operationally, and the escape hatch
  it would need has no reader. A bound with a no-op hatch is the artificial
  limit the philosophy refuses.
- **Extract the `%T` + `Root()` derivation into a shared helper and compare
  that.** Refused: a helper around a value that cannot distinguish two Mongo
  databases moves the defect to one place instead of fixing it, and it would
  make the comparison look like a prerequisite that was satisfied.
- **A separate reliability coverage document.** Refused: views are additive.
  The matrix is the inventory; a second table would be a second source of
  truth, and the guard that pins the plan to the matrix would not see it.
- **Raise the agents-tree byte budget to fit the payback.** Refused: the
  remedy for a page at its ceiling is tighter prose or a page of its own.

## What is not promised

- **No exactly-once for external effects.** The ticket's own principle; the
  scenario matrix asserts "zero duplicated effect" over the paths it drives,
  which is not the same promise.
- **The shape of the unchanged-fingerprint stop.** What the fingerprint
  covers (the corrected command, its output, the postcondition's own output)
  and what the stop does (fail immediately, or skip to the next rung)
  belongs to its slice, with a witness that reddens on an identical
  proposal.
- **The migration of the store identity.** Giving stores a real identity
  changes every `ContextRef.ID` already persisted. Whether the comparison
  tolerates a legacy-shaped ref or the context version bumps is that
  slice's decision.
- **Whether the engine seam is ever wired.** Decision 5 keeps it a seam; it
  does not promise an implementor, and it does not promise the seam survives
  a second implementor's arrival unchanged.
- **The database level of the rollout mode.** #2699 and ADR-132 own it. This
  record is their citation until they absorb it.
- **That the pilot runs.** Three criteria depend on a deployment gesture no
  agent can take.

## Consequences

- **Five criteria stay unticked, with seven named slices between them**
  (criteria 3 and 4 own two each), and the orchestrator files them as
  sub-issues under #1403: the scenario matrix, the unchanged-fingerprint
  stop, the self-repair rung's attempt count, the re-ask's eligibility gate,
  the store-identity seam (then the comparison), the business-stores
  declaration, and the pilot. Four qualifiers the code honours are
  unwitnessed and file with them: nothing asserts that a subbot child keeps
  the parent's policy and store declarations, nothing observes a node that
  did NOT run when admission denied it, nothing refuses a circuit that holds
  back a healthy scope, and no test reads the diagnostic projection through
  the CLI, Studio or Copi. The table above records each as unwitnessed
  rather than borrowing the suites that merely cross that code.
  An unticked box with a named slice is a tracked item; an unticked box with
  nothing behind it is the state this record ends.
- **A scope promoted to `enforce` can deny a run that was legitimately
  migrated between stores.** The walk-back is the policy ladder — lower the
  scope to `report`, where decisions are recorded and nothing is refused —
  and the hatch must be BUILT before the bound, not cited.
- **The ledger's rows 4 and 5 now state the shipped bounds** — each half
  either cited by test name or written as owed — and the three guards pin
  the citations: a renamed witness reddens `pkg/reliability` rather than
  leaving published prose standing over nothing.
- **The coverage inventory grows a `reliability` family**, so the
  stabilisation plan's measured table is re-derived with it; that table is
  pinned to the matrix by its own guard, in both directions.
- **A session that meets a torn checkpoint finds the ledger** from the
  agents tree instead of from this record: the engine page carries the
  pointers, inside its byte budget.
- **This record is cited, not copied.** ADR-044 keeps bounded repair at the
  producer, ADR-132 and #2699 keep the mode's database level; what belongs
  here is only the ownership map and the five rulings above.
