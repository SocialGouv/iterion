# impl-provenance — delivery evidence

Worktree `run-provenance`, branch `leap/run-provenance`. No commit or push.
No paid live model call. Mutations use owned scratch at
`/tmp/impl-provenance.kE0hqM`; the working files retain the corrected code.

The contract, formats, legacy ordering and API are documented in
[run provenance](../agents/engine/run-provenance.md). Historical nested
sub-bots stay compact with links to their own history, preventing a later
child resume from changing an older parent graph.

## Implementation sites

- Captured compiler inputs: [pkg/runview/compile.go:510](../../pkg/runview/compile.go#L510)
- Frozen queue AST: [pkg/runview/workflow_source.go:116](../../pkg/runview/workflow_source.go#L116)
- FS immutable snapshots: [pkg/store/attempt.go:40](../../pkg/store/attempt.go#L40)
- Mongo/blob implementation: [pkg/store/mongo/attempt.go:16](../../pkg/store/mongo/attempt.go#L16)
- Attempt opening: [pkg/runtime/engine_events.go:44](../../pkg/runtime/engine_events.go#L44)
- Authoritative projection / legacy fallback: [pkg/runview/workflow_export.go:164](../../pkg/runview/workflow_export.go#L164)
- Cloud bot-source resolution: [pkg/server/bot_resolver.go:22](../../pkg/server/bot_resolver.go#L22)
- HTTP selector and typed errors: [pkg/server/runs_detail.go:200](../../pkg/server/runs_detail.go#L200)
- Attempt boundaries in Studio: [studio/src/lib/runAttempt.ts:7](../../studio/src/lib/runAttempt.ts#L7)
- Selector / historical child links: [studio/src/components/Runs/RunView.tsx:647](../../studio/src/components/Runs/RunView.tsx#L647)
- Attempt metrics: [studio/src/hooks/useRunMetrics.ts:171](../../studio/src/hooks/useRunMetrics.ts#L171)
- Cumulative budget meter: [studio/src/components/Runs/leftPanel/overview/BudgetSection.tsx:39](../../studio/src/components/Runs/leftPanel/overview/BudgetSection.tsx#L39)
- Live and durable child attachment: [pkg/runtime/special_node.go:264](../../pkg/runtime/special_node.go#L264)

## Mutation witnesses

Each mutation is run independently and produces an assertion failure, not a
compiler or configuration error. Restore the statement after reproducing it.
Comments beside the tests explain the invariant.

| Witness | Production mutation | Observed |
|---|---|---|
| [TestAttemptProvenanceSurvivesResumeAndSourceDeletion:33](../../pkg/runview/workflow_provenance_test.go#L33) | [pkg/runtime/engine_events.go:42](../../pkg/runtime/engine_events.go#L42): `typ == store.EventRunStarted &#124;&#124; typ == store.EventRunResumed` → `typ == store.EventRunStarted` | RED |
| [TestMissingAttemptSnapshotDoesNotFallBack:201](../../pkg/runview/workflow_provenance_test.go#L201) | [pkg/runview/workflow_export.go:169](../../pkg/runview/workflow_export.go#L169): `if len(r.Attempts) == 0 {` → `if true {` | RED |
| [TestLegacyRecordedSourceOutranksDisk:254](../../pkg/runview/workflow_provenance_test.go#L254) | [pkg/runview/workflow_export.go:233](../../pkg/runview/workflow_export.go#L233): `recorded := recordedSource(r)` → `var recorded *workflowfile.Snapshot` | RED |
| [TestAttemptCapturesUnitAndBundleResources:141](../../pkg/runview/workflow_provenance_test.go#L141) | [pkg/runview/workflow_source.go:42](../../pkg/runview/workflow_source.go#L42): `s.Files[rel] = body` → `_ = rel; _ = body` | RED |
| [TestWorkflowHashCacheSeparatesSourceRevisions:176](../../pkg/runview/workflow_provenance_test.go#L176) | [pkg/runview/workflow_export.go:316](../../pkg/runview/workflow_export.go#L316): `if cached := cache.get(key); cached != nil {` → `if cached := cache.get(key); false && cached != nil {` | RED |
| [TestRunWorkflowStoredLegacyAndImmutableAttempt:22](../../pkg/server/run_workflow_provenance_test.go#L22) | [pkg/server/bot_resolver.go:22](../../pkg/server/bot_resolver.go#L22): `func (s *Server) legacyRunWorkflowSource(ctx context.Context, r *store.Run) (*workflowfile.Snapshot, error) {` → `func (s *Server) legacyRunWorkflowSource(ctx context.Context, r *store.Run) (*workflowfile.Snapshot, error) { return nil, &runview.WorkflowUnavailable{Code:"bot_source_missing",Source:r.BotID};` | RED |
| [TestAttemptQueueUsesSameRevision:18](../../pkg/server/cloudpublisher/provenance_test.go#L18) | [pkg/server/cloudpublisher/publisher.go:4702](../../pkg/server/cloudpublisher/publisher.go#L4702): `if wf != nil && wf.SourceSnapshot != nil {` → `if false && wf != nil && wf.SourceSnapshot != nil {` | RED |
| [TestAttemptProjectionDoesNotRebindHostMCP:228](../../pkg/runview/workflow_provenance_test.go#L228) | [pkg/runview/workflow_source.go:106](../../pkg/runview/workflow_source.go#L106): `true, b, false)` → `true, b, true)` | RED |
| [TestResume_BudgetlessWorkflowClearsPriorCaps:91](../../pkg/runtime/budget_ask_persist_test.go#L91) | [pkg/runtime/engine_run.go:640](../../pkg/runtime/engine_run.go#L640): `b := e.effectiveBudgetSnapshot() 	if err := e.store.SetRunBudgetSnapshot` → `b := e.effectiveBudgetSnapshot()  if b == nil { return } 	if err := e.store.SetRunBudgetSnapshot` | RED |
| [TestLoadWorkflowPreservesAttemptSource:14](../../pkg/runner/attempt_test.go#L14) | [pkg/runner/loop.go:3106](../../pkg/runner/loop.go#L3106): `runview.SourceSnapshotFromTransport(raw)` → `runview.SourceSnapshotFromTransport([]byte(`{}`))` | RED |
| [TestAttemptWithoutCompilerCaptureRefusesMutableDisk:272](../../pkg/runview/workflow_provenance_test.go#L272) | [pkg/runview/workflow_export.go:200](../../pkg/runview/workflow_export.go#L200): `if captured.Source == nil { 		return nil, unavailable("attempt_snapshot_missing", a.ID)` → `if captured.Source == nil { 		return buildLegacyWireWorkflow(ctx,r,resolve,cache)` | RED |
| [TestAttemptBundleExportIdentitySurvivesMaterialization:308](../../pkg/runview/workflow_provenance_test.go#L308) | [pkg/runview/compile.go:76](../../pkg/runview/compile.go#L76): `root := u.Root 	if b != nil {` → `root := u.Root 	if false && b != nil {` | RED |
| [TestAttemptSnapshotsFilesystem:9](../../pkg/store/attempt_conformance_test.go#L9) | [pkg/store/attempt.go:48](../../pkg/store/attempt.go#L48): `body, err := json.Marshal(a)` → `body, err := json.Marshal(a); if len(body)>1<<20 { body=[]byte(`{}`) }` | RED |
| [TestAttemptKeepsConsumedSubbotAttachment:341](../../pkg/runview/workflow_provenance_test.go#L341) | [pkg/runview/subbot.go:428](../../pkg/runview/subbot.go#L428): `Type: store.EventSubbotAttached` → `Type: store.EventNodeFinished` | RED |
| [TestAttemptAttachmentUsesEngineObserver:386](../../pkg/runview/workflow_provenance_test.go#L386) | [pkg/runview/subbot.go:425](../../pkg/runview/subbot.go#L425): `if req.OnChildAttached != nil {` → `if false && req.OnChildAttached != nil {` | RED |
| [isolates metrics:28](../../studio/src/lib/runAttempt.test.ts#L28) | [studio/src/lib/runAttempt.ts:21](../../studio/src/lib/runAttempt.ts#L21): `e.seq >= startSeq` → `true` | RED |
| [switches provenance and caps:44](../../studio/src/lib/runAttempt.test.ts#L44) | [studio/src/store/run/reducer.ts:348](../../studio/src/store/run/reducer.ts#L348): `budget: attempt.budget` → `budget: snapshot.run.budget` | RED |
| [keeps an observed active node:56](../../studio/src/lib/runAttempt.test.ts#L56) | [studio/src/lib/runAttempt.ts:52](../../studio/src/lib/runAttempt.ts#L52): `return { ...wf, nodes };` → `return wf;` | RED |
| [retains historical metrics:72](../../studio/src/lib/runAttempt.test.ts#L72) | [studio/src/store/run/reducer.ts:857](../../studio/src/store/run/reducer.ts#L857): `state.events.concat(appended)` → `state.events.concat(appended).slice(-5000)` | RED |
| [clears scrub:35](../../studio/src/lib/runAttemptView.test.tsx#L35) | [studio/src/components/Runs/runView/useSelectionState.ts:117](../../studio/src/components/Runs/runView/useSelectionState.ts#L117): `[runId, attemptKey]` → `[runId]` | RED |
| [authored labels:44](../../studio/src/lib/runAttemptView.test.tsx#L44) | [studio/src/lib/runChat/useNodeLabel.ts:20](../../studio/src/lib/runChat/useNodeLabel.ts#L20): `queryKey: ["run-workflow", runId, key]` → `queryKey: ["run-workflow", runId]` | RED |
| [stream has no active_ms:56](../../studio/src/lib/runAttemptView.test.tsx#L56) | [studio/src/hooks/useRunMetrics.ts:202](../../studio/src/hooks/useRunMetrics.ts#L202): `if (running && totalActive > 0)` → `if (running)` | RED |
| [reattached child:92](../../studio/src/lib/runAttempt.test.ts#L92) | [studio/src/lib/runAttempt.ts:61](../../studio/src/lib/runAttempt.ts#L61): `scope.isCurrent && reattached.includes(child.id)` → `false && scope.isCurrent && reattached.includes(child.id)` | RED |
| [sub-millisecond:99](../../studio/src/lib/runAttempt.test.ts#L99) | [studio/src/lib/runAttempt.ts:73](../../studio/src/lib/runAttempt.ts#L73): `return fraction(left) - fraction(right);` → `return 0;` | RED |
| [cumulative duration:67](../../studio/src/lib/runAttemptView.test.tsx#L67) | [studio/src/components/Runs/leftPanel/overview/BudgetSection.tsx:40](../../studio/src/components/Runs/leftPanel/overview/BudgetSection.tsx#L40): `Math.max(cp?.elapsedMs ?? 0, run.active_duration_ms ?? 0, (events[0]?.active_ms ?? 0) + metrics.durationMs)` → `metrics.durationMs` | RED |
| [retains consumed child:107](../../studio/src/lib/runAttempt.test.ts#L107) | [studio/src/lib/runAttempt.ts:60](../../studio/src/lib/runAttempt.ts#L60): `attached.has(child.id) &#124;&#124;` → `false &#124;&#124;` | RED |
| [historical child graphs compact:80](../../studio/src/lib/runAttemptView.test.tsx#L80) | [studio/src/hooks/useInlineSubbotData.ts:223](../../studio/src/hooks/useInlineSubbotData.ts#L223): `historical ? [] : rootChildrenByNode` → `rootChildrenByNode` | RED |

The shared FS/Mongo source-size and budget assertions live in
`pkg/store/storetest/attempts.go`. Raw overlays, Vite transforms and proof logs
are in `/tmp/impl-provenance.kE0hqM/mutations/`; their builders are
`mutations.py` and `ts-mutations.py` in that scratch root. Initial compile-only
or config-only failures are corrected and rerun; the table counts only real
assertion failures.

## Validation

- Exact `go build -mod=vendor ./...` is blocked by the **existing duplicate map
  key** `ITERION_SCHEDULE_PATH_MAX_BYTES` at
  `pkg/platformcfg/platformcfg.go:212,215`. That file is unchanged. The temporary
  overlay removes only that duplicate for useful validation of this change.
- All-package build with this overlay and complete `pkg/store` suite: **PASS**
  (`/tmp/impl-provenance.kE0hqM/build-generated-final.log`).
- Complete `pkg/dsl/ir`, `pkg/dsl/workflowfile`, `pkg/store/mongo` and
  `pkg/server/cloudpublisher` suites pass with the overlay. Connected Mongo
  conformance is skipped without `ITERION_TEST_MONGO_URI`; FS runs the same
  attempt snapshot contract, including a resource larger than 1 MiB.
- Targeted `pkg/runview`, `pkg/runtime`, `pkg/runner`, `pkg/server` suites pass:
  attempts, compiler/hash, queue hydration, reattachment, effective budgets
  and OpenAPI. Complete suites are attempted, but the sandbox forbids sockets
  required by existing `httptest` tests. Runner git fixtures also encounter
  the host signing configuration. These complete suites are not claimed green.
- Studio: **79 targeted tests pass**, including 15 attempt witnesses,
  human-form consumers, subbot hooks and source/accessibility discipline.
  The full suite before the last two child witnesses reports **3,627 passes,
  11 failures**. All 11 reproduce with the changed modules restored to `HEAD`
  through a temporary Vite transform: splitter sizing, dropzone attributes and
  an IssueCard banner. Baseline sources remain unchanged.
- TypeScript compilation is blocked by existing Lucide `IconComponent` type
  errors. The new attempt code has no reported type error after registering
  the `subbot_attached` wire variant. ESLint on changed TS files: **0 errors**;
  warnings remain for existing ref/non-null assertion patterns.
- `task check` is attempted. The regenerated package map passes; OpenAPI
  generation then stops on the same duplicate Go key. Maps, OpenAPI and TS
  schema are regenerated separately using the overlay.
- Final formatting: **gofmt clean**, **`git diff --check` clean**. Generated
  map freshness and its negative witness pass.

Representative reproduction (the exact build above has no overlay):

```sh
GOCACHE=/tmp/impl-provenance.kE0hqM/go-cache devbox run -- go test \
  -mod=vendor -overlay=/tmp/impl-provenance.kE0hqM/overlay.json \
  ./pkg/runview ./pkg/runtime ./pkg/runner ./pkg/server \
  -run 'Test(Attempt|LegacyRecorded|MissingAttempt|WorkflowHashCache|RunWorkflowStored|LoadWorkflow|Resume_.*Budget|RunBudget|Compile|ComputeWorkflowHash|ReattachSubbot|OpenAPI)' -count=1
```

## Review

Five local rounds, opening estimate 8, ceiling 50 (31 files at round 1), two
reviewers. Fixed findings include queue/snapshot revision mismatch, ambient
MCP rebinding, raw-IR disk reread, bundle-export hash instability, event
truncation, selection/label/history leaks, duration clocks, cumulative budget,
consumed child associations and live observation. Independent final witnesses
confirm one durable attachment and one live delivery with branch attribution.
No unresolved high/critical finding is reported in the reviewed scope.
No forge gate or cross-family upstream plan review is claimed.

Journal: `/tmp/impl-provenance.kE0hqM/review.jsonl`.
