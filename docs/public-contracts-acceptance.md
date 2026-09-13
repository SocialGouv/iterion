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
| Four views in one source model | AST, source/JSON/unparse round trips; every field retained | DSL/AST and a real Studio public edit round trip pass; full authoring/inspection consistency remains outstanding |
| Public inputs, outputs, files, effects, criteria | Resolved contracts, deterministic validators, actionable diagnostics | AST, parser, resolved types and deterministic validators pass; native file publication passes on FS/Mongo/S3; effect-verifier execution remains outstanding |
| Multiple typed inputs and explicit public/product exports | Supplier, type, optional/default/null/empty and product validation cases | Compiler and Engine cases pass on FS/Mongo, including connected optional absence and physical files |
| Native acyclic data graph, including crossed diamonds | Compiled graph inspection and runtime dependency traces | Crossed DAG and committed-producer waiting pass through the actual Engine on FS/Mongo |
| Automatic one-axis map, scalar broadcast, whole-array transport | 0/1/N, ambiguous axes, limits and stable order cases | Compiler cases and Engine 0/1/N, broadcast, whole-array collection, limits and order pass on FS/Mongo; file outputs outstanding |
| Existing Engine and admission seams | Shared root budgets/resources/effects, nested concurrency and cancellation cases | Single-root scheduling, iteration reservation, file handling and strict cancellation pass; nested root admission and verifier execution outstanding |
| Validate artifacts before publishing outputs | Missing/invalid/stale files; required unconsumed product prevents success | Immutable file capture, scratch-shadow isolation and runtime freshness/declared-file checks pass on FS/Mongo/S3; actual process-kill recovery remains outstanding |
| Durable invocation identity and atomic publication | Filesystem and real Mongo replica-set crash injection cases | Store and Engine value publication/acknowledgment fault injection pass on FS/Mongo; physical file validation and actual process termination outstanding |
| Pause, cancel, crash and compatible resume | Persisted states, valid reuse, descendant invalidation, uncertain effect recovery | Engine pause/cancel/resume, selective source invalidation, interrupted CAS and manual/idempotent effect decisions pass on FS/Mongo; verifier execution, file recovery and complete source closure outstanding |
| Full native storage namespace and old-writer exclusion | Actual supported old mutators cannot change native closure or blobs | Store routing, no-shadow-fallback and actual old FS/Mongo/S3 executable checks pass; workspaces and deployment protection outstanding |
| Versioned queue and semantic identity | Delayed work, mixed consumers and no forced semantic downgrade | Queue v15 rejects older executable consumers; Engine refuses interpreter changes, including forced resume; complete consumer inventory outstanding |
| Capability census and activation barrier | Positive local/distributed activation, unknown/stale refusals, epoch invalidation | Local scope proof and store-bound admission pass; trusted distributed fleet/queue census and access reconciliation outstanding |
| Rollback | New root launches stop; existing compatible executions remain resumable | Local deactivation and admitted-run continuation pass; distributed rollback outstanding |
| Native composition and verified legacy adapters | Captured child dependencies, inherited policies, unchanged legacy traces | Unverified nested/control nodes now fail compilation; composition and adapters outstanding |
| Incomplete conversion assistance | Draft remains incomplete until required mappings/effects/guarantees verified | Outstanding |
| Studio/API/CLI/Copi | Actual browser and API document round trips, four views, map/cost visibility | Studio public/technical graph editing, API/CLI public projection and real Chromium save pass; Copi, conversion, runtime cost visibility and composition outstanding |
| Registry and authoring documentation | Parser/registry/EBNF conformance, generated docs and skills | Passing for the contract/compiler layer; further surfaces outstanding |
| Legacy non-regression | Corpus plus deterministic order/count/budget/checkpoint/empty-fanout traces | Outstanding |
| Shorts/Town/Tabarria representative pilots | Committed thresholds before measurements, equivalent legacy baseline and conversion report | Version-3 structural slices pass 9/9 after threshold `3d933d067` and fixture `9e627ac0e`; 13/13 named cases pass with and without race instrumentation. Native is slower on short fake jobs; full source conversion, media outputs and measured AI cost remain outstanding |
| Required tests really execute | Real Mongo and Playwright; expected-case manifest rejects missing/skipped cases | Storage/Engine manifests pass with real Mongo; all 33 Playwright Chromium cases now pass, including the native Studio round trip; complete feature acceptance remains outstanding |
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
  `scripts/verify-port-tests.mjs` verified all 70 expected cases in
  `pkg/store/storetest/native_namespaces.json` passed without skips. They cover
  metadata and blob round trips, exact public inputs, descendant namespaces,
  no legacy shadow fallback, unsupported-record refusal and deletion closure.
- Actual executables built from pinned main `3872f9dd1d1cbf9c18ed338c66c9f55aaaea3fcc`
  cannot read native runs through their legacy namespace. Their writes,
  repairs, deletion and pruning preserve native filesystem bytes, Mongo BSON
  records and S3 objects. Old CLI inspect/fork/rewind reach the missing-run
  check; old CLI pruning preserves the native directory. Mongo tests use a
  real replica set and both versions' real S3 clients against a disposable
  HTTP object fixture. This does not certify old queue consumers or workspace
  maintenance. Reproduction: [compatibility tests](public-contracts-testing.md).
- The native checkpoint persists captured identities, invocation attempts,
  ordered collections, exact public values, file references and root budget
  reservations in the existing run CAS. Mongo native reads/writes explicitly
  use majority read/write concern with journaling. FS uses the existing
  fsynced temporary-file, rename and directory-fsync commit.
- `TestNativeExecutionStateFilesystem` and `TestNativeExecutionStateMongo`
  pass with race detection. Injected loss immediately before the store commit
  leaves staged artifacts unpublished; loss after commit permits an identical
  acknowledgment retry without another budget charge. Competing coordinators
  cannot overwrite each other. Native values survive ordinary metadata writes
  without numeric coercion or whitespace/HTML-escaping identity changes.
  An uncertain effect requires recovery evidence tied to its exact attempt.
  These tests exercise the storage boundary; they do not yet exercise Engine
  scheduling, external effects, physical file validation or actual process
  termination inside a filesystem/Mongo write.
- Full `pkg/store`, `pkg/store/blob` and real-Mongo `pkg/store/mongo`
  suites pass with `-race -json -count=1`, including shared legacy
  conformance and the deletion-inventory guard. The manifest verified all
  58 required native cases without skips. A subsequent narrow change made
  Mongo scratch-directory creation check native record compatibility before
  mutation; the FS/Mongo unsupported-record cases passed again with race
  detection. `go vet` passed for the store, storetest and Mongo packages.
- `TestNativeFileCaptureFilesystem`, `TestNativeFileCaptureMongo` and
  `TestNativeFileCaptureMongoS3` pass with race detection. The last uses a real
  S3 client against the HTTP object fixture. Native capture streams into a
  private verified snapshot, retains content-addressed attempt references and
  refuses truncated, oversized or changed bodies before replacing any result.
  Native reads and the final scratch upload cannot substitute a scratch
  shadow for a captured file. Run deletion removes captured files and private
  staging. Capturing a file does not itself publish a workflow result.
  The native manifest verifies 70 cases without skips; the focused rerun also
  covers ordinary run-file uploading, S3 client round trips and deletion
  collection coverage. Separate Engine cases now check fresh producer
  provenance and declared file properties; process-kill recovery remains open.

- Native activation records and immutable admissions are bound to the
  canonical filesystem root or Mongo backend identity. Copying proof or an
  accepted run to another store cannot authorize execution. Local CLI
  `contracts inspect`, `probe`, `activate` and `deactivate` are wired; no
  trusted distributed fleet census exists yet.
- `TestValidateNativeWorkflowReturnsPublicProjection` and
  `TestPublicViewExposesContractsWithoutTechnicalConfiguration` pass. The
  generated OpenAPI and TypeScript schemas type `/api/validate`'s native
  public graph; `iterion validate --json` exposes the same projection.
- A real Chromium e2e case edits a native responsibility, saves the `.bot`,
  and reloads it with graph bindings and technical policy intact. The source
  equivalence check ignores diagnostic spans and JSON formatting while
  keeping absent, null and empty values distinct.
- The complete Playwright Chromium suite passed 33/33 cases on the rebuilt
  branch binary. A preexisting launch-caption assertion was updated to accept
  both the curated fallback and the cached aggregator source; both retain the
  required context and unknown-price behavior.

## Verification rules

The runtime value layer now has 60 mandatory named cases in
`pkg/runtime/ports_cases.json`, all passing with `-race` against the actual
filesystem store and Mongo 8 replica set. Its Mongo artifact client is a real
S3 client connected to a disposable HTTP object fixture. These cases execute
the parser, compiler and Engine, rather than replaying individual executors.
They cover map 0/1/4, reverse completion order, multi-input readiness, crossed
dependencies, connected optional absence, root iteration reservations,
correction before publication and exact integer arithmetic above 2^53.
Recovery covers pause, cancellation winning against finalization/admission,
unchanged successes, selective invalidation after a technical source change,
and manual/idempotent effect decisions tied to an exact attempt.
Six injected commit boundaries run on both stores, including lost acknowledgment
after successful publication. These are injected Store errors, not process kills.

The existing correction, compute, compiler and native-store transition tests
also pass after the shared seam changes. Full `pkg/dsl/expr`, `pkg/dsl/ir` and
`pkg/backend/model` pass with race detection. The full legacy corpus and final
repository checks remain outstanding.

The native runtime remains incomplete: effect verifiers, nested root admission,
verified composition and adapters, distributed access census and full-source
pilots are outstanding. File capture, local activation and product publication
are wired into the native Engine; those passing cases do not authorize a
distributed rollout or finish #1165.

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
