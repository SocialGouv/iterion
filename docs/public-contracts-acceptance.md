# Public contracts — implementation and acceptance for #1165

Scope: the complete [issue #1165](https://github.com/SocialGouv/iterion/issues/1165),
implemented on a branch from main `3872f9dd1d1cbf9c18ed338c66c9f55aaaea3fcc`.
This matrix tracks incomplete work explicitly. A passing layer is not evidence
that the feature, its deployment barrier, or its pilots are complete.

The authoritative source remains the existing `.bot` document. The four views
are its public workflow contract, the graph, public component contracts and
technical configuration. The native runtime opt-in is
`runtime_semantics: "ports-v1"`; an absent marker retains legacy semantics.
Syntax profiles and runtime semantics are independent.

| Requirement | Evidence required | Current state |
| --- | --- | --- |
| Four views in one source model | AST, source/JSON/unparse round trips; every field retained | DSL/AST round trips pass; full interface round trips outstanding |
| Public inputs, outputs, files, effects, criteria | Resolved contracts, deterministic validators, actionable diagnostics | AST, parser, resolved types and deterministic value/criterion validators pass; runtime publication outstanding |
| Multiple typed inputs and explicit public/product exports | Supplier, type, optional/default/null/empty and product validation cases | Compiler cases pass; runtime cases outstanding |
| Native acyclic data graph, including crossed diamonds | Compiled graph inspection and runtime dependency traces | Crossed DAG compilation and cycle rejection pass; runtime traces outstanding |
| Automatic one-axis map, scalar broadcast, whole-array transport | 0/1/N, ambiguous axes, limits and stable order cases | Map inference, shared-axis reuse, whole-array typing and ambiguous-axis rejection pass; execution outstanding |
| Existing Engine and admission seams | Shared root budgets/resources/effects, nested concurrency and cancellation cases | Outstanding |
| Validate artifacts before publishing outputs | Missing/invalid/stale files; required unconsumed product prevents success | Outstanding |
| Durable invocation identity and atomic publication | Filesystem and real Mongo replica-set crash injection cases | Outstanding |
| Pause, cancel, crash and compatible resume | Persisted states, valid reuse, descendant invalidation, uncertain effect recovery | Outstanding |
| Full native storage namespace and old-writer exclusion | Actual supported old mutators cannot change native closure or blobs | Store routing and no-shadow-fallback checks pass on FS/Mongo; old executable tests, workspaces and deployment protection outstanding |
| Versioned queue and semantic identity | Delayed work, mixed consumers and no forced semantic downgrade | Outstanding |
| Capability census and activation barrier | Positive local/distributed activation, unknown/stale refusals, epoch invalidation | Outstanding |
| Rollback | New root launches stop; existing compatible executions remain resumable | Outstanding |
| Native composition and verified legacy adapters | Captured child dependencies, inherited policies, unchanged legacy traces | Outstanding |
| Incomplete conversion assistance | Draft remains incomplete until required mappings/effects/guarantees verified | Outstanding |
| Studio/API/CLI/Copi | Actual browser and API document round trips, four views, map/cost visibility | Outstanding |
| Registry and authoring documentation | Parser/registry/EBNF conformance, generated docs and skills | Passing for the contract/compiler layer; further surfaces outstanding |
| Legacy non-regression | Corpus plus deterministic order/count/budget/checkpoint/empty-fanout traces | Outstanding |
| Shorts/Town/Tabarria representative pilots | Committed thresholds before measurements, equivalent legacy baseline and conversion report | Outstanding |
| Required tests really execute | Real Mongo and Playwright; expected-case manifest rejects missing/skipped cases | Outstanding |
| Reviewable PR targeting main | Layered commits, scoped diff, current PR checks/review and evidence links | Outstanding |

## Evidence recorded during implementation

- Go `1.26.2` and Node `24.12.0` verified through `devbox run` in the
  repository-pinned devbox container.
- `go test ./pkg/dsl/ast ./pkg/dsl/parser ./pkg/dsl/unparse`: passing.
  The new `TestPublicContractsSurviveSourceAndEditorRoundTrips` exercises both
  syntax profiles and the complete editor JSON transport, including absent,
  null and empty defaults, large integers and technical policies.
- `TestPublicContractSourceDiagnosticsRetainFileAndLine` and
  `TestPublicContractJSONRejectsNilDeclarations`: passing.
- Parser/registry/EBNF conformance and generated-document freshness: passing.
  Documents regenerated with the repository's `task dsl:gen`.
- `go test ./pkg/dsl/ast ./pkg/dsl/parser ./pkg/dsl/ir ./pkg/dsl/spec
  ./pkg/dsl/unparse ./pkg/backend/model`: passing, including enum compile
  probes and generated-document freshness. Native output schema conformance
  preserves requiredness, nullability, nested shapes and exact large integers;
  it performs no implicit numeric or string coercion.
- `TestPortGraphCompilesMapBroadcastAndWholeArrayCollection`,
  `TestPortGraphReusesAnAxisWhenOneArrayFeedsTwoScalarPorts`,
  `TestPortGraphCompilesCrossedDependenciesWithoutControlJoins`,
  `TestPortGraphProductCanAlsoFeedAConsumer`,
  `TestPortGraphRejectsInvalidBindingsAndExportsWithPositions`,
  `TestPortGraphDetectsCyclesBeforeExecution`, and
  `TestLegacyWorkflowKeepsControlSemantics`: passing. These inspect compiled
  graphs and diagnostics; they are not runtime execution evidence.
- `TestPublicJSONRejectsAmbiguousObjects`: passing. Public defaults and
  criterion parameters reject duplicate JSON members as well as trailing
  values, retaining large numeric values without rounding.

- Store routing now uses `pc1_` IDs, `port_runs_v1/<id>/run.ports-v1.json`,
  Mongo collection families suffixed `_ports_v1`, and blob keys prefixed
  `ports-v1/`. Native run-file scratch uses a distinct directory family.
  Explicit creation identity is required; full saves cannot upsert a missing
  native run or change its interpreter. These are storage guarantees, not
  evidence that the native scheduler or deployment barrier is implemented.
- Native store tests pass on filesystem and a real Mongo 8 replica set with
  `ITERION_TEST_REQUIRED=1`, `ITERION_TEST_MONGO_URI` and `go test -race -json`.
  `scripts/verify-port-tests.mjs` verified all 28 expected cases in
  `pkg/store/storetest/native_namespaces.json` passed without skips. They cover
  metadata and blob round trips, exact public inputs, descendant namespaces,
  no legacy shadow fallback, unsupported-record refusal and deletion closure.
- Full `pkg/store` and `pkg/store/blob` suites pass. The real-Mongo shared
  legacy conformance suite passed. The subsequent full Mongo package run
  found a static deletion-inventory parser that did not recognize the new
  namespace selector; that guard was updated to require routed collections
  and its focused rerun passed. Final broader verification remains required.

## Verification rules

Run Go 1.26 and Node 24 through the repository's `devbox run` environment.
Use deterministic executors and declared fake effects; no paid model call is
necessary to test scheduling. Real Mongo and browser prerequisites are mandatory
for their required suites. Missing prerequisites are incomplete verification.

Record named tests and actual results in this matrix and the e2e coverage matrix
as each layer is verified. Do not infer a scheduler guarantee from syntax tests
or individual node replay. Pilot thresholds and equivalence rules must be
committed before measuring; the report must identify that commit. Unknown model
tokens or charges remain unknown.

No production deployment, production workflow modification, live paid pilot,
mass migration, PR merge or legacy removal belongs to this implementation task.
