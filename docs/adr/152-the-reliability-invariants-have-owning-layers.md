# ADR-152: The reliability invariants have owning layers — one invariant, one owning layer, one witness named by TEST; the engine's correction seam is wired to the producer and the rollout mode stays an operator floor

- **Status**: Accepted (2026-11-03), decision 5 amended the same day on the operator's verdict — on the clauses below, each backed by a measurement in this tree. What is named under *What is not promised* is **not** decided here: each axis is owed its own slice, taken when the work that needs it starts. On the shape [ADR-143](143-sandbox-read-back-contract.md) set and [ADR-144](144-shared-skills-are-bundle-dependencies.md) / [ADR-145](145-shared-fragments-are-bundle-dependencies.md) applied: decide what the tree backs, NAME what it does not, and list the owed slices with a witness each.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude Opus 5), on the ruling the debt-leap lane took for the five unticked acceptance criteria of [#1006](https://github.com/SocialGouv/iterion/issues/1006); decision 5 amended by devthejo (session with Claude Opus 5.5) on the operator's verdict of 2026-11-03
- **Landed by this change**: this record; rows 4 and 5 of the published ledger, restated on the bounds the tree actually ships and cited by test name ([workflow-reliability-1006.md](../workflow-reliability-1006.md)); the `reliability` family of [e2e-coverage-matrix.md](../e2e-coverage-matrix.md) with its true statuses, and the plan table that is derived from it ([e2e-stabilisation-plan.md](../e2e-stabilisation-plan.md)); the agents-tree pointers that owed it ([dsl-errors-resume.md](../agents/engine/dsl-errors-resume.md)). **No production code changes with it.** Owed by the slices behind it, each with its own witness: the scenario matrix (criterion 10), `ClawExecutor` wired as the engine's `OutputCorrector` with the re-ask's eligibility witness (criteria 4 and 5, decision 5), the unchanged-proposal stop of the verified-action ladder with the self-repair rung's attempt-count witness (criteria 5 and 4), the store-identity seam and only then the admission comparison (criterion 3), the business-stores declaration surface, the pilot (criterion 11).
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
- **Behind the producer's round stands a node RETRY, which is not a
  correction.** The in-place round gives up as an error whose wording no
  classifier knows (`structured output invalid…`, `schemaReaskFailure` in
  [`pkg/backend/model/executor_reask.go`](../../pkg/backend/model/executor_reask.go)).
  In the trunk, `execLoopRunNode` hands it to `handleNodeFailure`
  ([`pkg/runtime/recovery_dispatch.go`](../../pkg/runtime/recovery_dispatch.go));
  `recovery.Classify` matches no type and no needle and falls through to
  `ErrCodeExecutionFailed`, which `recovery.DefaultRecipes()` maps to
  `ExecutionFailedRecipe(1)`: re-execute the node once, then surface it as
  `failed_resumable`. Every production launch path wires that dispatcher
  (`pkg/cli/run.go`, `pkg/dispatcher/engine_runner.go`,
  `pkg/dispatcher/subbot.go`, `pkg/runner/loop.go`, `pkg/runner/subbot.go`,
  and `pkg/runview`'s service). The retry is a fresh execution — a fresh
  first answer and a fresh in-place round — so a trunk node whose output
  stays invalid pays up to SIX generations (twice the first answer, the
  re-ask and the extraction) and ends `EXECUTION_FAILED`, which
  `pkg/retrypolicy` classifies `DispositionTransient`: on the CLI
  `--auto-resume` allow-list. A fan-out branch and `reInvokeBackend`
  dispatch no recovery, so they pay one execution's three. The engine's own
  schema failure is another code: `validateNodeOutput` raises
  `SCHEMA_VALIDATION`, `DispositionReexecutable`, and `execLoopAfterExec`
  fails the run with it without consulting the dispatcher.
- **What wiring the seam has to cross**, measured when the operator ruled
  on decision 5. A `ClawExecutor` schema failure leaves `Execute` as an
  error, and the engine enters `correctAndValidateNodeOutput` only on an
  output an execution RETURNED — after `execLoopRunNode` in the trunk, after
  `executeWithResources` in a fan-out branch — so an executor that merely
  implemented the interface would never be called. The executor the engine
  holds is the producer itself: `runview.BuildExecutor` returns a
  `*model.ClawExecutor`, which every launch path hands unwrapped to
  `runtime.New`, while `botreplay`'s recorder calls `Execute` with no engine
  at all. The seam's usage type, `runtime.OutputCorrectionUsage`, is
  declared in `pkg/runtime`, which imports `pkg/backend/model`: as declared,
  the producer cannot name it without an import cycle. The in-place path
  already pays a third billed generation on its extraction exit (the first
  answer, the re-ask, the claw extraction), and the engine's default budget
  of two (`defaultOutputCorrectionBudget`) has never bought a production
  generation, no production executor implementing the seam.
  `validateAndRetry` has TWO production callers, not one. `executeBackend`
  validates an agent's or a judge's answer against its declared output
  schema (`f.outputSchema`, the name `ir.NodeOutputSchema` gives the
  engine). `executeLLMRouterUnified`
  ([`pkg/backend/model/executor_router.go`](../../pkg/backend/model/executor_router.go))
  validates an LLM router's selection against a schema it synthesises per
  call from the candidates (`buildRouterSchema`), with "the same single
  re-ask an agent's schema failure gets". The engine holds no schema for
  that selection — a `RouterNode` is not an `ir.LLMNode`, so
  `ir.NodeOutputSchema` answers the empty string — and a trunk LLM router is
  special-dispatched: `execLLMRouter`
  ([`pkg/runtime/routing.go`](../../pkg/runtime/routing.go)) books a failed
  router's spend and fails it without reaching
  `correctAndValidateNodeOutput`. A human node's LLM half re-asks nothing: it
  generates through `GenerateObjectDirect`, never through `validateAndRetry`.
  The seam's own arguments do not carry a round either.
  `CorrectOutputWithUsage` receives the node, the invalid output and the
  violation, while a round needs what produced the first answer: the serving
  task (its prompt, rendered from the node input), the serving backend, the
  delegate result a continuation reads (`planSchemaReask`,
  `completedClawConversation`) and the schema. The output map keeps only
  metadata keys of that result (`stampDelegateOutputMeta`), and the
  branches of one `fan_out_each` node run the same node concurrently, so a
  cache keyed by node would hand one branch another's conversation.
  Nor does the helper run where the execution ran. The trunk builds the
  node's execution context — `execContext`, which stamps the run ID, the
  node ID and the runner-home scope — inside `execLoopRunNode`, and releases
  the node's `needs:` lease when that function returns; `execLoopAfterExec`
  receives the loop's own context and calls the helper with it, after the
  release, and `reInvokeBackend` likewise passes its `ctx`, not its
  `execCtx`. A claw re-ask continues its conversation only where the context
  names the run: `completedClawConversation` reads it through
  `runtimeContextFrom`, and with no run ID `planSchemaReask` falls to
  `ReaskRestart`. Today's in-place round runs inside `Execute`, under the
  execution context and inside the lease, so a trunk claw agent continues
  tool-less; a fan-out branch carries no run identity by design (the
  `execContext` doc), so a branch's re-ask restarts today already.
  `WithOutputValidation` applies to EVERY node kind that declares an output
  schema (`ir.NodeOutputSchema`: LLM, human, tool, compute, subbot, wait),
  where production validates an LLM node's output in the producer and
  conforms a compute's in its own dispatch (`model.ConformComputeOutput`).
  And the seam's own contract (`OutputCorrector` in
  [`pkg/runtime/engine.go`](../../pkg/runtime/engine.go)) — a corrector
  "must not publish external side effects" — does not fit the producer's
  round as written: only `ReaskContinueConversation` turns the task's tools
  off, while `ReaskResumeSession` resumes the session with the tools the
  node declares and `ReaskRestart` re-runs the turn with them.
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
   | An eligible invalid output is corrected at its producer within a defined limit | `pkg/backend/model`: the verified-action ladder for a `tool` node whose recipe is a command or a script; for model nodes the schema re-ask today, and once decision 5's unit lands the `pkg/runtime` correction episode for an agent or a judge, its round generated by `ClawExecutor`, while an LLM router keeps its single re-ask | `TestVerifiedAction_SelfRepair`, `TestVerifiedAction_OnlyARepairableRecipeEntersTheSelfRepairRung`, `TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent`, `TestVerifiedAction_RecoverExhaustedFails`, `TestValidateAndRetry_InjectsSchemaFeedback`, `TestValidateAndRetry_BooksTheClawRecoveryCall` | partial — rung 4's declared attempt is witnessed and so is the re-ask's single call, rung 3's generation count is not and neither is the re-ask's eligibility gate (decision 3): the criterion's subject IS the limit, so two unwitnessed halves of it keep the row off `met`; the count is owed with the ladder's stop, the eligibility with decision 5's unit |
   | An unchanged payload and unchanged violations cannot loop | the same ladder, for its own stop; for agents and judges the engine episode's fingerprint comparison, which decision 5 puts on the production path (an LLM router's single re-ask cannot loop) | `TestSchemaValidation_UnchangedCorrectionStopsImmediately` (engine seam only) | partial — the named gap of decision 4, and the model-node half until decision 5's unit lands |
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
   producer package green. That witness is owed with the ladder's
   unchanged-proposal stop (decision 4), whose own witness cannot tell a
   stop from a loop that ignores the declared budget without it; until it
   exists the criterion reads `partial` and its box
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
   not as covered, and its witness lands with decision 5's unit, where the
   eligibility becomes the corrector's answer to the engine. Reading rows
   4 and 5 as "nothing corrects" was the error this
   record closes: two production paths correct, one of them under a bound
   the author chose.

4. **Criterion 5 is PARTIAL on ONE named gap: the verified-action ladder is
   bounded by a budget and has no unchanged-fingerprint stop.** The
   self-repair rung carries the previous stdout, stderr and resolved command
   forward into the next proposal and compares no fingerprint, so a model
   that returns the same corrected command spends every attempt before the
   policy rung fails the node. The engine seam HAS that stop and no
   production path reaches it today; decision 5 brings agents and judges to it,
   and it does not bring the ladder: the ladder answers another failure
   class — an unmet postcondition, judged by the postcondition rather than
   by a schema — under bounds the workflow author declares per node, where
   the episode validates a candidate against a schema under an engine-wide
   budget. So the ladder keeps its own stop. The gap is a slice with its own witness — an
   identical proposal must stop the ladder at the second rung, and distinct
   proposals must spend exactly the declared attempts, the count decision 3
   owes — not a tick, and not a licence to call the criterion met.

5. **The engine's `OutputCorrector` seam is WIRED: `ClawExecutor` becomes
   its production implementor, and a schema-invalid model output has ONE
   correction owner, the engine's episode.** This record first decided the
   opposite — the seam kept as a seam, neither retired nor wired, the wiring
   refused for its cost. The operator decided on 2026-11-03 to wire it, and
   the record follows that verdict for the reason it gave for keeping the
   seam: the engine's loop is the one place where a durable episode, a
   budget a restart does not reset and a fingerprint stop already exist, so
   bringing the production producer to it answers criteria 4 and 5 for
   model nodes instead of rebuilding all three inside the producer. The
   earlier text stands under *Rejected alternatives*, with the verdict as
   the reason it lost. No code ships with this amendment: the wiring is one
   unit, owed with its witness. The reconciliation, read from the code
   measured under *Context*:

   | Failure class | Owner | Bound | Persisted |
   |---|---|---|---|
   | an agent's or a judge's output fails its declared output schema on an ELIGIBLE shape (a parse fallback, a missing required field) | the `pkg/runtime` episode (`correctAndValidateNodeOutput`), its round generated by `ClawExecutor` | the engine's budget, counted in rounds, ONE by default | the `OutputCorrectionEpisode` on the run document |
   | an LLM router's selection fails the schema synthesised from its candidates, on an eligible shape | `ClawExecutor`'s in-place round, as today — the engine holds no schema for the selection | one round by construction, outside the engine's budget | its `delegate_retry` / `delegate_finished` events, no episode |
   | either output fails on an ineligible shape (a type or an enum violation) | none — the node fails, nothing is bought, as today | — | nothing |
   | a provider error handed back as the answer (`API Error:`) | none — named as the route's failure, as today | — | nothing |
   | a `tool` node's postcondition stays unmet | ADR-044's ladder, untouched | `max_repair_attempts` / `max_agent_attempts` | `node_verified_action` |
   | a transient delegate error | `retryDelegateLoop`, which is a retry, not a correction | its backoff budget | its `delegate_retry` events |
   | a trunk node's episode ends without a valid output (`exhausted`, `unchanged`) | the engine's recovery dispatch, as today — a retry of the node, not a correction: `ExecutionFailedRecipe(1)` re-executes it once, with a fresh first answer and NO round | the recipe's one retry, outside the engine's budget | its `node_recovery` event (code `EXECUTION_FAILED`), the episode left terminal |

   - **No stacked layers.** The engine announces that it owns correction
     through the type-asserted optional extension it already uses to push
     launch-time state into its executor (`workDirSetter` and its siblings),
     and only when its store answers `store.AsOutputCorrectionStore`. The
     announcement is engine-wide; the stand-down is per CALL SITE: it holds
     only where the engine validates the same schema the producer does —
     `executeBackend`, whose schema is the node's declared output, the one
     `ir.NodeOutputSchema` names. There `ClawExecutor` does not correct in
     place: an eligible schema failure leaves `Execute` as a TYPED failure,
     declared on the producer's side and read by the engine with
     `errors.As` — the fact travels with the failure, it is never
     re-derived from a message. An LLM router keeps its in-place round under
     an owning engine as without one: standing it down would leave its
     failure with no owner at all, since the engine neither validates its
     selection nor routes its error to the helper. A caller with no engine —
     `botreplay`'s recorder — keeps the in-place round too, and every path
     runs ONE round function, so they cannot drift. A human node's LLM half
     raises no typed failure, having no re-ask to stand down, so its error
     path — the human pause `execAutoOrPauseHuman` falls back to — is
     today's.
   - **The typed failure carries the round, and the engine hands it back.**
     The failure holds the invalid payload and the round's HANDLE: the
     serving task, the serving backend, the delegate result a continuation
     reads, and the schema. The engine treats the handle as opaque and
     attaches it to the context of each round it opens, through an accessor
     the producer declares; the round advances it in place, so a second
     round continues from the first round's answer. One handle per
     execution, never a cache keyed by node: two `fan_out_each` branches of
     one node each correct their own conversation. A handle lives as long
     as the execution that raised it and is never persisted — a resumed
     node re-executes and raises a fresh one. An invalid output that
     `ClawExecutor` merely RETURNED — reachable only under
     `WithOutputValidation`, for a node kind it does not re-ask — carries no
     handle, so it is not eligible and opens no episode.
   - **The episode opens where the execution ran.** The sites that read the
     typed failure are the ones whose node can raise it, and each opens the
     episode in its own ERROR branch, through `correctAndValidateNodeOutput`,
     with the execution's context rather than the loop's: the trunk in
     `execLoopRunNode`, under its `execCtx` and inside the node's `needs:`
     lease, BEFORE `handleNodeFailure`; the fan-out branch in `branch.go`,
     under its own `execCtx` and lease; `reInvokeBackend` after its
     `ErrNeedsInteraction` check, under its `execCtx`. Each round crosses
     the gate `executeWithResources` holds for an `Execute` call, and
     promotes the run ID into the producer's runtime context exactly as
     `Execute` does. So a trunk claw agent's round continues its
     conversation with its tools off, as today's in-place round does, and a
     branch's round plans as a branch's re-ask does today. An episode that
     succeeds hands its output on as a returned output: the helper
     `execLoopAfterExec` already calls validates it again and opens no
     second episode.
   - **A terminal episode goes back to the recovery dispatch, unchanged.**
     In the trunk, an episode that settles `exhausted` or `unchanged` hands
     `handleNodeFailure` the ORIGINAL typed failure. Its message stays
     today's and it is no `RuntimeError`, so `recovery.Classify` still
     answers `EXECUTION_FAILED`, `ExecutionFailedRecipe(1)` still
     re-executes the node once, and a node that fails again ends
     `EXECUTION_FAILED` with today's disposition and CLI auto-resume
     behaviour. The retry draws a fresh first answer and buys NO round: its
     ledger key is the invocation's, which a retry derives again on purpose
     — "Output can change across a retry/resume and therefore must never
     reset a consumed budget" (`correctAndValidateNodeOutput`) — so the
     terminal episode returns its violation at once. A `spend_blocked`
     episode hands the spend stop instead (`BUDGET_EXCEEDED`, which
     `BudgetRecipe` pauses for a raised cap). A trunk node whose output
     stays invalid therefore pays up to FOUR generations where it pays six
     today; a branch or a `reInvokeBackend` node pays today's three.
   - **The engine derives the violation; it never adopts the producer's.**
     The episode's first violation is the engine's validation of the typed
     failure's payload against the node's schema, not the producer's
     message, so the fingerprint stop compares two violations one validator
     wrote: a round that returns the same payload settles `unchanged`, a
     different payload that still fails settles `exhausted` once the
     budget is spent (`spend_blocked` when the spend check stops it
     first), and only an `exhausted` episode re-opens on a raised
     budget.
   - **A round, not a generation, is what the budget counts.** A round is
     what `validateAndRetry` does after the first answer: one re-ask in the
     conversation or session that answer ran in (`planSchemaReask`, falling
     to `ReaskRestart` where nothing can be continued) and, only when that
     re-ask errors or still answers unstructured, one claw extraction
     (`extractStructuredViaClaw`). Counted per generation, a re-ask that
     returns the same unstructured text would trip the fingerprint stop
     before the extraction ran, on the very backend the extraction exists
     for. The engine's process default of two has never bought a production
     generation, and the unit lowers it to ONE round: one execution pays
     what today's in-place round pays, two correction generations at most on
     top of its own answer. What falls is the recovery retry's second round
     (above). The cost this record first feared was never that
     third generation, which the in-place path already bills on its
     extraction exit; it was a second loop stacked over the first.
   - **Eligibility is the producer's answer, asked before a round is
     counted.** Which shape another ask can fix is the producer's knowledge
     (`retryEligible`: a parse fallback or a missing required field). The
     engine asks before it opens an attempt, so an ineligible violation
     opens no episode, buys no generation and fails the node exactly as it
     does today. The witness decision 3 owes for that gate lands with the
     unit.
   - **The bound and its hatch.** The budget stays the engine's:
     `ITERION_OUTPUT_CORRECTION_BUDGET`, operator-grade and refused to bots
     and to `.env` files, sets the process default, and
     `WithOutputCorrectionBudget` overrides it per engine. `0` stands every
     correction the engine owns down, an agent's or a judge's re-ask
     included, and leaves the LLM router's in-place round as it is; a raised budget re-opens an
     exhausted episode (the `budget_raised` phase). Each round passes the
     engine's spend check first (`outputCorrectionSpendError`, at
     `budgetHardThreshold`, where the engine refuses a new node execution), so a run at its spend edge settles
     `spend_blocked` instead of buying one. The recovery retry is not this
     budget's: it keeps the recipe's own bound, which a host changes in the
     recipe map it hands `WithRecoveryDispatch`, as it can today.
   - **The seam's contract is amended, not bypassed.** The unit rewrites
     the `OutputCorrector` doc: a corrector may re-run the node's own turn,
     with the effects the node declares (its tools, its sandbox) — the
     effects a recovery retry or a resume of the node already re-runs — and
     it still must not PUBLISH: artifact persistence, `publish:` and
     downstream edges happen only after validation succeeds. Restricting
     engine-owned rounds to tool-less turns is refused below: only a claw
     agent with a captured conversation has one.
   - **What is persisted, and how a resume behaves.** The episode is
     written BEFORE each round and after its result, under a ledger key a
     retry or a resume derives again (`correctionInvocationIdentity`). A
     crash inside a round leaves that round counted, so it is never
     re-spent. A resumed node re-executes, its output having never been
     published, and so draws one fresh sample but never a fresh budget: an
     episode already `exhausted` or `unchanged` returns its violation with
     no round, which `TestSchemaValidation_PreseededTerminalEpisodeIsNotReinvoked`
     pins at the engine.
   - **What an operator sees.** The round's generations keep their own
     events — `delegate_retry` with its re-ask mode, then
     `delegate_started` / `delegate_finished` marked attempt 2 and priced at
     what they added. Above them stands the episode: a `node_recovery`
     event per phase (`strategy: schema_output_correction`; `attempt`,
     `succeeded`, `exhausted`, `unchanged`, `spend_blocked`,
     `budget_raised`), the episode on the run document and in the snapshot
     (`output_corrections`, `output_correction_history`), and the episode in
     the pilot's report. After a terminal episode in the trunk comes the
     recovery dispatch's own `node_recovery` event, as today: no strategy,
     its classified `code` and the recipe's reason.
   - **What the unit moves first.** The usage type the seam returns moves
     where the producer can name it, aliased in `pkg/runtime` so every
     existing reader compiles, and it gains the unreported-calls count its
     own doc defers to a second implementor. A round reports its MARGINAL
     spend, because the engine SUMS it onto the node's totals
     (`preserveCorrectionMetadata`): the session rule `foldSpend` applies
     today — a session total folds at the max — runs inside the round
     before it reports, or a shared session is billed twice. An episode
     opened from the typed failure validates its first payload and each
     candidate against the node's schema whatever `WithOutputValidation`
     says. That option keeps
     its scope and its zero production callers: enabling it would reach
     every node kind with a declared output schema, a second behaviour
     change this one does not ride on.

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

- **Keep the engine's `OutputCorrector` seam a SEAM, neither retired nor
  wired** — decision 5 as this record first took it. Its text: "It is the
  documented hatch for a schema-level corrector, and the modularity rule is
  what keeps it: an executor that can correct implements the interface
  rather than earning an engine `if`. Wiring `ClawExecutor` into it today
  would mean reconciling THREE correction layers, not two — the ladder, the
  re-ask and the engine loop — and every schema-invalid node would pay a
  third generation. Retiring it would delete the only place where a durable
  correction episode, a budget that survives a restart and a fingerprint
  stop already exist." The wiring itself stood here as refused for this
  record: "a production cost and latency change (a third billed generation
  on the failing path)", needing "the two in-executor paths subordinated or
  stood down first". Lost on the operator's verdict of 2026-11-03, which
  decided the wiring. The cost it cited is what the reconciliation answers:
  the in-place round is stood down rather than stacked, the ladder is not a
  layer of it, and one round by default buys no generation an execution
  does not buy today — the bill can only fall, by the recovery retry's
  second round.
- **Fail a terminal episode as `SCHEMA_VALIDATION`, through
  `execLoopAfterExec`, with no node retry.** Refused: the bill would drop
  further, to three generations, but the fresh sample a trunk node draws
  today on `ExecutionFailedRecipe(1)` would go with it, and the code would
  move from `EXECUTION_FAILED` (`DispositionTransient`, re-driven by the CLI
  `--auto-resume`) to `SCHEMA_VALIDATION` (`DispositionReexecutable`, which
  that loop refuses) — a second behaviour change the wiring does not need.
- **Give the recovery retry a round of its own.** Refused: it would keep
  today's six generations only by deriving a new ledger key per attempt,
  and the episode's key is stable across a retry and a resume precisely so
  that neither resets a consumed budget.
- **Open the episode from the loop's context, after the execution
  returned.** Refused: the trunk would lose its run ID and its `needs:`
  lease for the round, so a claw agent's re-ask would restart the turn with
  its tools on instead of continuing its conversation tool-less, outside
  the slot that bounds the node.
- **Restrict engine-owned rounds to tool-less turns, keeping the seam's
  contract as written.** Refused: only a claw agent with a captured
  conversation has a tool-less continuation, so claude_code's
  `resume_session` re-ask and every restart — each fan-out branch among
  them — would lose a correction they get today.
- **Count generations rather than rounds.** Refused: the budget would read
  as the bill, but the fingerprint stop would then fire between the re-ask
  and the extraction on a backend that answers the same unstructured text
  twice — the backend the extraction exists for.
- **Turn `WithOutputValidation` on in production and let the producer stop
  validating.** Refused: the option applies to every node kind with a
  declared output schema, so the wiring would bring a second behaviour
  change with it, and the producer's eligibility knowledge would move into
  an engine that cannot know which shape another ask fixes.
- **Stand every in-place round down once the engine owns correction.**
  Refused: the LLM router's round would vanish with no owner taking it
  over — the engine holds no schema for a router's selection and fails a
  trunk router on any error — so a selection one more turn could have fixed
  would fail the run, the very loss the router's re-ask exists to prevent.
- **Hand the round its inputs through a cache keyed by node.** Refused: the
  branches of one `fan_out_each` node run concurrently, so a node key would
  hand one branch another's conversation; the handle travels with the typed
  failure instead.
- **Fold the verified-action ladder into the episode as a second
  corrector.** Refused: it is another failure class. Its oracle is the
  postcondition, not the schema the episode validates a candidate against,
  and its bounds are the ones the workflow author declares per node, where
  the episode's is an engine-wide budget — folding it would judge a command
  by a schema and replace an author's bound with an operator's. The ladder
  keeps its own bound and its own stop (decision 4).
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
- **A wider eligibility.** #1006 asks for the re-ask to reach deterministic,
  verifiable violations too. A type or an enum violation that buys a round
  changes the spend of runs that fail fast today, so widening the gate is a
  decision of its own — opt-in, with its own witness — and not a side
  effect of the wiring.
- **The correction budget's database level.** A run-affecting setting
  belongs to a level above the bot (ADR-132, #2699); until that resolver
  exists the budget's floor is the process variable, as the rollout mode's
  is.
- **That the seam survives a second implementor unchanged.** Decision 5
  gives it one production implementor; it does not freeze its shape for
  the next.
- **The database level of the rollout mode.** #2699 and ADR-132 own it. This
  record is their citation until they absorb it.
- **That the pilot runs.** Three criteria depend on a deployment gesture no
  agent can take.

## Consequences

- **Five criteria stay unticked, with six named slices between them**
  (criterion 3 owns two, criteria 4 and 5 share two), and the orchestrator
  files them as sub-issues under #1403: the scenario matrix, `ClawExecutor`
  wired as the engine's corrector with the re-ask's eligibility witness, the
  ladder's unchanged-proposal stop with the self-repair rung's attempt
  count, the store-identity seam (then the comparison), the business-stores
  declaration, and the pilot. Four qualifiers the code honours are
  unwitnessed and file with them: nothing asserts that a subbot child keeps
  the parent's policy and store declarations, nothing observes a node that
  did NOT run when admission denied it, nothing refuses a circuit that holds
  back a healthy scope, and no test reads the diagnostic projection through
  the CLI, Studio or Copi. The table above records each as unwitnessed
  rather than borrowing the suites that merely cross that code.
  An unticked box with a named slice is a tracked item; an unticked box with
  nothing behind it is the state this record ends.
- **A run that relies on the silent re-ask changes when the wiring
  lands, and its spend can only fall.** One round by default pays, per
  execution, what the in-place round pays; a trunk node whose output stays
  invalid pays up to four generations instead of six, its recovery retry
  drawing a fresh first answer and no second round, while its failure code
  (`EXECUTION_FAILED`) and the CLI auto-resume that reads it do not move.
  What moves besides: the round leaves an episode and its
  `node_recovery` events, and the pilot's report reads every episode short
  of `succeeded` as not rollback-safe (`correctionEpisodeSettled`), so a
  run whose round failed is newly counted where it left no ledger before;
  a resumed node draws a fresh sample and no second round unless the
  budget is raised; a run at its spend edge settles `spend_blocked` instead
  of re-asking; and `ITERION_OUTPUT_CORRECTION_BUDGET=0`, which today stands
  down a loop no production path reaches, then stands an agent's or a
  judge's re-ask down too. An LLM router's selection is not among them: it
  keeps its in-place round, its bill and its events, with no episode.
  The unit's release note names each.
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
