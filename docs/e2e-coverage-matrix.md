<!-- e2e-coverage-matrix: v1 — machine-parsed; see the coverage-matrix skill of the e2e-coverage bot -->

# E2E coverage matrix

The **single** feature×coverage inventory for iterion. One row per
operator-observable promise, at the granularity a regression report would
name it. Every row is terminal (`covered-deterministic` / `covered-live` /
`unit-only` / `excluded`) or an honest `uncovered` gap with the plan.

This file supersedes the three partial coverage docs it reconciles:

- [docs/e2e_coverage.md](e2e_coverage.md) — the iterion×claw-code-go live
  matrix + the deliberate claw-side gaps (folded into the `backends`,
  `tools-mcp` and `observability` families below).
- [docs/live-e2e-coverage.md](live-e2e-coverage.md) — the opt-in real-LLM
  layer (how it works, the quality-judge panel, the per-bot targets). Still
  the reference for *running* the live layer; its bot/feature tables are
  mirrored here as `covered-live` rows.
- [e2e/SCENARIOS.md](../e2e/SCENARIOS.md) — the flagship-workflow stub-executor
  scenarios (folded into `runtime` and `dsl`).

## Legend

| Status | Meaning |
|---|---|
| `covered-deterministic` | A CI-runnable, credential-free test drives the real seams and asserts observable outcomes |
| `covered-live` | Only exercised in the opt-in `live`-tagged layer (needs a real model/credential) |
| `unit-only` | Deliberately terminal at unit level — an e2e would only re-test the harness |
| `excluded` | Not exercisable in this repo's harness (needs a third-party tenant / cloud control plane) |
| `uncovered` | Real gap — backlog |

## What "the front door" means per family

- **dsl** — the front door is `.bot` source text: parse → compile → the
  diagnostic or IR an operator sees from `iterion validate`. Rows whose
  behaviour only shows at execution time cite a runtime/e2e test instead.
- **runtime** — the public engine API (`runtime.Engine.Run/Resume`) against
  a real `store.RunStore`, asserting persisted status, checkpoint, artifacts
  and the event stream. `e2e/` additionally enters through `.bot` fixtures.
- **cli** — `cli.Run*` entry points with a real store and (where an LLM would
  be involved) a stub `runtime.NodeExecutor` injected at the documented seam.
- **server-api / cloud** — `httptest` against the real wired handler.
- **studio-ui** — no browser harness exists in this repo; UI rows are honest
  gaps rather than silent skips.

## Scope of the current campaign

Pass 1 built this inventory and closed the highest-value CLI quick wins.
Everything still `uncovered` is deliberate backlog for later scoped runs,
family by family (the biggest blocks: `studio-ui`, parts of `cloud`, and the
sandbox/container surface).

| ID | Feature | Family | Status | Tests | Notes |
|---|---|---|---|---|---|
| dsl.node-agent | agent node: LLM node with structured I/O executes and publishes | dsl | covered-deterministic | TestSingleModel_HappyPath (e2e/e2e_test.go) | |
| dsl.node-judge | judge node: verdict-producing LLM node | dsl | covered-deterministic | TestSingleModel_HappyPath (e2e/e2e_test.go) | |
| dsl.node-tool | tool node: direct shell command, no LLM | dsl | covered-deterministic | TestCIFix_HappyPath (e2e/e2e_test.go) | |
| dsl.node-compute | compute node: deterministic expression output | dsl | covered-deterministic | TestToolAndComputePublishArtifact (pkg/runtime/engine_test.go) | |
| dsl.node-human | human node: pause/resume with interaction record | dsl | covered-deterministic | TestCompliance_HumanGate (e2e/e2e_test.go) | |
| dsl.node-subbot | subbot node: nested child run, outputs read back | dsl | covered-deterministic | TestRunSubbotsPersistNestedLineage (pkg/cli/run_subbot_test.go) | |
| dsl.node-emit-wait | emit/wait: in-bot event pair with mandatory timeout | dsl | covered-deterministic | TestEventsEmitWait (e2e/events_emit_wait_test.go) | |
| dsl.node-await-answers | await_answers node parks its branch until answers land | dsl | covered-deterministic | TestAwaitAnswersReleasedByAnswer (e2e/async_interaction_test.go) | |
| dsl.node-done | done terminal node ends the run finished | dsl | covered-deterministic | TestSingleModel_HappyPath (e2e/e2e_test.go) | |
| dsl.node-fail | fail terminal node ends the run non-resumable failed | dsl | covered-deterministic | TestFailNode (pkg/runtime/engine_test.go) | |
| dsl.router-fan-out-all | router fan_out_all spawns parallel branches | dsl | covered-deterministic | TestDualParallel_HappyPath (e2e/e2e_test.go) | |
| dsl.router-fan-out-each | router fan_out_each: per-item branches with a dep DAG | dsl | covered-deterministic | TestFanOutEach_DAG_DiamondOrderingAndParallelism (pkg/runtime/fan_out_each_test.go) | |
| dsl.router-condition | router condition mode picks the matching edge | dsl | covered-deterministic | pkg/dsl/ir/validate_test.go, TestElseEdge_Routing (pkg/runtime/else_edge_test.go) | |
| dsl.router-round-robin | router round_robin alternates targets across iterations | dsl | covered-deterministic | pkg/runtime/round_robin_test.go | |
| dsl.router-llm | router llm mode: model picks the route | dsl | covered-deterministic | TestLLMRouterSelectsOtherRoute (pkg/runtime/llm_router_test.go) | |
| dsl.edges-conditional | edge `when` / `when not` conditions on a boolean output field | dsl | covered-deterministic | TestSingleModel_RefineLoop (e2e/e2e_test.go) | |
| dsl.edges-else | edge `else` fires only when no sibling `when` matched | dsl | covered-deterministic | TestElseEdge_PreferredOverStrayUnconditional (pkg/runtime/else_edge_test.go) | |
| dsl.edges-loop | bounded loop edge `as name(n)` | dsl | covered-deterministic | TestBoundedLoop (pkg/runtime/engine_test.go) | |
| dsl.edges-loop-templated-cap | loop cap templated from an upstream output | dsl | covered-deterministic | TestLoopTemplatedCap_FromOutput (pkg/runtime/engine_test.go) | |
| dsl.edges-data-mapping | edge `with {…}` data mapping and reference interpolation | dsl | covered-deterministic | TestResolveMapping_InterpolatesSurroundingLiterals (pkg/runtime/engine_resolve_mapping_test.go) | |
| dsl.refs | reference syntax: input/vars/outputs/artifacts substitution | dsl | covered-deterministic | pkg/dsl/ir/ref_test.go | |
| dsl.vars-defaults | `vars:` defaults applied when no override is supplied | dsl | covered-deterministic | pkg/dsl/ir/validate_var_default_test.go | |
| dsl.vars-enum | `vars:` enum constraint rejects an out-of-set value at launch | dsl | covered-deterministic | TestRunRejectsInvalidEnumVar (pkg/runtime/engine_var_enum_test.go) | |
| dsl.presets | in-source `presets:` block resolves named value sets | dsl | covered-deterministic | pkg/dsl/ir/presets_test.go | CLI wiring covered by cli.run-preset |
| dsl.prompts | `prompt <name>:` blocks and prompt includes | dsl | covered-deterministic | pkg/dsl/ir/prompt_include_test.go | |
| dsl.schemas | `schema <name>:` typed node output contracts | dsl | covered-deterministic | pkg/backend/model/schema_test.go | |
| dsl.cursors | `cursor <name>:` calibration fragments reach the system prompt | dsl | covered-deterministic | pkg/backend/model/cursors_test.go, pkg/dsl/ir/cursor_resolve_test.go | |
| dsl.attachments | `attachments:` block: files resolved and passed to nodes | dsl | covered-deterministic | pkg/dsl/ir/attachments_test.go, pkg/runtime/attachment_path_test.go | |
| dsl.skills-field | `skills:` field pulls library skills into the run mirror | dsl | covered-deterministic | pkg/dsl/ir/validate_skills_test.go, pkg/runtime/library_skills_test.go | |
| dsl.mcp-server-block | `mcp_server:` block declares stdio/http/sse MCP servers | dsl | covered-deterministic | TestMCPServer_SSETransport (pkg/dsl/parser/parser_mcp_test.go) | |
| dsl.capabilities | `capabilities:` list opens the board tool surface | dsl | covered-deterministic | pkg/dsl/ir/validate_capabilities_test.go, TestBoardDispatcher_E2E_CapabilityGate (e2e/board_dispatcher_test.go) | |
| dsl.supervisor-block | `supervisor <name>:` declaration compiles and spawns a coordinator | dsl | covered-deterministic | pkg/dsl/ir/compile_supervisors_test.go, pkg/supervise/coordinator_test.go | |
| dsl.compress-field | `compress:` precedence (CLI → node → workflow → env → default) | dsl | covered-deterministic | pkg/dsl/ir/compress_test.go | |
| dsl.auto-memory-field | `auto_memory:` per-node MEMORY.md switch, off by default | dsl | covered-deterministic | pkg/dsl/ir/auto_memory_test.go, pkg/backend/model/executor_auto_memory_test.go | |
| dsl.permission-field | `permission:` mode + allow/ask/deny rule lists | dsl | covered-deterministic | pkg/dsl/ir/permission_test.go, pkg/backend/permission/permission_test.go | |
| dsl.budget-block | `budget:` block fields compile onto the workflow | dsl | covered-deterministic | pkg/dsl/ir/budget_ceiling_test.go | |
| dsl.sandbox-block | `sandbox:` block (image/build/network/host_state) compiles | dsl | covered-deterministic | pkg/dsl/ir/sandbox_test.go, pkg/dsl/parser/parser_sandbox_test.go | |
| dsl.worktree-field | `worktree:` mode default resolution | dsl | covered-deterministic | pkg/dsl/ir/worktree_default_test.go | |
| dsl.session-modes | session modes (fresh / inherit / artifacts_only) | dsl | covered-deterministic | pkg/backend/model/session_test.go | |
| dsl.secrets-block | `secrets:` declarations + optional-secret semantics | dsl | covered-deterministic | pkg/dsl/ir/secrets_test.go, pkg/dsl/ir/optional_secret_test.go | |
| dsl.memory-block | `memory:` block scopes/visibility validation | dsl | covered-deterministic | pkg/dsl/ir/memory_visibility_test.go | |
| dsl.verified-action | Verified Action quad (goal/postcondition/policy/recovery) | dsl | covered-deterministic | TestVerifiedActionEngineEmitsAndStrips (e2e/verified_action_test.go) | |
| dsl.groups-iteration | `group:` expansion / iteration sugar | dsl | covered-deterministic | pkg/dsl/ir/expand_groups_test.go, pkg/dsl/ir/foreach_test.go | |
| dsl.diagnostics | compile diagnostics C001–C2xx codes and severities | dsl | covered-deterministic | pkg/dsl/ir/diag_codes_test.go, TestValidate_Invalid (pkg/cli/cli_test.go) | |
| dsl.unparse-roundtrip | IR → `.bot` serialization round-trips | dsl | covered-deterministic | pkg/dsl/unparse/roundtrip_test.go | |
| dsl.ast-json | AST JSON encode/decode (`MarshalFile`/`UnmarshalFile`) | dsl | covered-deterministic | pkg/dsl/ast/jsonenc_test.go | |
| dsl.expr | expression evaluator for compute nodes and `when` conditions | dsl | unit-only | pkg/dsl/expr/expr_test.go, pkg/dsl/expr/overflow_test.go | pure evaluator, exhaustively asserted at unit level; its e2e effect is already covered by dsl.node-compute — an extra pipeline test would re-assert a pure function |
| dsl.parser-fuzz | lexer/parser robustness on malformed input | dsl | unit-only | pkg/dsl/parser/fuzz_test.go | fuzzing is by nature a unit-level property test; the operator-visible surface (a diagnostic, not a panic) is dsl.diagnostics |
| runtime.linear-execution | sequential node execution to a terminal node | runtime | covered-deterministic | TestLinearPath (pkg/runtime/engine_test.go) | |
| runtime.await-wait-all | convergence `await: wait_all` waits for every branch | runtime | covered-deterministic | TestDualParallel_HappyPath (e2e/e2e_test.go) | |
| runtime.await-best-effort | convergence `await: best_effort` proceeds on partial branches | runtime | covered-deterministic | TestChaos_FailMidFanOut_BestEffort (pkg/runtime/chaos_test.go) | |
| runtime.local-loop | local loop re-executes and versions artifacts | runtime | covered-deterministic | TestSingleModel_RefineLoop (e2e/e2e_test.go) | |
| runtime.global-reloop | global reloop restarts the recipe from an upstream node | runtime | covered-deterministic | TestSingleModel_GlobalReloop (e2e/e2e_test.go) | |
| runtime.loop-exhaustion | loop cap exhaustion fails the run with LOOP_EXHAUSTED | runtime | covered-deterministic | TestCIFix_LoopExhaustion (e2e/e2e_test.go), TestLoopExhaustionRuntimeError (pkg/runtime/hardening_test.go) | |
| runtime.budget-cost | `max_cost_usd` exceeded stops the run and emits budget_exceeded | runtime | covered-deterministic | TestBudgetCostExceeded (pkg/runtime/budget_test.go) | |
| runtime.budget-tokens | `max_tokens` exceeded stops the run | runtime | covered-deterministic | TestBudgetTokensExceeded (pkg/runtime/budget_test.go) | |
| runtime.budget-duration | `max_duration` exceeded stops the run | runtime | covered-deterministic | TestBudgetDurationExceeded (pkg/runtime/budget_test.go) | |
| runtime.budget-warning | budget warning event at the soft threshold, advisory only | runtime | covered-deterministic | TestBudgetWarningEmitted (pkg/runtime/budget_test.go), TestWarnTokensAdvisoryNeverBlocks (pkg/runtime/budget_test.go) | |
| runtime.budget-shared | budget accounting shared across parallel branches | runtime | covered-deterministic | TestBudgetSharedFirstComeFirstServed (pkg/runtime/budget_test.go) | |
| runtime.max-parallel-branches | `max_parallel_branches` semaphore bounds concurrency | runtime | covered-deterministic | TestFanOutEach_DAG_BoundedParallelism (pkg/runtime/fan_out_each_test.go) | |
| runtime.workspace-safety | only one mutating branch may run concurrently | runtime | covered-deterministic | TestWorkspaceSafetyRejectsDualMutation (pkg/runtime/budget_test.go) | |
| runtime.checkpoint | a checkpoint is saved after every successful node | runtime | covered-deterministic | TestCheckpointPreservesUpstreamOutputs (pkg/runtime/engine_test.go) | |
| runtime.resume-failed | resume from failed_resumable restarts at the failing node | runtime | covered-deterministic | TestResumeFromFailed (pkg/runtime/engine_test.go), TestResumeDoesNotReplayUpstream (pkg/runtime/engine_test.go) | |
| runtime.resume-hash-guard | resume refuses a changed `.bot` unless `--force` | runtime | covered-deterministic | TestForceResumeBypassesHashCheck (pkg/runtime/engine_test.go), pkg/runview/service_resume_hash_test.go | |
| runtime.resume-human | resume a paused_waiting_human run with answers | runtime | covered-deterministic | TestHumanPauseAndResume (pkg/runtime/engine_test.go), TestResume_Success (pkg/cli/cli_test.go) | |
| runtime.cancel | cancellation produces a cancelled status with a checkpoint | runtime | covered-deterministic | TestCancelProducesCancelledStatus (pkg/runtime/hardening_test.go) | |
| runtime.timeout | outer deadline produces a TIMEOUT failure | runtime | covered-deterministic | TestTimeoutProducesFailedStatus (pkg/runtime/hardening_test.go) | |
| runtime.interaction-modes | human `interaction:` llm / llm_or_human / async escalation | runtime | covered-deterministic | TestInteractionLLMOrHumanEscalation (pkg/runtime/engine_test.go), TestInteractionLLMAutoRespond (pkg/runtime/engine_test.go) | |
| runtime.ask-user-conversation | ask_user relays prior Q/A and persists the conversation | runtime | covered-deterministic | TestInteractionAskUserPersistsConversation (pkg/runtime/engine_test.go) | |
| runtime.worktree-finalize | `worktree: auto` creates a branch and fast-forwards the checkout | runtime | covered-deterministic | pkg/runtime/worktree_test.go | |
| runtime.rewind | `iterion rewind` re-anchors a run and invalidates downstream state | runtime | covered-deterministic | TestRewindThenResume_SkipsUpstreamNodes (e2e/rewind_resume_test.go) | |
| runtime.rewind-workspace | rewind restores workspace files for non-worktree runs | runtime | covered-deterministic | TestRewindRestoresWorkspaceEndToEnd (e2e/rewind_workspace_test.go) | |
| runtime.fork | fork a run at a prior LLM turn into a resumable child run | runtime | covered-deterministic | pkg/runview/fork_test.go | |
| runtime.event-stream | event sequence coherence (ordering, pairing, monotonic seq) | runtime | covered-deterministic | TestEventSequenceCoherence (e2e/e2e_test.go) | |
| runtime.artifact-versioning | repeated node executions version their artifacts | runtime | covered-deterministic | TestSingleModel_GlobalReloop (e2e/e2e_test.go) | |
| runtime.publish-gate | only a `publish:`-declared node leaves an artifact | runtime | covered-deterministic | TestOnlyAPublishedNodeLeavesAnArtifact (e2e/handoff_publish_test.go) | |
| runtime.skills-mirror | bundle/plugin/library skills mirrored into `.claude/skills/` | runtime | covered-deterministic | TestMirrorBundleSkills_CopiesIntoClaudeSkills (pkg/runtime/bundle_test.go) | |
| runtime.devbox-provision | a bot's/target's `devbox.json` is installed and put on PATH | runtime | covered-deterministic | TestEngineRun_HostDevbox_RepoProjectInstallsInPlace (pkg/runtime/devbox_host_test.go) | |
| runtime.subbot-depth-guard | nested subbot recursion depth guard | runtime | covered-deterministic | TestSubbotRunnerForCLI_RecursionDepthGuard (pkg/cli/subbot_nested_test.go) | |
| runtime.supervisor | supervisor steers a watched node via the message inbox | runtime | covered-deterministic | pkg/supervise/coordinator_test.go, pkg/backend/model/inbox_test.go | live end-to-end variant: TestLive_Feat_Supervisor |
| runtime.recovery-dispatch | adaptive recovery ladder for verified action nodes | runtime | covered-deterministic | TestVerifiedActionEngineEmitsAndStrips (e2e/verified_action_test.go), pkg/backend/model/executor_verified_action_test.go | |
| runtime.privacy-redaction | secret/PII redaction across the run pipeline | runtime | covered-deterministic | TestE2E_PrivacyPipeline (e2e/privacy_test.go) | |
| persistence.run-json | run.json metadata, status transitions, format version | persistence | covered-deterministic | TestFormatVersionPersisted (pkg/runtime/hardening_test.go), pkg/store/store_test.go | |
| persistence.events-jsonl | events.jsonl append + monotonic seq + replay | persistence | covered-deterministic | pkg/store/store_test.go | |
| persistence.artifacts | versioned per-node artifacts under artifacts/ | persistence | covered-deterministic | pkg/store/store_test.go | |
| persistence.interactions | interaction records (questions/answers) persisted per run | persistence | covered-deterministic | TestAwaitAnswersAlreadyAnswered (e2e/async_interaction_test.go), pkg/store/store_test.go | |
| persistence.child-runs | parent/child run lineage for subbots | persistence | covered-deterministic | TestRunSubbotsPersistNestedLineage (pkg/cli/run_subbot_test.go) | |
| persistence.workspace-versioning | content-addressed workspace snapshots + restore | persistence | covered-deterministic | pkg/workspacetrack/native_test.go | |
| persistence.store-anchoring | store dir resolution (project `.iterion` vs `$ITERION_HOME/projects`) | persistence | covered-deterministic | TestStoreAnchorDir_BotInsideProjectResolvesProjectStore (pkg/cli/storeanchor_test.go) | |
| persistence.mongo-store | cloud Mongo-backed run store conformance | persistence | covered-deterministic | TestMongoStore_Conformance (pkg/dispatcher/boardmongo/conformance_test.go) | CI job `mongo-conformance`; skips without a Mongo endpoint |
| cli.validate | `iterion validate` parses, compiles and reports diagnostics | cli | covered-deterministic | TestValidate_Valid (pkg/cli/cli_test.go), TestValidate_Invalid (pkg/cli/cli_test.go) | |
| cli.validate-bundle | `iterion validate` cross-checks a bundle manifest (C2xx) | cli | covered-deterministic | TestRunValidate_BundleVarTypoWarns (pkg/cli/validate_bundle_test.go) | |
| cli.run | `iterion run` executes a `.bot` and persists the run | cli | covered-deterministic | TestRun_Success (pkg/cli/cli_test.go) | |
| cli.run-vars | `iterion run --var key=value` overrides workflow vars | cli | covered-deterministic | TestRun_WithVars (pkg/cli/cli_test.go) | |
| cli.run-preset | `iterion run --preset` applies an in-source preset, `--var` wins over it | cli | covered-deterministic | TestRunPresetAppliesValuesAndVarWins (e2e/cli_launch_overrides_test.go) | |
| cli.run-budget-override | `iterion run --max-*` re-budgets the workflow for this run | cli | covered-deterministic | TestRunBudgetOverrideCapsTheRun (e2e/cli_budget_override_test.go) | |
| cli.run-model-backend-override | `iterion run --model/--backend selector=…` re-target nodes | cli | uncovered | | overrides are consumed inside the real `ClawExecutor` (runview.BuildExecutor), so a stub executor cannot observe them; plan: an executor-level e2e or a live test. Parsing is unit-covered by pkg/backend/model/model_override_test.go |
| cli.run-human-pause | `iterion run` returns at a human pause (`--no-interactive`) | cli | covered-deterministic | TestRun_HumanPause (pkg/cli/cli_test.go) | |
| cli.run-json | `--json` machine output mode for run | cli | covered-deterministic | TestRun_SuccessJSON (pkg/cli/cli_test.go) | |
| cli.run-recipe | `iterion run --recipe <file>` applies a recipe overlay | cli | uncovered | | recipe parsing covered by pkg/backend/recipe/recipe_test.go; the CLI overlay wiring has no test. Plan: run a `.bot` with a recipe that changes a node's model/budget and assert the run reflects it |
| cli.run-auto-resume | `--auto-resume N` re-drives a retryable failed_resumable run | cli | uncovered | | gate/backoff/classification unit-covered (pkg/cli/auto_resume_test.go); the loop itself waits ≥15s (30s base backoff) so a deterministic e2e needs an injectable clock — a testability seam, deferred |
| cli.resume | `iterion resume` continues a paused/failed/cancelled run | cli | covered-deterministic | TestResume_Success (pkg/cli/cli_test.go) | |
| cli.resume-answers-file | `iterion resume --answers-file` / `--answer @file` | cli | covered-deterministic | TestResolveFileAnswerFlags_AttachesLocalFile (pkg/cli/resume_file_answers_test.go), TestParseAnswersFile (pkg/cli/cli_test.go) | |
| cli.resume-subbot | resume a run that owns subbot children | cli | covered-deterministic | TestResume_RunWithSubbot (pkg/cli/resume_subbot_test.go) | |
| cli.inspect | `iterion inspect` lists runs and shows a run's state | cli | covered-deterministic | TestInspect_ListRuns (pkg/cli/cli_test.go), TestInspect_SingleRun (pkg/cli/cli_test.go) | |
| cli.inspect-events | `iterion inspect --events` renders the stored event stream | cli | covered-deterministic | TestInspect_WithEvents (pkg/cli/cli_test.go) | |
| cli.inspect-node | `iterion inspect --node` per-node trace/artifacts/log sections | cli | covered-deterministic | TestInspect_SectionTrace (pkg/cli/cli_test.go), TestInspect_SectionArtifactsIncludesBody (pkg/cli/cli_test.go) | |
| cli.report | `iterion report` renders a run's chronological markdown report | cli | covered-deterministic | TestReportRendersChronologicalRunReport (e2e/cli_report_test.go), TestReportHonoursOutputPathAndJSON (e2e/cli_report_test.go) | |
| cli.diagram | `iterion diagram` emits a Mermaid graph for a `.bot` | cli | covered-deterministic | TestDiagramRendersEveryNodeAndEdgeOfTheWorkflow (e2e/cli_diagram_test.go), TestDiagramViewsDifferAndUnknownViewIsRefused (e2e/cli_diagram_test.go), TestDiagramJSONCarriesTheRenderedGraph (e2e/cli_diagram_test.go) | every node + edge of e2e/testdata/diagram_mini.bot, the condition/loop/mapping labels, the compact↔detailed↔full deltas, and the typo'd `--view` refusal |
| cli.runs-prune | `iterion runs prune` age/status/keep-last retention + dry-run | cli | covered-deterministic | TestRunPrune_AgeFiltering (pkg/cli/runs_prune_test.go), TestRunPrune_DryRunDeletesNothing (pkg/cli/runs_prune_test.go) | |
| cli.runs-async-questions | `iterion runs questions` / `runs answer` drive an async question to delivery | cli | covered-deterministic | TestRunsQuestionsThenAnswerReleasesAwaitGate (e2e/cli_async_questions_test.go), TestRunsAnswerRejectsBadInput (e2e/cli_async_questions_test.go) | |
| cli.fork | `iterion fork` creates a resumable fork at a prior turn | cli | covered-deterministic | pkg/runview/fork_test.go | CLI layer is a thin wrapper over runview.Service.Fork |
| cli.rewind | `iterion rewind` (incl. `--auto` bot-diff targeting) | cli | covered-deterministic | TestRewind_RefusesRunningRun_E2E (e2e/rewind_resume_test.go) | |
| cli.import | `iterion import` lowers a Claude-Code workflow script to a draft `.bot` | cli | covered-deterministic | TestRunImport_WritesDraft (pkg/cli/import_test.go) | |
| cli.bots-list | `iterion bots list` discovers `.bot`/`.botz` bundles | cli | covered-deterministic | TestBotsList_Bundle (pkg/cli/bots_test.go) | |
| cli.bots-create | `iterion bots create` scaffolds a discoverable bundle | cli | covered-deterministic | TestBotsCreate_ProducesDiscoverableBundle (pkg/cli/bots_create_test.go) | |
| cli.bots-regen-catalog | `iterion bots regen-catalog` regenerates Nexie's catalog skill | cli | covered-deterministic | bots/catalog_freshness_test.go | |
| cli.bundle-pack | `iterion bundle pack` produces a loadable `.botz` | cli | covered-deterministic | TestBundle_SecAuditSource_PackOpenCompile (e2e/bundle_sec_audit_source_test.go) | |
| cli.marketplace | `iterion marketplace submit/install/uninstall` (bot + plugin kinds) | cli | covered-deterministic | TestMarketplaceCLI_SubmitInstallUninstall_KindAware (pkg/cli/marketplace_test.go) | |
| cli.plugin | `iterion plugin list/enable/disable/install/uninstall/config` | cli | covered-deterministic | pkg/plugin/install_test.go, pkg/plugin/config_test.go | |
| cli.skill-library | `iterion skill list/show/add/rm/import/export` | cli | uncovered | | the library store is covered (pkg/skilllib), the `iterion skill` command wrapper is not. Plan: add/list/export round-trip against a temp library root |
| cli.secret | `iterion secret set/list/rm` local sealed-secret lifecycle | cli | covered-deterministic | TestSecretSetListRemoveRoundTrip (e2e/cli_secret_test.go), TestSecretProjectScopeOverridesGlobal (e2e/cli_secret_test.go) | |
| cli.memory | `iterion memory export/import/du` | cli | uncovered | | the memory store is covered (pkg/memory); the CLI wrapper is not. Plan: export → import round-trip into a temp `$ITERION_HOME` |
| cli.models | `iterion models` resolves capabilities and their source | cli | covered-deterministic | TestRunModels_JSONSingleModel (pkg/cli/models_test.go) | |
| cli.openapi | `iterion openapi` emits this build's OpenAPI 3.1 spec offline | cli | covered-deterministic | pkg/server/openapi_test.go | |
| cli.schedule | `iterion schedule add/list/remove/install/uninstall` crontab manifest | cli | covered-deterministic | TestRunScheduleAddListRemove (pkg/cli/schedule_test.go), TestRunScheduleInstallUninstall_SeamRoundTrip (pkg/cli/schedule_test.go) | |
| cli.schedule-gate | schedule overlap policy + pre-launch guard + tick audit | cli | covered-deterministic | TestScheduleRun_OverlapSkipsAndAuditsBlockingRun (pkg/cli/schedule_gate_test.go), TestScheduleRun_GuardNonZeroBlocks (pkg/cli/schedule_gate_test.go) | |
| cli.issue | `iterion issue create/list/show/move/update/close/board` | cli | covered-deterministic | TestIssueCLILifecycleCreateMoveUpdateClose (e2e/cli_issue_test.go), TestIssueCloseRefusesABoardWithNoTerminalState (e2e/cli_issue_test.go) | create → list/show → move → update → close read back through a fresh native.Store, plus the events.jsonl audit trail |
| cli.sandbox-doctor | `iterion sandbox doctor [--strict]` host/run pre-flight diagnosis | cli | covered-deterministic | TestRunSandboxDoctorStrictNoSandbox (pkg/cli/sandbox_strict_test.go), TestRunNetworkStrictChecks (pkg/cli/sandbox_strict_test.go) | |
| cli.studio | `iterion studio` boots the server and reports its port | cli | covered-deterministic | TestRunStudio_OnReady_RandomPort (pkg/cli/studio_test.go), TestIsLoopbackBindHost (pkg/cli/studio_bind_test.go) | |
| cli.dispatch | `iterion dispatch` daemon boots from a config with the bot catalogue | cli | covered-deterministic | TestBuildDefaultConfig_ValidatesAndExtractsCatalogue (pkg/cli/dispatch_defaults_test.go) | |
| cli.migrate-to-cloud | `iterion migrate to-cloud` local store → Mongo/S3 | cli | covered-deterministic | pkg/cli/migrate_orgs_test.go, pkg/cli/migrate_run_paths_test.go | orgs backfill + run-path rewrite halves; the S3 blob half needs a cloud backend (see cloud.migrate-blobs) |
| cli.mcp-server | `iterion mcp` operator MCP server (local_*/remote_* tools) | cli | covered-deterministic | TestMCPServer_DetachedRunSurvivesServerExit (e2e/mcp_server_test.go), pkg/operatormcp/tools_local_test.go | |
| cli.supervise | `iterion supervise` attaches to a managed run or a raw claude session | cli | covered-deterministic | pkg/supervise/coordinator_test.go, pkg/supervise/transcript_test.go | |
| cli.remote | `iterion remote` typed subcommands against a cloud instance | cli | covered-deterministic | TestRemoteRunsLaunch_SendsSourceAndVars (pkg/cli/remote_test.go), TestRemoteRunsFollow_CursorAndTerminal (pkg/cli/remote_test.go) | |
| cli.remote-login | browser loopback CLI-auth token mint + persistence | cli | covered-deterministic | TestResolveRemoteConfig_EnvMode (pkg/cli/remote_test.go), pkg/server/auth_routes_test.go | |
| cli.version | `iterion version [--commit]` | cli | covered-deterministic | cmd/iterion/version_test.go | |
| cli.bench-asymptote | `iterion bench asymptote` convergence benchmark | cli | uncovered | | pkg/benchmark covers metric collection; the asymptote command itself drives real runs. Plan: a stub-executor benchmark over a fixture loop |
| backends.selection-explicit | node `backend:` / workflow `default_backend:` selects the delegate | backends | covered-deterministic | pkg/backend/model/resolve_backend_test.go | |
| backends.autodetect | credential probing picks a backend when none is declared | backends | covered-deterministic | pkg/backend/detect/detect_test.go, pkg/backend/model/resolve_backend_test.go | live variant: TestLive_Feat_BackendAutodetect |
| backends.claw | claw in-process client: generation, retry, cache observability | backends | covered-deterministic | pkg/backend/model/claw_backend_test.go, pkg/backend/model/generation_test.go | real-provider behaviour: covered-live by TestLive_Lite_ClawComprehensive |
| backends.claude-code | claude_code CLI delegate: append-system-prompt, setting sources | backends | covered-deterministic | pkg/backend/delegate/claude_code_cred_test.go | |
| backends.pi | pi delegate incl. the RPC session + embedded extension | backends | covered-deterministic | pkg/backend/delegate/pi_rpc_test.go, pkg/backend/delegate/pi_mcp_test.go | |
| backends.kimi | kimi CLI delegate through the generic CLI-agent seam | backends | covered-deterministic | pkg/backend/delegate/cliagent_test.go | |
| backends.grok | grok CLI delegate through the generic CLI-agent seam | backends | covered-deterministic | pkg/backend/delegate/grok_test.go | |
| backends.codex-legacy | legacy codex delegate (frozen, C030 diagnostic) | backends | covered-deterministic | pkg/dsl/ir/compile_test.go | deprecated surface: kept compiling + diagnosed, not extended |
| backends.system-prompt-composition | per-backend SystemPromptMode (Standalone/Append/AuthoredBase) | backends | covered-deterministic | pkg/backend/delegate/delegate_test.go | |
| backends.reasoning-effort | `reasoning_effort` propagation and wire remapping | backends | covered-deterministic | pkg/backend/model/effort_test.go | |
| backends.ultracode | `ultracode` mode: xhigh + orchestration prerogative + C089 | backends | covered-deterministic | pkg/dsl/ir/ultracode_test.go, pkg/backend/model/effort_test.go | live behaviour on 4.8: TestLive_Feat_Ultracode |
| backends.retry-classification | transient vs fatal backend errors, retry + feedback | backends | covered-deterministic | pkg/backend/model/executor_retry_classification_test.go, pkg/backend/model/network_retry_test.go | |
| backends.cost-accounting | per-call cost/token accounting reaching the run totals | backends | covered-deterministic | pkg/backend/cost/cost_test.go, pkg/backend/model/executor_hooks_cost_test.go | |
| backends.oauth-forfait | subscription OAuth paths (Anthropic auth token, ChatGPT codex) | backends | covered-deterministic | pkg/backend/model/openai_forfait_ctx_test.go, pkg/secrets/subscription_oauth_test.go | credential *resolution* is deterministic; a real forfait call is excluded (needs a live subscription) |
| backends.bedrock-vertex-foundry | AWS Bedrock / GCP Vertex / Azure Foundry providers | backends | excluded | | needs real cloud credentials for AWS/GCP/Azure; claw's SDK paths are unit-tested with mocked clients and iterion adds no logic of its own |
| backends.model-quality | real model output quality / value-for-money grading | backends | covered-live | e2e/live_quality_test.go | the essence of the feature IS the live model; the judge panel is report-only by default |
| tools-mcp.tool-registry | tool registry + per-node allow lists (claw-native names) | tools-mcp | covered-deterministic | pkg/backend/tool/registry_test.go | |
| tools-mcp.claw-builtins | claw built-in tools (read/write/bash/glob/grep/edit/web_fetch) | tools-mcp | covered-deterministic | pkg/backend/tool/claw_builtins_test.go | live variant: TestLive_Lite_ClawBuiltinTools |
| tools-mcp.mcp-lifecycle | MCP server startup, health check and `--skip-mcp-health` | tools-mcp | covered-deterministic | pkg/backend/mcp/config_test.go, TestSkipMCPHealthFromEnv (pkg/cli/run_skipmcp_test.go) | |
| tools-mcp.mcp-transports | MCP stdio / streamable-http / legacy-sse transports | tools-mcp | covered-deterministic | pkg/backend/mcp/browser_test.go, pkg/backend/delegate/pi_mcp_test.go | |
| tools-mcp.mcp-oauth | MCP OAuth broker / PKCE wiring | tools-mcp | covered-deterministic | pkg/backend/mcp/oauth_test.go | a real third-party OAuth consent flow is excluded (see integrations.third-party-oauth) |
| tools-mcp.board-tools | board capability tools over stdio, HTTP and in-process claw | tools-mcp | covered-deterministic | TestBoardDispatcher_E2E_BotCreatesAndDispatches (e2e/board_dispatcher_test.go), pkg/backend/tool/claw_board_tools_test.go | |
| tools-mcp.ask-user | ask_user / ask_user_async / await_answers MCP surface | tools-mcp | covered-deterministic | pkg/askusermcp/http_test.go, TestAwaitAnswersReleasedByAnswer (e2e/async_interaction_test.go) | |
| tools-mcp.permission-gate | permission gate blocks/asks on a non-allow-listed tool call | tools-mcp | covered-deterministic | pkg/backend/model/permission_gate_test.go, pkg/backend/permission/permission_test.go | live variants: TestLive_Feat_Permission_Deny / _Ask |
| tools-mcp.secret-guard | secret placeholders never leak into tool input/output | tools-mcp | covered-deterministic | pkg/backend/model/hooks_secretguard_test.go, pkg/backend/model/secretguard_binding_hosts_test.go | |
| tools-mcp.computer-use | read_image / screenshot / computer_use dispatch | tools-mcp | covered-deterministic | pkg/backend/tool/claw_builtins_test.go | headless-unavailable propagation is the deterministic half; live use is TestLive_Lite_ClawReadImage |
| tools-mcp.tool-display | human-readable rendering of tool calls in console/report | tools-mcp | unit-only | pkg/backend/tooldisplay/tooldisplay_test.go | pure formatter over an event payload; an e2e would assert string shape through the whole stack without adding risk coverage |
| observability.report-generation | `report.md` chronological rendering from events + artifacts | observability | covered-deterministic | TestReportRendersChronologicalRunReport (e2e/cli_report_test.go) | the artifact table lifts each artifact's conventional `summary:` field; node outputs without one are intentionally not inlined |
| observability.metrics | benchmark.CollectMetrics over a finished run | observability | covered-deterministic | TestCIFix_HappyPath (e2e/e2e_test.go), pkg/benchmark/benchmark_test.go | |
| observability.alerts | stall/budget/failure alerting + liveness heartbeat | observability | covered-deterministic | pkg/alert/manager_test.go | |
| observability.completion-webhooks | run-completion webhooks behind the SSRF guard | observability | covered-deterministic | pkg/notify/completion_test.go, pkg/secure/httpdial/httpdial_test.go | |
| observability.user-notifications | run-outcome web-push notifications with per-episode dedup | observability | covered-deterministic | pkg/usernotify/dispatcher_test.go | |
| observability.otlp-tracing | OTLP exporter wiring for runs/server | observability | covered-deterministic | pkg/cloud/tracing/tracing_test.go, pkg/benchmark/otlp_test.go | an end-to-end span assertion needs a real collector; setup/shutdown/no-endpoint paths are asserted here |
| sandbox.docker-driver | docker driver: container lifecycle, workspace bind-mount | sandbox | covered-live | e2e/live_feat_sandbox_net_test.go | needs a container runtime; the deterministic half is spec construction (pkg/sandbox/spec_test.go) |
| sandbox.spec-resolution | sandbox spec resolution (auto / devcontainer / image / none) | sandbox | covered-deterministic | pkg/sandbox/spec_test.go, pkg/sandbox/factory_test.go | |
| sandbox.network-policy | `network: allowlist/denylist` CONNECT proxy enforcement | sandbox | covered-live | e2e/live_feat_sandbox_net_test.go | enforcement requires a live proxy + container; policy parsing is in pkg/sandbox/spec_test.go |
| sandbox.host-state | `host_state:` auto / none mounts of `~/.iterion` and `~/.claude` | sandbox | covered-deterministic | pkg/sandbox/spec_test.go | |
| sandbox.kubernetes-driver | kubernetes driver for cloud runner pods | sandbox | excluded | | needs a live cluster + registry; no fake apiserver harness exists in this repo |
| sandbox.buildkit | `sandbox.build:` via docker buildx on the local driver | sandbox | excluded | | needs a Docker daemon with BuildKit; rejected by design on the k8s driver |
| plugins.registry | plugin discovery, enable state, builtin embedding | plugins | covered-deterministic | pkg/plugin/plugin_test.go, pkg/plugin/inspect_test.go | |
| plugins.install | `plugin install` from a git URL or path (incl. bare skills repos) | plugins | covered-deterministic | pkg/plugin/install_test.go, pkg/plugin/skilllib_test.go | |
| plugins.rewriters | rewriter chain rewrites shell commands (rtk compression) | plugins | covered-deterministic | pkg/plugin/plugin_test.go, pkg/dsl/ir/compress_test.go | live end-to-end with the rtk binary: TestLive_Feat_Compress |
| plugins.contributed-skills | plugin skills/commands/agents mirrored into the workspace | plugins | covered-deterministic | TestMirrorInjectedPluginFiles_WritesEachKind (pkg/runtime/contributions_test.go) | |
| plugins.hooks-merge | plugin hook fragments idempotently merged into settings.json | plugins | covered-deterministic | pkg/plugin/lifecycle_test.go, pkg/backend/model/settings_hooks_test.go | |
| plugins.private-source | team-scoped private plugin source binding (git + secret ref) | plugins | covered-deterministic | pkg/pluginsource/pluginsource_test.go | |
| server-api.run-console | run console REST: run detail, events, log, node sections | server-api | covered-deterministic | pkg/server/runs_test.go, pkg/runview/service_test.go | |
| server-api.run-launch | launch a run over HTTP with vars/overrides | server-api | covered-deterministic | pkg/runview/service_launch_budget_test.go | |
| server-api.run-control | pause / cancel / resume / rewind / bump-loop / raise-budget | server-api | covered-deterministic | pkg/runview/service_commands_test.go | |
| server-api.run-ws | live run WebSocket stream | server-api | covered-deterministic | pkg/server/runs_ws_test.go | |
| server-api.review-scope | review scope + diff since the previous human gate | server-api | covered-deterministic | pkg/server/runs_review_scope_test.go | |
| server-api.run-files | run workspace file browse/read/write with path containment | server-api | covered-deterministic | pkg/server/runs_files_test.go | |
| server-api.run-commits | per-run commit list + commit detail | server-api | covered-deterministic | pkg/server/runs_commits_test.go | |
| server-api.preview-proxy | run preview proxy behind the SSRF guard | server-api | covered-deterministic | pkg/server/runs_preview_test.go | |
| server-api.answer-human | answer a human gate / async question over HTTP | server-api | covered-deterministic | pkg/server/runs_answer_uploads_test.go | |
| server-api.queued-messages | operator chat messages queued into a running node's inbox | server-api | covered-deterministic | pkg/backend/model/inbox_test.go, pkg/server/runs_steer_test.go | |
| server-api.bots | bot catalog listing + per-bot metadata over HTTP | server-api | covered-deterministic | pkg/server/bots_routes_test.go, pkg/botregistry/registry_test.go | |
| server-api.board | native kanban REST (CRUD, transitions, labels, views) | server-api | covered-deterministic | pkg/dispatcher/native/http_test.go, pkg/dispatcher/native/store_test.go | |
| server-api.dispatcher-dashboard | dispatcher state/refresh/cancel HTTP surface | server-api | covered-deterministic | TestDispatcherE2E_HTTPSurface (e2e/dispatcher_test.go), pkg/dispatcher/http_test.go | |
| server-api.server-info | `/api/server/info` capability flags gate the SPA features | server-api | covered-deterministic | pkg/server/backends_routes_test.go | |
| server-api.openapi | generated OpenAPI 3.1 spec matches the wired routes | server-api | covered-deterministic | pkg/server/openapi_test.go | |
| server-api.local-secrets | `/api/local/secrets` desktop/CLI secret surface | server-api | covered-deterministic | pkg/server/local_skills_routes_test.go | |
| server-api.static-spa | embedded studio SPA served from the binary | server-api | covered-deterministic | pkg/server/spa_test.go | |
| dispatcher.poll-dispatch | poll a tracker and dispatch one run per eligible issue | dispatcher | covered-deterministic | TestDispatcherE2E_DispatchAndRelease (e2e/dispatcher_test.go) | |
| dispatcher.retry | retry with backoff after a failed run, give up at max attempts | dispatcher | covered-deterministic | TestDispatcherE2E_RetryAfterFailure (e2e/dispatcher_test.go), TestDispatcherGivesUpAfterMaxAttempts (pkg/dispatcher/dispatcher_test.go) | |
| dispatcher.cancel | cancel an in-flight dispatched run | dispatcher | covered-deterministic | TestDispatcherE2E_CancelInFlight (e2e/dispatcher_test.go) | |
| dispatcher.state-transitions | issue state transitions around dispatch + revert on failure | dispatcher | covered-deterministic | TestDispatch_TransitionsToInProgress (pkg/dispatcher/loop_state_test.go), TestDispatcherE2E_RespectsTerminalStateChange (e2e/dispatcher_test.go) | |
| dispatcher.hooks | lifecycle hooks (after_create/before_run/after_run/before_remove) | dispatcher | covered-deterministic | pkg/dispatcher/hooks_test.go, TestCleanupWorkspace_RunsBeforeRemoveBeforeDeletingDir (pkg/dispatcher/cleanup_workspace_test.go) | |
| dispatcher.per-ticket-bot | per-ticket bot override + assignee routing | dispatcher | covered-deterministic | TestBuildSpec_PerTicketBotSetsRouteKey (pkg/dispatcher/loop_bot_override_test.go) | |
| dispatcher.concurrency | per-state concurrency caps + claim conflict handling | dispatcher | covered-deterministic | TestDispatcherRespectsClaimConflict (pkg/dispatcher/dispatcher_test.go), TestDispatch_SlotCountedFromClaimTime (pkg/dispatcher/loop_setup_offload_test.go) | |
| dispatcher.cost-cap | daily cost cap gate blocks further dispatch | dispatcher | covered-deterministic | TestRefreshCostCapGatesWhenExceeded (pkg/dispatcher/cost_cap_test.go) | |
| dispatcher.workspace-cleanup | per-run workspace/worktree teardown policies | dispatcher | covered-deterministic | TestCleanupWorkspace_RemovesLinkedWorktreeRegistration (pkg/dispatcher/cleanup_workspace_test.go) | |
| dispatcher.native-tracker | native filesystem kanban tracker (board.json/issues/events) | dispatcher | covered-deterministic | pkg/dispatcher/native/store_test.go, TestNativeStore_Conformance (pkg/dispatcher/boardmongo/conformance_test.go) | |
| dispatcher.github-tracker | GitHub Issues tracker adapter | dispatcher | covered-deterministic | pkg/dispatcher/tracker/github_test.go | real GitHub API is stubbed at the HTTP boundary |
| dispatcher.forgejo-tracker | Forgejo/Gitea tracker adapter | dispatcher | covered-deterministic | pkg/dispatcher/tracker/forgejo_test.go | real Forgejo API is stubbed at the HTTP boundary |
| triggers.subscription-registry | subscription CRUD + query by repo / by bot | triggers | covered-deterministic | pkg/trigger/subscription_test.go | |
| triggers.evaluator | evaluator matches an event to subscriptions and launches | triggers | covered-deterministic | pkg/trigger/evaluator_test.go | |
| triggers.board-source | board transition promotes a card / direct-launches a bot | triggers | covered-deterministic | pkg/trigger/board_integration_test.go, TestIssueTriageTrigger_E2E_ConsumeAndLaunch (e2e/issue_triage_trigger_test.go) | |
| triggers.consume-labels | `consume_labels` strips the matcher labels atomically pre-launch | triggers | covered-deterministic | pkg/trigger/consume_labels_test.go, TestIssueTriageTrigger_E2E_ConsumeAndLaunch (e2e/issue_triage_trigger_test.go) | |
| triggers.run-outcome | run completion events chain the next bot | triggers | covered-deterministic | pkg/trigger/runoutcome_test.go | |
| triggers.scheduler | schedule-kind subscriptions tick on their cron | triggers | covered-deterministic | pkg/trigger/scheduler_test.go, pkg/trigger/scheduler_gate_test.go | |
| triggers.eventbus-inproc | in-process event bus delivery | triggers | covered-deterministic | pkg/eventbus/inproc_test.go | |
| triggers.eventbus-nats | NATS event bus on the separate ITERION_EVENTS stream | triggers | covered-deterministic | pkg/eventbus/nats_test.go | needs a NATS endpoint; skips cleanly without one |
| triggers.cloudsched | cloud recurring-bot scheduler with a multi-replica CAS ticker | triggers | covered-deterministic | pkg/cloudsched/cloudsched_test.go | |
| triggers.retry-policy | `usage_window` retry policy resolution across all layers | triggers | covered-deterministic | pkg/retrypolicy/policy_test.go | |
| webhooks.gitlab | GitLab MR open/reopen + `/revi` note re-review launches | webhooks | covered-deterministic | pkg/server/webhooks_gitlab_test.go, pkg/webhooks/webhooks_test.go | |
| webhooks.github | GitHub PR events launch the configured bot | webhooks | covered-deterministic | pkg/server/webhooks_github_test.go | |
| webhooks.forgejo | Forgejo/Gitea PR events launch the configured bot | webhooks | covered-deterministic | pkg/server/webhooks_forgejo_test.go | |
| webhooks.generic | generic JSON inbound trigger | webhooks | covered-deterministic | pkg/webhooks/router_test.go | |
| webhooks.auth | `iwh_` token / HMAC admission, rate limits, idempotent delivery | webhooks | covered-deterministic | pkg/webhooks/match_test.go, pkg/server/webhooks_routes_test.go | |
| webhooks.handoff | produces/consumes hand-off stamps the next run's launch vars | webhooks | covered-deterministic | pkg/server/webhooks_handoff_test.go, bots/handoff_declarations_test.go | |
| webhooks.merge-gate | Revi posts a deterministic `revi/review` commit status | webhooks | covered-deterministic | pkg/forge/reviews_test.go, pkg/server/forge_publish_test.go | |
| forge.connections | forge connection / repo-integration / OAuth-app stores | forge | covered-deterministic | pkg/forge/oauth_app_store_test.go | |
| forge.admin-clients | per-provider admin clients (repos, hooks, permissions) | forge | covered-deterministic | pkg/forge/forgelive_test.go, pkg/forge/command_map_test.go | provider APIs stubbed at the HTTP boundary |
| forge.github-app | GitHub App manifest flow + installation-token minting | forge | covered-deterministic | pkg/server/forge_app_resolution_test.go | the interactive App-creation consent screen is excluded (needs a real GitHub org) |
| forge.orchestrator | provision/deprovision: webhook + secret + bindings + schedules | forge | covered-deterministic | pkg/forge/orchestrator_test.go | |
| forge.token-refresh | background token refresh worker | forge | covered-deterministic | pkg/forge/refresh_test.go | |
| forge.config-share | shared-config read/write under a synthetic share grant | forge | covered-deterministic | pkg/configshare/configshare_test.go | |
| cloud.auth-session | login / logout / refresh / session cookies + JWT claims | cloud | covered-deterministic | pkg/auth/service_test.go, pkg/server/auth_routes_test.go | |
| cloud.sso-oidc | OIDC SSO providers + domain verification | cloud | covered-deterministic | pkg/auth/oidc/generic_test.go, pkg/server/org_sso_routes_test.go | a real IdP consent round-trip is excluded (see integrations.third-party-oauth) |
| cloud.password-reset | password reset request/confirm + mail delivery fallback | cloud | covered-deterministic | pkg/auth/password_reset_test.go, pkg/mail/mail_test.go | |
| cloud.orgs-teams | two-level tenancy: org membership, team scoping, context switch | cloud | covered-deterministic | pkg/identity/team_test.go, pkg/server/auth_teams_test.go | |
| cloud.invitations | team/org invitations: create, lookup, accept | cloud | covered-deterministic | pkg/server/auth_views_test.go | |
| cloud.pat | personal access tokens (`iap_` bearers) | cloud | covered-deterministic | pkg/pat/pat_test.go | |
| cloud.audit-log | tenant + platform audit log of control-plane mutations | cloud | covered-deterministic | pkg/audit/audit_test.go | |
| cloud.quotas | per-org monthly run/cost counters gate the launch | cloud | covered-deterministic | pkg/orgusage/orgusage_test.go, pkg/server/launch_gate_test.go | |
| cloud.queue-dispatch | NATS work queue: enqueue, claim, schema-version handling | cloud | covered-deterministic | pkg/queue/types_test.go, TestSchemaVersionMismatchIsATypedTransientError (pkg/queue/schema_version_transient_test.go) | |
| cloud.runner-pod | runner claims a queued run, executes, reports status back | cloud | covered-deterministic | pkg/runner/loop_test.go | |
| cloud.runner-credentials | credential injection + sealing into a runner pod | cloud | covered-deterministic | pkg/runner/git_credentials_test.go, pkg/secrets/run_secrets_test.go | |
| cloud.credential-pool | pledge/lease broker as the fourth credential tier | cloud | covered-deterministic | pkg/credpool/broker_test.go, pkg/server/cloudpublisher/credpool_tier_test.go | |
| cloud.secrets-sealing | AES-256-GCM sealing + BYOK/generic/bot-binding domains | cloud | covered-deterministic | pkg/secrets/sealer_test.go, pkg/secrets/run_secrets_test.go | |
| cloud.bot-sources | team-authored bot bundles (fork a catalog bot, author a new one) | cloud | covered-deterministic | pkg/botsource/botsource_test.go, pkg/server/bot_sources_routes_test.go | |
| cloud.marketplace | hosted registry entries (bot + plugin), moderation, visibility | cloud | covered-deterministic | pkg/marketplace/jsonstore_test.go | |
| cloud.memory-store | cloud MemoryStore adapter + quota | cloud | covered-deterministic | pkg/knowledge/scope_test.go, pkg/memory/space_test.go | |
| cloud.dlq | dead-letter queue show/replay/delete | cloud | uncovered | | `iterion remote admin dlq` and the server side have no test. Plan: enqueue a poisoned message against the memory queue and assert show → replay → delete |
| cloud.migrate-blobs | `migrate to-cloud` artifact/blob upload to S3 | cloud | uncovered | | needs an S3-compatible endpoint; plan: a minio/fake-S3 stub, or mark excluded once confirmed impractical |
| cloud.desktop-exchange | desktop auth token exchange endpoint | cloud | covered-deterministic | pkg/server/desktop_sso_test.go | |
| cloud.valkey-state | ephemeral cross-replica state (OAuth/CSRF, board tokens, rate buckets) | cloud | uncovered | | pkg/valkey has no tests at all; plan: a miniredis-backed round-trip for each of the three state kinds |
| bots.catalog-universality | catalog bots stay repo- and stack-agnostic | bots | covered-deterministic | bots/catalog_universality_test.go | |
| bots.catalog-compiles | every catalog bot parses + compiles clean | bots | covered-deterministic | bots/catalog_parse_compile_test.go, bots/catalog_typing_test.go | |
| bots.catalog-freshness | the generated bot-catalog skill matches the manifests | bots | covered-deterministic | bots/catalog_freshness_test.go | |
| bots.golden-replay | bot golden replay revalidates frozen LLM outputs | bots | covered-deterministic | TestGoldens (pkg/botreplay/goldens_test.go) | |
| bots.feature-dev | feature-dev (Featurly): campaign + gate convergence | bots | covered-deterministic | TestVibeFeatureDev_ConvergesFirstPass (e2e/feature_dev_test.go), TestVibeFeatureDev_RedVerifyRoutesBackToCampaign (e2e/feature_dev_test.go) | |
| bots.whole-improve-loop | whole-improve-loop (Willy) | bots | covered-deterministic | TestWholeImproveLoop_ContinuesUntilComplete (e2e/whole_improve_loop_test.go) | |
| bots.branch-improve-loop | branch-improve-loop (Billy) | bots | covered-deterministic | TestBranchImproveLoop_ContinuesUntilClean (e2e/branch_improve_loop_test.go) | |
| bots.docs-refresh | docs-refresh (Doki) | bots | covered-deterministic | TestDocsRefresh_ConvergesFirstPass (e2e/docs_refresh_test.go) | |
| bots.whats-next | whats-next (Nexie) chat loop + dispatch | bots | covered-deterministic | TestWhatsNextV2_ChatLoop_PauseResumeClose (e2e/whats_next_loop_test.go) | |
| bots.secured-renovacy | secured-renovacy (Renovacy) patch/minor/fix-loop paths | bots | covered-deterministic | TestSecuredRenovacy_PatchFastTrack (e2e/secured_renovacy_test.go) | |
| bots.sec-audit-source | sec-audit-source (Seki): cap_findings + scan_health gates | bots | covered-deterministic | TestSecAuditSource_ScanHealth_GuardsAgainstFacade (e2e/sec_audit_scan_health_test.go), TestSecAuditSource_CapFindings_BoundsScannerOutput (e2e/sec_audit_cap_findings_test.go) | |
| bots.sec-audit-deps | sec-audit-deps (Depsy): per-ecosystem + generic CVE heuristics | bots | covered-deterministic | TestSecAuditDeps_GenericHeuristic_DetectsMalwareSignals (e2e/sec_audit_deps_heuristics_test.go) | |
| bots.e2e-coverage | e2e-coverage (Endy): matrix gate + continuation loop | bots | covered-deterministic | TestE2ECoverage_ContinuesUntilComplete (e2e/e2e_coverage_bot_test.go), bots/e2e_coverage_matrix_gate_test.go | |
| bots.review-pr | review-pr (Revi): finding ids, stale anchors, gate status | bots | covered-deterministic | bots/review_pr_finding_id_test.go, bots/review_pr_stale_anchor_test.go | |
| bots.dep-update-guard | dep-update-guard (Vetty): prepare/gate/automerge routing | bots | covered-deterministic | bots/dep_update_guard_gate_test.go, bots/dep_update_guard_automerge_test.go | |
| bots.feed-watch | feed-watch: state machine, routing, SSRF-safe fetch | bots | covered-deterministic | TestFeedWatch_ScriptsStateMachine (e2e/feed_watch_test.go), TestFeedWatch_FetchRejectsSSRF (e2e/feed_watch_test.go) | |
| bots.issue-triage | issue-triage (Triagy): author trust gate + label consumption | bots | covered-deterministic | TestIssueTriageTrigger_E2E_ConsumeAndLaunch (e2e/issue_triage_trigger_test.go) | |
| bots.golden-master | golden-master harness sync invariants | bots | covered-deterministic | bots/golden_master_harness_sync_test.go | |
| bots.review-topology | mono/dual review topology injected only into opting-in bots | bots | covered-deterministic | TestReviewTopology_MonoClaudeSingleFamily (e2e/review_topology_test.go) | |
| bots.verify-gate | the deterministic verify_build/verify_run gate resists drift | bots | covered-deterministic | bots/verify_run_drift_test.go, bots/verify_probe_wiring_test.go | |
| bots.remaining-catalog | the remaining catalog bots (evolve, adr-*, rgaa-audit, bmady, devbox-setup, modernize, wiki-gen, app-dev, supply-shield*, smoke, revi-converse, feature-gap-fill, test-coverage) | bots | covered-live | e2e/live_bot_adr_cartograph_test.go, e2e/live_bot_evolve_test.go, e2e/live_bot_rgaa_audit_test.go, e2e/live_bot_bmady_test.go, e2e/live_bot_devbox_setup_test.go, e2e/live_bot_feature_gap_fill_test.go, e2e/live_bot_test_coverage_test.go | these bots' value IS the LLM work product (a review, an ADR map, an accessibility audit); they have no deterministic graph gate to assert against, so the live layer + quality panel is the honest coverage. Their graphs are compile-checked by bots.catalog-compiles |
| studio-ui.run-console | run console view: timeline, node detail, diffs, chat | studio-ui | uncovered | | vitest covers the API/hook layer (studio/src/api/runs.lifecycle.test.ts); there is no browser harness in this repo. Plan: introduce Playwright against `iterion studio` and cover the console flow first |
| studio-ui.launch-modal | Launch modal: bot picker, vars, overrides, target repo | studio-ui | uncovered | | same gap as studio-ui.run-console |
| studio-ui.board | `/board` kanban with drag-and-drop | studio-ui | uncovered | | same gap; the underlying REST is covered by server-api.board |
| studio-ui.pipelines | `/pipelines` control-center board + concurrency cap | studio-ui | uncovered | | vitest covers studio/src/api/pipelineBoards.test.ts; no browser flow test |
| studio-ui.dispatcher | `/dispatcher` live dashboard | studio-ui | uncovered | | same gap; the REST is covered by server-api.dispatcher-dashboard |
| studio-ui.bots-gallery | `/bots` gallery, per-bot home, guided builder `/bots/new` | studio-ui | uncovered | | the scaffold engine is covered (pkg/botscaffold); the guided-builder UI flow is not |
| studio-ui.editor | workflow editor + `/api/parse` round-trip | studio-ui | uncovered | | the parse/unparse round-trip is covered (dsl.unparse-roundtrip); the editor UI is not |
| studio-ui.secrets-view | Secrets view gated on `server_info.secrets_enabled` | studio-ui | uncovered | | REST covered by server-api.local-secrets; UI flow not |
| studio-ui.browser-pane | Browser pane / preview attach | studio-ui | uncovered | | preview proxy covered by server-api.preview-proxy; UI pane not |
| desktop.wails-app | Wails desktop wrapper (window, runtime bridge, packaging) | desktop | unit-only | studio/src/__tests__/desktopBridge.test.ts | the Go half is cgo + build-tagged (`desktop,webkit2_41`) and excluded from lint/CI; an e2e needs a GUI session — the bridge contract is the testable seam |
| integrations.third-party-oauth | real third-party OAuth consent flows (GitHub App, IdP, MCP OAuth) | integrations | excluded | | needs a real provider tenant and a human consent screen; iterion's side (state/PKCE, callback handling, token storage) is covered by cloud.sso-oidc, forge.github-app and tools-mcp.mcp-oauth |
| integrations.claude-md-autoload | claw TUI CLAUDE.md auto-load | integrations | excluded | | a claw-code-go TUI-boot behaviour with no iterion workflow surface; iterion uses its own command registry (carried over from docs/e2e_coverage.md) |
| integrations.claw-lifecycle-hooks | claw lifecycle hooks / ctx propagation into hook handlers | integrations | excluded | | iterion installs no `lifehooks.Runner` on its claw client, so there is no iterion-level behaviour to exercise; wiring one is a separate feature (carried over from docs/e2e_coverage.md) |
| integrations.permission-modes-auto | claw permission modes `auto` / `dontAsk` | integrations | excluded | | iterion runs headless (workflow mode) and never surfaces these modes; the two it does use are covered by tools-mcp.permission-gate |
</content>
</invoke>
