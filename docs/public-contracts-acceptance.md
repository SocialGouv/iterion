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
| Public inputs, outputs, files, effects, criteria | Resolved contracts, deterministic validators, actionable diagnostics | AST, parser, resolved types and deterministic validators pass; native file publication passes on FS/Mongo/S3; attempt-bound verifier replay passes with a capable executor on FS/Mongo, while production verifier implementations remain outstanding |
| Multiple typed inputs and explicit public/product exports | Supplier, type, optional/default/null/empty and product validation cases | Compiler and Engine cases pass on FS/Mongo, including connected optional absence and physical files |
| Native acyclic data graph, including crossed diamonds | Compiled graph inspection and runtime dependency traces | Crossed DAG and committed-producer waiting pass through the actual Engine on FS/Mongo |
| Automatic one-axis map, scalar broadcast, whole-array transport | 0/1/N, ambiguous axes, limits and stable order cases | Compiler cases and Engine 0/1/N, broadcast, whole-array collection, limits and order pass on FS/Mongo; native file outputs are separately captured and validated |
| Existing Engine and admission seams | Shared root budgets/resources/effects, nested concurrency and cancellation cases | Single-root scheduling, iteration reservation, file handling, strict cancellation and verifier capability refusal pass; nested root admission and production verifier wiring outstanding |
| Validate artifacts before publishing outputs | Missing/invalid/stale files; required unconsumed product prevents success | Immutable file capture, scratch-shadow isolation and runtime freshness/declared-file checks pass on FS/Mongo/S3; real process kill after the first captured byte leaves no published file and recovers on FS/Mongo |
| Durable invocation identity and atomic publication | Filesystem and real Mongo replica-set crash injection cases | Store and Engine publication/acknowledgment fault injection pass on FS/Mongo; real child-process SIGKILL after effect dispatch and during file capture passes on both stores. The local orphan scan recognizes native checkpoints; cloud lease adoption remains to be proven end to end |
| Pause, cancel, crash and compatible resume | Persisted states, valid reuse, descendant invalidation, uncertain effect recovery | Engine pause/cancel/resume, selective source and corrupt-file invalidation, interrupted CAS and manual/idempotent/verified effect decisions pass on FS/Mongo; real process-kill recovery passes on both stores after the explicit orphan status transition; complete child source closure remains outstanding |
| Full native storage namespace and old-writer exclusion | Actual supported old mutators cannot change native closure or blobs | Store routing, no-shadow-fallback and actual old FS/Mongo/S3 executable checks pass; workspaces and deployment protection outstanding |
| Versioned queue and semantic identity | Delayed work, mixed consumers and no forced semantic downgrade | Queue v15 rejects older executable consumers; Engine refuses interpreter changes, including forced resume. Runner now treats a queued native run with durable `PortExecution` as a resume on redelivery; complete consumer inventory and real mixed-fleet delivery proof remain outstanding |
| Capability census and activation barrier | Positive local/distributed activation, unknown/stale refusals, epoch invalidation | Local scope proof and store-bound admission pass; production Mongo now refuses manually populated distributed evidence without a trusted verifier. NATS system/Kubernetes census, ACL reconciliation and positive distributed activation remain outstanding |
| Rollback | New root launches stop; existing compatible executions remain resumable | Local deactivation, two-build continuation and inherited admission for native descendants after disablement or proof expiry pass; distributed rollback outstanding |
| Native composition and verified legacy adapters | Captured child dependencies, inherited policies, unchanged legacy traces | An internal legacy-control adapter child now runs and resumes in the native namespace under its parent's immutable admission on FS/Mongo; public adapter verification, child-source capture and shared root admission remain outstanding |
| Incomplete conversion assistance | Draft remains incomplete until required mappings/effects/guarantees verified | Legacy validation now returns sorted candidate inputs/nodes with explicit unresolved mapping, effect and file gaps; verified conversion and adapters remain outstanding |
| Studio/API/CLI/Copi | Actual browser and API document round trips, four views, map/cost visibility | Studio public/technical graph editing, API/CLI public projection, MCP local read/write and validation, and real Chromium save pass. MCP offers registry-backed public syntax with technical kinds and complete source on demand; Studio distinguishes mapped, whole-array and broadcast bindings. CLI inspect and MCP run get summarize native invocation/map/product status and reported usage, with unknown cost called out. A real Copi authoring session, bundles, full runtime cost attribution and composition remain outstanding |
| Registry and authoring documentation | Parser/registry/EBNF conformance, generated docs and skills | Passing for the contract/compiler layer; further surfaces outstanding |
| Legacy non-regression | Corpus plus deterministic order/count/budget/checkpoint/empty-fanout traces | Full `task test` passes with its declared shell prerequisites installed. Actual pinned-main and current binaries produce equal deterministic status, count, budget, checkpoint, join and empty-fanout traces. All 117 `.bot` files in the three reference projects validate with unchanged diagnostics; project-specific execution traces remain outstanding |
| Shorts/Town/Tabarria representative pilots | Committed thresholds before measurements, equivalent legacy baseline and conversion report | Version-3 structural slices pass 9/9 after threshold `3d933d067` and fixture `9e627ac0e`; 13/13 named cases pass with and without race instrumentation. Native is slower on short fake jobs; full source conversion, media outputs and measured AI cost remain outstanding |
| Required tests really execute | Real Mongo and Playwright; expected-case manifest rejects missing/skipped cases | Race-instrumented manifests pass 72 store, 75 Engine and 3 legacy-trace cases with real Mongo and pinned old binaries; all 33 Playwright Chromium cases pass, including the native Studio round trip; complete feature acceptance remains outstanding |
| Reviewable PR targeting main | Layered commits, scoped diff, current PR checks/review and evidence links | Outstanding |

## Evidence recorded during implementation

- Inherited native child admission follows a bounded, ID-linked run lineage to the
  originally admitted root. A new root still needs a fresh activation, while
  descendants inherit the root's immutable proof after Disable or expiry.
  Native children independently admitted by older launchers retain their own
  valid proof after upgrade. A compatible version-zero parent can create a
  child with a current-format inherited admission, and a 33rd run-tree level
  is refused before insertion.
  The internal `legacy-adapter-v1` interpreter executes legacy control flow
  within the native run namespace; a paused human gate resumes under a fresh
  Engine on filesystem and real Mongo without a native ports checkpoint.
  A public `LaunchSpec.ParentRunID` cannot claim this inheritance without a
  verified composition path. The CLI preflight accepts an already admitted
  native row after rollback, but still refuses a new root. This does not yet
  expose a verified public adapter node or share root budgets and permits with
  executable descendants.
  The race-instrumented Engine manifest verifies 75 cases and the activation
  manifest verifies 33 cases without skips, with real Mongo, NATS and pinned
  legacy executables where required.
- The NATS capability heartbeat now lets an already-started broker PUT return
  under its bounded operation timeout before cleanup, rather than discarding
  its revision solely because shutdown began. Its real-broker cancellation test
  passed 40 consecutive race-instrumented runs; the full NATS package and
  `go vet` passed afterward.

- Studio's native connection picker now excludes known contradictions in
  requiredness, nullability, array bounds and file media types, plus duplicate
  suppliers, self/dependency cycles and a second inferred map axis. Existing
  connections remain visible. The compiler remains authoritative for resolved
  schema equivalence and full graph validation. Fourteen targeted frontend
  tests pass, including a real editor-store binding action; TypeScript and
  scoped ESLint pass. The rebuilt real-server Chromium native-contract test
  passes without skips, preserving the public edit and technical source.

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
  `scripts/verify-port-tests.mjs` verified all 72 expected cases in
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
  The native manifest verifies 72 cases without skips; the focused rerun also
  covers ordinary run-file uploading, S3 client round trips and deletion
  collection coverage. Separate Engine cases now check fresh producer
  provenance and declared file properties; separate process-kill cases now
  cover an interrupted capture before publication.

- Native activation records and immutable admissions are bound to the
  canonical filesystem root or Mongo backend identity. Copying proof or an
  accepted run to another store cannot authorize execution. Local CLI
  `contracts inspect`, `probe`, `activate` and `deactivate` are wired; no
  trusted distributed fleet census exists yet.
- `TestValidateNativeWorkflowReturnsPublicProjection` and
  `TestPublicViewExposesContractsWithoutTechnicalConfiguration` pass. The
  generated OpenAPI and TypeScript schemas type `/api/validate`'s native
  public graph; `iterion validate --json` exposes the same projection.
- `local_contract_spec` exposes public contract and graph syntax, registered
  deterministic criteria and the native semantic marker directly from the
  shared DSL registry. Copi can request one technical kind by name without
  loading all technical configuration. `TestLocalContractSpecLayersRegistryForAuthoring`
  checks that the default view omits technical workflow fields and that an
  explicit technical lookup works. `local_contract_read` returns the public
  view plus SHA-256 by default and complete source only on request;
  `local_contract_write` validates a staged standalone native `.bot` before
  publishing it under that digest. The focused tests cover creation,
  modification, stale writes, escaping paths, hidden technical source and
  read-only mode. The full `pkg/operatormcp` suite and `go vet` pass. A real
  `iterion mcp` stdio subprocess also passed the public-spec → write → public
  read → explicit technical read → ordinary validate round trip. This
  exercises the MCP authoring seam, not an actual Copi-driven editing session
  or a transactional bundle conversion.
- Legacy validation exposes only a provisional `conversion_draft`, with
  candidate variables and node kinds plus unresolved output, binding, effect
  and file questions. API, CLI and `local_validate` share this shape; it never
  certifies an executable contract. The compatibility and local rollback
  procedure is in `public-contracts-activation.md`; distributed activation
  remains blocked by missing trusted queue/fleet access evidence.
- `VerifiedEffectReplay` passes on filesystem and real Mongo. The verifier
  sees the exact run, invocation, attempt and captured inputs; only bounded
  evidence that the effect did not occur allows replay. Unknown or empty
  evidence leaves the invocation uncertain. A default executor without the
  capability refuses admission before dispatch. The final race JSON manifest
  verifies 72 store and 75 Engine cases without skips.
- `TestNativeProcessKillRecoveryFilesystem` and its Mongo counterpart kill a
  separate test process after the effect-dispatched checkpoint. After an
  explicit supervisor-equivalent status transition, resume refuses to replay
  without an attempt-bound decision and completes exactly once after it. The
  tests do not certify automatic orphan detection by a production supervisor.
- `TestNativeFileCaptureKillRecoveryFilesystem` and its Mongo counterpart kill
  a separate process after the Store reads the first byte of a declared file,
  before its private snapshot is complete. No file reference is published.
  After the explicit orphan-status transition, resume retries the producer
  and publishes only a verified attempt-2 file; both cases pass with race
  detection. The separate local boot-scan test covers native checkpoint
  classification; cloud lease adoption remains an open integration item.
- `TestReconcileOrphans` now covers a native run with `PortExecution` and a
  native run without it. The local Service boot scan marks only the former
  `failed_resumable`. `TestNativeOrphanProcessBecomesResumableAfterKill` holds
  the native run's cross-process lock in a real child, proves the live run is
  left alone, kills the child, then proves the scan marks it resumable. Both
  cases pass with race detection and the full `pkg/runview` suite passes.
  Previously the scan looked only for a legacy `Checkpoint` and classified
  every native crash as terminal `failed`. The cloud runner uses NATS
  lease/redelivery and needs its own native process-kill integration proof.
- `TestQueuedNativeLaunchUsesDurablePortExecution` covers a queued native run
  whose launch message is redelivered after state was persisted. The runner
  now synthesizes a resume from `PortExecution` instead of restarting the
  graph; a first native launch without state remains a launch. The focused
  regression and the full `pkg/runner` suite pass. This guards the status
  switch, not actual delivery by a mixed fleet on the shared consumer.
- `TestInspectSummarizesNativeRunWithoutRawState` verifies human CLI inspection
  names the native semantics, pending nodes, a complete zero-item map, waiting
  products and reported usage without printing technical source identities.
  JSON inspection still carries the complete persisted run for technical
  analysis. The focused CLI checks and `go vet` pass.
- `local_run_get` now projects native invocation counts, ordered map summaries,
  required product publication status and reported usage without technical
  identities or active reservation keys. `total_cost_known` is false when the
  run contains unpriced nodes. A Store-backed MCP test checks the projection;
  this is reported usage, not a proof that every provider has supplied a price.
- A declared paid effect that returns no cost is now counted as unpriced even
  when it reports zero tokens. The focused Engine case passes on filesystem
  and real Mongo with race detection; later finite-cost admissions remain
  blocked by the existing unpriced-usage guard. The runtime manifest includes
  this case on both stores.
- Studio's source picker and edge hint now lift a mapped producer's declared
  scalar output into its effective array type, so a downstream whole-array
  input can select it. The complete `task studio:check` passes 155 files and
  1,369 tests. The real Chromium native-contract scenario passes and checks
  that `render.text` appears as `string[]` for `collect.texts`.
- The Mongo file-recovery fault wrapper now forwards its backend identity.
  Previously its recovery test failed activation before it could inject a
  corrupt or transient file read. Both cases pass with race detection after
  forwarding the identity; this was a test-fixture defect, not a changed
  production file-recovery path.
- The dispatcher's local orphan promotion now uses `PortExecution` as a
  recovery point, matching the Studio service; its native regression and
  full `pkg/dispatcher` suite pass. The runner's workspace-reset audit also
  recognizes native durable state on an undated redelivery, with a focused
  regression. Neither case substitutes for a real cloud process-kill and
  JetStream redelivery test.
- Native selective rewind now uses one run-document CAS to invalidate the
  chosen node and its descendants in both the current graph and captured
  supplier revisions. Filesystem and real Mongo cases cover mapped batches,
  empty collections and a dependency removed from edited source; a real
  Engine resume replays only that suffix, preserving independent work and
  cumulative usage. Refused scopes, uncertain effects and a concurrent resume
  leave the checkpoint intact. The workspace remains in place; explicit
  `--restore-scope none` is required for worktree runs. Automatic native pivot
  selection, workspace restoration and native forks remain outstanding. The
  legacy turn-based fork still refuses native state before mutation.
  The full `pkg/runview` suite and `go vet` pass; the dedicated manifest
  verifies 12 named cases with race detection and real Mongo, without skips.
- A raw production Mongo Store refuses a hand-written distributed activation
  record even if its queue version and `consumer_access_evidence` are filled.
  The isolated Engine fixture injects an explicit verifier; a future-dated
  proof is rejected. The full race-instrumented Engine manifest passes 75
  cases on filesystem and real Mongo without skips. This is a fail-closed
  seam, not a positive deployment census.
- `task test` passed after adding `jq` and `python3` to the disposable devbox
  test container. An earlier pass failed three shell-backed `bots` cases only
  because those commands were missing; their focused rerun passed unchanged.
- A fresh `task test` pass after the native orphan, runner-redelivery,
  dispatcher and CLI-inspection changes completed across the repository.
  `go vet` also passes for the changed runtime, service, runner, dispatcher
  and CLI packages.
- Cloud resume's grant clamp now reads banked cost from the native root's
  durable `PortExecution.Budget` when present, rather than treating the
  absent legacy checkpoint as zero. The full `pkg/server/cloudpublisher`
  suite passes after this change; distributed native activation remains
  blocked by the separate fleet and queue census gap.
- `TestLegacyRuntimeTraceParity` runs a committed ordinary `.bot` workflow
  through the actual pinned-main and current CLIs. The zero- and two-element
  fan-out cases have equal persisted status, budgets, checkpoint location and
  iteration charge, node admissions/completions, mapped outputs and join
  behavior. The race-instrumented manifest verified all three named cases
  without skips. This fixture is a deterministic compatibility sentinel, not
  a substitute for before/after traces of every project workflow.
- Read-only `iterion validate --json` comparisons used the actual pinned-main
  and branch CLIs against all 117 `.bot` sources found in Shorts (27), Town
  (40) and Tabarria (50). Every invocation returned success and `valid: true`;
  each old/current pair had identical normalized diagnostics, with zero
  mismatches. The source set's sorted relative-path/SHA-256 manifest hashes to
  `e18fa9e27e6cbd0293471130c015d697e0e845c6f76f95dcf58dc49b4ff8d2a6`.
  The three version-3 reference files retain their separately frozen hashes.
  This verifies parsing and diagnostics, not execution or bundle resolution.
- A real Chromium e2e case edits a native responsibility, saves the `.bot`,
  and reloads it with graph bindings and technical policy intact. The source
  equivalence check ignores diagnostic spans and JSON formatting while
  keeping absent, null and empty values distinct.
- The complete Playwright Chromium suite passed 33/33 cases on the rebuilt
  branch binary. A preexisting launch-caption assertion was updated to accept
  both the curated fallback and the cached aggregator source; both retain the
  required context and unknown-price behavior.

## Verification rules

The runtime value layer now has 70 mandatory named cases in
`pkg/runtime/ports_cases.json`, all passing with `-race` against the actual
filesystem store and Mongo 8 replica set. Its Mongo artifact client is a real
S3 client connected to a disposable HTTP object fixture. These cases execute
the parser, compiler and Engine, rather than replaying individual executors.
They cover map 0/1/4, reverse completion order, multi-input readiness, crossed
dependencies, connected optional absence, root iteration reservations,
correction before publication and exact integer arithmetic above 2^53.
Recovery covers pause, cancellation winning against finalization/admission,
unchanged successes, selective invalidation after a technical source change,
and manual/idempotent/verified effect decisions tied to an exact attempt.
Six injected commit boundaries run on both stores, including lost acknowledgment
after successful publication. Separate tests also kill a real process after
durable effect dispatch and during private file capture on both stores. The
tests require an explicit orphan-status transition; they do not prove
automatic production orphan detection.

The existing correction, compute, compiler and native-store transition tests
also pass after the shared seam changes. Full `pkg/dsl/expr`, `pkg/dsl/ir` and
`pkg/backend/model` pass with race detection. A complete `task test` pass now
covers the repository's ordinary legacy corpus and unit suites; explicit
before/after legacy executable traces now pass for the committed fan-out
fixture. Project-specific legacy trace comparison and final generated checks
remain open.

The native runtime remains incomplete: production effect verifiers, nested root
admission, verified composition and adapters, distributed access census and
full-source pilots are outstanding. File capture, local activation and product
publication are wired into the native Engine; those passing cases do not authorize a
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

- Distributed activation records now use schema 4, with independent
  `policy_revision` and `proof_revision`. A lease claim does not extend proof
  freshness; renewal compares the enabled policy, observation revision,
  fingerprint and NATS fencing token. Mongo checks the lease deadline again
  using its clock at the write. Disable advances only the policy revision,
  preserving the latest observations, and cannot conflict with renewal alone.
  Old/future activation schemas remain refused. The run admission's
  `activation_revision` denotes the operator policy revision. All holders of
  Store write credentials are inside the proof-integrity trust boundary.
- Capability heartbeats use `ports-v1.census.<principal>.<instance>` in the
  existing rollout KV bucket, without changing its TTL or monotonic epoch
  keys. Freshness uses the broker timestamp (60 seconds); the opt-in publisher
  ticks every 20 seconds and deletes its own revision at shutdown. The
  authority's subject-and-sequence-bounded cleanup removes entries and
  tombstones older than 24 hours while preserving concurrent publications.
  The retention cutoff is advanced in tests; no 24-hour soak is claimed.
- The distributed-primitives manifest passes 26 cases with race detection,
  real Mongo and NATS 2.14, including an actual pinned-main `EnsureSchema`
  caller. `go vet` passes for activation, Store and NATS packages. These are
  storage and census primitives: the production authority observer, effective
  ACL/Kubernetes reconciliation, refresher lifecycle, operator handlers and
  positive distributed activation are still outstanding. Raw Mongo continues
  refusing a hand-written activation record.
- The complete Studio check after the connection-picker change passes 156
  test files and 1,375 tests, in addition to its rebuilt Chromium round trip.
- The privileged source parser now invokes the same binary through a private
  pipe protocol, without inherited environment, `.env` loading or telemetry.
  Upstream parse errors are redacted; parent-owned private scratch is removed
  even after killing a parser that has written source bytes. Includes are
  restricted to whole-line `include "relative.conf"` statements naming files
  in the supplied bundle. Other spellings, even ambiguous occurrences of the
  word in values/comments, are refused by this initial source profile. Bounds:
  32 files, 512 KiB of source, 256 expanded include visits and a five-second
  helper deadline. External environment variables and include cycles are
  refused; local variables and included permission defaults are retained.
- The pinned image digest used by cloud compose and now NATS CI actually
  reports **2.14.5**. The first exact-version integration check caught a 2.14.0
  parser mismatch even though that fixture's digest agreed. The upstream
  `nats-server/v2/conf` dependency is therefore pinned to 2.14.5, matching the
  broker. It adds five vendored files and also requires `nkeys` 0.4.15 → 0.4.16
  and `compress` 1.19.1 → 1.19.2; existing module checksums verify. This is not
  a claim that arbitrary NATS patch versions are interchangeable.
- All 18 required parser cases pass with race detection, including the real
  production helper's `.env` bypass and an actual 2.14.5 broker's loaded
  `config_digest`, through HTTP and authenticated system VARZ both before and
  after reload. The static production build, queue/activation race suites and
  relevant `go vet` checks pass. The existing NATS CI job now builds the old
  and current fixture executables and runs these tests. Protected-API exclusion
  and Kubernetes/credential-custody reconciliation remain pending; parsing a
  matching configuration does not establish that authority.
- Static subject-permission analysis now checks the complete intersection of
  protected patterns and allowed subjects minus the union of denies. A finite
  alphabet partition covers unnamed literals; `>` consumes one or more tokens.
  Unsupported patterns, queue-qualified rules and excessive analysis complexity
  refuse a conclusion. The bounded static-profile loader now refuses dynamic
  reply permissions and other unsupported auth paths before using this evaluator.
- Actual NATS 2.14.5 publish/subscription tests match the evaluator for seven
  permission arrangements, including inherited default denial, an explicit
  empty permissions block overriding those defaults, and an empty configured
  allow list permitting access. API, ACK and KV subjects plus a generated
  wildcard witness are exercised. This is permission-language evidence, not
  the complete protected-API census or an exclusion proof for the deployment.
  The parser/permission cases remain in the required manifest and the package
  passes `go vet`.
- The helper's output limiter uses a private buffer rather than embedding
  `bytes.Buffer`: an inherited `ReadFrom` method would let `io.Copy` bypass
  the capped `Write` path. A pipe-copy regression passes with race detection,
  bringing the earlier combined parser/permission manifest to 40 cases.
- The supported NATS 2.14.5 static-profile loader now projects effective
  publish/subscribe allow and deny rules for named password and public-nkey
  users. It applies account defaults only when a user omits permissions; an
  explicit empty block overrides them, matching broker delivery. It accepts
  top-level `VAR_` local variables resolved by the upstream parser, confines
  operational and authorization keys to a closed schema, and refuses dynamic
  responses, imports/exports, queue-qualified rules and unreviewed access
  paths. The returned profile excludes passwords. A real pinned broker agrees
  for inherited defaults and a signed nkey, denies anonymous access, and
  reports the same parsed digest through HTTP and authenticated system VARZ
  before and after reload. The race-instrumented manifest now verifies all
  **91** named parser/profile/permission cases without skips; `go vet` and a
  static production build pass. This is still an isolated source projection:
  live JetStream/KV topology reconciliation, credential-holder inventory,
  Kubernetes reconciliation and production verifier remain outstanding.
- Source bytes and all upstream parsed keys/values are checked for UTF-8 before
  JSON serialization. The pinned broker accepts an escaped non-UTF-8 subject
  byte and distinguishes it from U+FFFD in effective permissions, while JSON
  would otherwise replace the byte and collapse allow/deny rules. The helper
  now refuses this lossy configuration; a real-broker differential test and
  an invalid-source test cover both sides of that boundary.
- A queue-account ACL classifier now reports concrete witnesses for Core run
  and DLQ subjects, native cancel/heartbeat/steer subjects, reply inboxes,
  JetStream requests and their request/reply injection directions, both ACK
  prefixes and both KV buckets. It includes the
  KV backing streams in the known API forms and separately flags unreviewed
  `$JS.API.>` variants; domain and cross-account import/export routes are
  excluded by the supported profile. A pinned 2.14.5 broker confirms that
  an exact `MSG.NEXT` grant fetches from the shared durable, an exact stream
  purge grant clears the run stream, and a Core subscription receives its
  run payload. The distinct system account is reported as privileged authority
  exposure rather than being treated as a harmless other account. The full
  race-instrumented NATS manifest has 93 cases without skips. The classifier
  still needs authoritative live topology, credential
  custody and workload reconciliation before its result can admit a root.
- The same classifier treats system-account access to `$SYS.>` requests or
  events and `_INBOX.>` replies in either direction as privileged. The pinned
  broker confirms that a user allowed to read system requests and publish
  inbox replies can forge a monitoring response; a VARZ request alone does
  not authenticate its responder.
- A read-only system-account observer now binds one named broker's VARZ
  identity, pinned version and loaded digest to the parsed source, then reads
  one complete authenticated CONNZ snapshot within explicit size bounds. A
  pinned broker confirms both system and worker identities appear and a wrong
  digest refuses observation. An equal-total connection-churn regression
  shows why partial offset pages are refused. This detects current contradictions; it cannot
  establish that every broker or disconnected credential holder was inventoried.
- A bounded system-account `PING.IDZ` comparison now checks every promptly
  responding broker against the declared ID/name set and refuses missing,
  duplicate, foreign or malformed replies. A pinned 2.14.5 two-broker cluster
  confirms that both peers respond and an omitted peer is rejected. Silence or
  a network partition can still
  hide a broker, so this remains corroboration of the operator's exhaustive
  inventory assertion, not an independent completeness proof.
- A read-only authority observation now combines the declared `PING.IDZ`
  broker set with concurrent VARZ/CONNZ reads for every broker and reconciles
  loaded configuration digests and connected principals against the static
  inventory. A pinned 2.14.5 broker and the production parser exercise this
  combined path, including a stale-digest refusal; the NATS CI job requires
  that case without skips. A dedicated, named system-account dialer avoids the
  ordinary queue connector's JetStream schema initialization and redacts
  credential-bearing URL failures.
  The combined observation is still not a distributed activation proof.
- Explicit `contracts.distributed` configuration loads through the existing
  YAML/environment precedence and validates a server-only authenticated
  system URL, operator Secret reference and bounded namespace scope. The Helm
  chart defaults to no authority wiring; an opt-in trusted release receives
  the system URL through a named Secret key while a distinct worker release
  has zero server replicas, no server HPA and no system URL. Each release can
  override the shared non-secret NATS URL with its own Secret-backed account
  URL. Authority templates reject sandbox RBAC, credential-bearing shared
  NATS URLs and reserved authority keys in `config.extraEnv`. The chart profile
  script passes locally and is wired into the existing Helm CI job. Runtime
  authority loading and Kubernetes verification are still outstanding.
- A bounded server-side `kubectl get secret` reader now retrieves only the
  named authority Secret, checks Kubernetes object identity and resource
  version, and keeps the JSON material out of pointer/value formatting or
  marshaled source metadata. The four-case required manifest exercises the
  actual bounded subprocess pipe and cancellation with an inherited stdout
  descriptor using a disposable kubectl shim, and
  is wired into the existing Go CI job. This is source acquisition only:
  permitted-writer and custody verification remain outstanding.
- Version-1 operator records now require bounded concrete NATS source bundles,
  broker pod identities, namespace scope, every declared credential holder and
  issuer, and named permitted writers. The strict parser rejects unknown or
  duplicate JSON keys, ambiguous custody references and mutable image tags;
  sensitive NATS configuration is omitted from record formatting and JSON.
  The parser requires canonical schema field names, rejecting Unicode and
  ASCII aliases while preserving case-distinct NATS source filenames. Static
  analysis parses every declared broker source with the
  pinned profile, reconciles all configured NATS principals to custody entries,
  classifies protected ACL exposure and refuses inconsistent broker ACLs or
  system credentials assigned outside the authority role. Equivalent ACL rule
  order is normalized across brokers. Twelve required
  reader/schema/static cases pass with race detection. These fields are
  operator assertions only: Kubernetes RBAC, live broker digests, workload
  coverage and credential custody have not yet been reconciled.
- A bounded Kubernetes workload reader now lists Pods, Deployments,
  ReplicaSets, StatefulSets, DaemonSets, Jobs, CronJobs and
  ReplicationControllers in each declared namespace. It retains private pod
  templates and running-pod status for later launch and credential-reference
  checks, including scaled-to-zero templates; unsupported objects, partial
  lists and oversized subprocess output refuse the read. The 15-case authority
  manifest passes with race detection. This inventory does not establish that
  custom controllers, cross-namespace RBAC or external credential holders are
  absent; no distributed activation proof is produced.
- UID-bound owner references now connect Pods through ReplicaSets/Jobs to
  their declared controllers. A workload reconciliation step checks explicit
  NATS SecretKeyRef variables against each holder's namespace, immutable image,
  container and ServiceAccount. It refuses old scaled-to-zero controllers,
  changed descendant images, unknown holders, inline URL values and known
  credential Secrets mounted or imported through unreviewed paths. The
  17-case authority manifest passes with race detection. Secret contents,
  ConfigMap/envFrom supply, effective RBAC and external holders are still
  outside this partial reconciliation; it cannot authorize activation.
- The shared bounded kubectl transport now also reads one exact NATS Secret
  key with UID/resourceVersion identity. URL userinfo is validated without
  exposing the password through formatting, JSON or errors. Nineteen required
  authority cases pass with race detection. A named, well-formed Secret key
  still does not prove that its principal matches the static NATS configuration
  or that no other holder has copied the credential.
- Credential-source reconciliation now compares every Kubernetes holder's
  named Secret key with its declared NATS principal and records the exact
  Secret UID/resourceVersion. It refuses a user mismatch or two revisions of
  one Secret observed in a probe, while reporting external CLI/automation
  holders as unresolved. The single-URL profile rejects comma-separated NATS
  server lists in both Secret values and configured system URLs, because the
  NATS client would otherwise give later servers different authentication.
  Twenty-two authority cases pass with race detection. Broker acceptance,
  exclusive custody and cross-namespace RBAC remain unverified.
- A bounded read-only RBAC source now lists namespaced Roles/RoleBindings and
  cluster-wide ClusterRoles/ClusterRoleBindings, preserving policy rules,
  subjects, referenced roles, aggregation flags and resource versions. It
  rejects missing lists, continuation pages, foreign objects and unsupported
  role references. Twenty-four authority cases pass with race detection. The
  source is not yet an effective permission analysis, nor evidence that RBAC
  is the cluster's only authorizer.
- A conservative RBAC boundary analyzer now resolves referenced roles and
  worker ServiceAccount/group grants across trusted RoleBindings and all
  ClusterRoleBindings. It refuses trusted-namespace Secret/pod/RBAC access,
  pod-producing controllers, token/impersonation and similar privilege paths,
  plus unapproved direct writers of the authority Secret. Aggregated or
  missing referenced roles fail closed. Pod subresources, controller scale
  subresources and anonymous Secret writers are also denied. Twenty-seven
  authority cases pass with race detection. This is RBAC corroboration only: admission controllers,
  non-RBAC authorizers, indirect writer paths and live worker-identity denials
  remain unverified.
- The broker Pod launch checker requires a running, ready Pod with the declared
  immutable image and an exact `/nats-server -c <config>` invocation; it rejects
  command-line authorization overrides and stale runtime images. A separate
  reconciliation matches every declared broker to one per-server system-account
  observation, including version, name, configuration digest and currently
  connected principal identities. A bounded Secret reader now compares every
  declared NATS source file with the Secret keys selected by a read-only Pod
  volume declaration, including nested includes, and records both Pod and Secret revisions.
  Thirty-six authority cases pass with race detection. These checks do not
  discover omitted brokers or disconnected credential holders, establish
  observation freshness, actual kubelet-mounted bytes or a complete Kubernetes
  authorization boundary.
- After the authority config changes, a complete `task test` run passed.
  An earlier run concurrent with the automatic reviewer exceeded the frozen
  Town zero-item latency ceiling; the isolated Town case and all nine pilot
  scenarios passed afterward, as did the complete uncontended run. Pilot
  thresholds and implementation were not changed to resolve that variance.
- A complete `task test` pass initially found two fixture/CI omissions:
  `pkg/runview` was missing from the Mongo job, and the raw-distributed-proof
  test called a Mongo factory without checking the optional fixture gate.
  The factory now skips only in ordinary runs without a configured URI and
  fails in required mode. The Mongo job covers runview and explicitly selects
  the raw-proof and native process-kill tests. Eight targeted real-Mongo/CI
  cases pass with race detection, and the subsequent complete `task test`
  pass succeeds. These fixes change verification coverage, not admission rules.

### Admission and compatible-build recovery

New launches continue to require the exact probed binary capability, including
its build fingerprint and activation format. An admitted run now also carries
an immutable resume-compatibility digest over its native semantics, persisted
format, namespace and explicit interpreter compatibility version. Recovery
checks that digest and the store/creation identity without consulting an
enabled or fresh launch proof. New admissions carry version 1 and require this
digest. Admissions written before this field existed have absent version and
digest; they remain readable and resumable while the original native format
and declared recovery compatibility version 1 are supported. New creations
cannot use the old shape. Unknown admission versions and malformed mixed
shapes are refused.

`TestNativeAdmissionSurvivesCompatibleBuildUpgrade` builds two real fixture
executables with distinct embedded version and commit values. The first
admits a native run; the second cannot start a new root under that build's
proof, disables launches, then accepts the persisted run for recovery. The
test proves a build-fingerprint change with otherwise identical runtime code;
it does not certify arbitrary code upgrades. Incompatible interpreter changes
must advance `ResumeCompatibilityVersion`, and the resumed execution's
persisted identity is checked separately. Filesystem and real-Mongo Store
tests also refuse mutation of the persisted recovery digest. Distributed
rollback and live redelivery remain open acceptance items.

The first revision of this change made `resume_digest` mandatory for every
decoded record. Automatic review caught that it would hide existing native
runs from filesystem lists and make both filesystem and Mongo loads fail.
The versioned read path and raw old-shape filesystem/Mongo regression cases
fix that compatibility break. The race-instrumented distributed-primitives
manifest verifies all 26 named cases executed without skips, including the
two-build and both old-record cases; targeted native Engine rollback passes on
filesystem and Mongo. The full repository `task test` passed before this
review correction with the real Mongo replica set and historical binaries.
A complete rerun after correction also passes with the historical binaries;
its Mongo-specific paths are covered by the separate real-Mongo race run.
The full native storage manifest now verifies 72 named cases without skips
against filesystem, real Mongo and the historical executable fixture.
`go vet` passes for activation, Store, Mongo and runtime packages.
