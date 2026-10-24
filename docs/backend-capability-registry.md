# Backend capability registry — seed and review contract

`pkg/backend/backendcaps` is the declarative leaf shared by compiler and runtime.
The seed observes commit `9e5a31e2de75da413077bf5146432f6a470d77b3`. It changes
no permission, routing, tool, session or maturity verdict. Custom registries
remain open: unknown names acquire neither capabilities nor compiler refusals.
Lookups are exact; existing callers retain their normalization policy.

## Shape and scope

[registry.json](../pkg/backend/backendcaps/registry.json) has one row for each
of the seven backends: `claude_code`, `claw`, `codex`, `pi`, `grok`, `kimi`,
`opencode`. Each cell has a stable name, axis, verdict, evidence (repository
file:line at the seed commit, or test name), and optional scope detail.
The inventory records three different observations:

- **168 implementation cells** record existing wiring, using `shipped`,
  `partial`, `typed-refused`, `inert`, `unwired (gap)`, `unwired (no image)`
  and explicit `unknown`. A partial capability still needs runtime checks.
- **63 published proof cells** preserve the nine columns of
  [backends.md](backends.md#per-backend-capability-matrix) verbatim, including
  compound verdicts such as `proven (events) · unknown (cost)`. A code path
  existing does not upgrade a published proof cell.
- [live_seed.json](../pkg/backend/backendcaps/live_seed.json) preserves all
  fields of the **57** target rows from
  [the live ledger](../e2e/testdata/live/ledger.json): **55 `never`, 2 `pass`**
  (`test:live:bot:rgaa-audit`, `test:live:claude-bg`). There is no backend
  column in that ledger. A target pass is never assigned to every backend.

Rows also carry the established detection/preference ranks, system-prompt
composition and native exclusion roster. Every returned slice/map is a copy.
The package imports only the standard library and embeds its declarations;
there is no repository-file read at runtime.

`session-id-resume` means consumption of `Task.SessionID`, independently of
`session-persist`: claw offers persist but does not consume that ID;
opencode consumes the ID but cannot pack its shared SQLite session store.
Similarly, `mcp-forwarding` means delivery of `Task.MCPServers`, not all MCP
support: claw resolves it in-process, and pi needs its RPC transport to serve
forwarded servers. `tool-list` arrival is neither name resolution nor proof
of an empty effective tool surface. The Claude roster stays at its existing
14 entries, including legacy `Task`; this does not resolve #1651.

The axes reference the still-open work in
[epic #1404](https://github.com/SocialGouv/iterion/issues/1404): permission,
resume, fork, session, MCP, subagents, workspace commands, allowed tools,
bundle skills, image/presence, proxy/SSRF, metering, interaction and provider
routing. **None of its 19 sub-issues is implemented or closed by this seed.**
The claw ↔ claude_code doctrine is already settled; extending it to the five
CLI-agent backends remains the epic's decision.

## Enumeration inventory

The requested 14 core sites are all derived. Locations below name the
**pre-migration HEAD lines**, so they remain reproducible with `git show
9e5a31e2de75:<path>` rather than drifting with this refactor. Function and
variable names locate the current consumers.

| # | Site at seed HEAD | Derived fact |
|---|---|---|
| 1 | `pkg/dsl/ir/validate_fallbacks.go:52` — `gateEnforcingModes` | Permission ask/deny membership |
| 2 | `pkg/dsl/ir/validate_fallbacks.go:67` — `reasoningEffortBackends` | Effort dial presence; sorted diagnostic output remains sorted |
| 3 | `pkg/dsl/ir/validate_fallbacks.go:163` — `externalHookGateBackends` | External gate hook requirement |
| 4 | `pkg/dsl/ir/validate_command.go:16` — `commandIgnoringBackends` | Inert per-node binary override |
| 5 | `pkg/dsl/ir/validate_provider.go:57` — `hintIgnoringBackends` | Inert provider chain |
| 6 | `pkg/dsl/ir/validate_async_backend.go:15` — `validateAsyncBackends` | Explicit async refusal; unknown routes still defer |
| 7 | `pkg/dsl/ir/validate_async_backend.go:62` — `validateSyncInteractionBackends` | Inert sync interaction warning |
| 8 | `pkg/backend/toolcatalog/toolcatalog.go:50` — `ConstrainsTools` | Registry name resolution |
| 9 | `pkg/backend/toolcatalog/declared.go:56` — `ReceivesToolList` | Tool-list delivery |
| 10 | `pkg/backend/delegate/claude_code.go:78` — `claudeNativeTools` | Exact legacy 14-name roster, in legacy order |
| 11 | `pkg/backend/detect/detect.go:87` — `DefaultPreferenceOrder` | `claude_code,claw`, unchanged |
| 12 | `pkg/backend/detect/detect.go:124` — `detectBackends` | Five probes in legacy order; kimi/grok remain unprobed |
| 13 | `pkg/backend/model/executor_retry.go:520` — `sessionResumeBackends` | Session-ID consumption |
| 14 | `pkg/backend/model/executor_persist.go:46` — `SessionResumeCapability` | Portable persistent-session support |

The search found additional mechanically derivable sites; 14 is not a claim
that the repository contains only 14 enumerations:

| Site at seed HEAD | Disposition |
|---|---|
| `pkg/backend/model/executor_build_task.go:1561` — `mcpForwardingBackends` | Derived Task.MCPServers delivery |
| `pkg/backend/automemory/automemory.go:57` — `supportedBackends` | Both support and authored-prompt requirement derived |
| `pkg/llmroute/policy.go:83` — `Harnesses` | Derived names, preserving presentation order; both error-message lists also derive from this slice |
| `pkg/backend/delegate/delegate.go:242` — `SystemPromptModeForBackend` | Derived composition metadata; unknown remains standalone |
| `pkg/backend/model/executor_retry.go:495` — `providerFallbackEligible` | Derived positive provider-chain predicate, matching the compiler's negative predicate |
| `delegate.Backend*`, `detect.Backend*`, `llmroute.Harness*`, `toolcatalog.ClawBackend` | Compatibility aliases to the leaf's constants |

These other name switches/tables remain deliberately owned by their existing
seams; the reason is a different computation, not an unrecorded verdict:

| Site | Why it remains / follow-up |
|---|---|
| `pkg/backend/delegate/registry.go:36` — `DefaultRegistry` | Constructor wiring requires concrete backend types. The conformance witness asserts every declared delegate has an adapter; claw is injected by the model executor, not this constructor. A future factory seam can own construction. |
| `pkg/backend/detect/detect.go` — probe adapter map | Credential-specific functions remain adapters. The registry decides membership/order; missing adapter wiring fails loudly. Adding kimi/grok probes is a new implementation, outside this seed. |
| `pkg/backend/ambient/ambient.go:187` — `backendVerdicts` | Per-policy verdicts and refusal reasons form a richer matrix. A later registry schema can carry those policy dimensions without flattening them into a support boolean. |
| `pkg/llmroute/policy.go` — `ConsumesProviderHint`, `HonoursProviderHintValue`, and model twins | Single-value, chain and value-sensitive provider semantics differ. The chain predicate derives; the other dimensions retain their existing conformance tests. |
| `pkg/server/effort.go:234` — `handleEffortCapabilities` | Model-dependent effort levels and live Codex model discovery. The registry owns dial presence; moving the resolver algorithms requires an adapter interface, not a boolean conversion. |
| `pkg/backend/model/capabilities.go:58`, `capabilities_public.go:57` | Provider/model capabilities (context windows, reasoning, temperature), not backend membership. Keep their dynamic modelspecs overlay. |
| `pkg/backend/toolcatalog/capabilities.go:51`, `toolcatalog.go:289`, `refused.go:36` | Tool-level classes, the registered built-in union, and refusal wording. These are not backend lists; actual tool-registry conformance remains their witness. The backend's subagent refusal is separately visible in the table. |
| `pkg/backend/sessionpack/archive.go:240` — `sessionFileNameMatches` | Backend-specific archive filename grammar. Persist eligibility derives above; moving the grammar needs a packer adapter. |
| `pkg/backend/model/slash_commands.go:65` | Claw's expansion algorithm; Claude's native implementation lives in the CLI. Shared capability availability cannot replace either implementation. |
| `cmd/iterion/run.go:155`, `remote_runs.go:590`, `resume.go:145`; `studio/src/api/backends.ts:7` | CLI prose has distinct presentation orders/legacy qualifiers; the TypeScript union needs an exported/generated contract. A follow-up can generate these views from the registry without silently changing labels or transport schema. |
| `pkg/dsl/ir/diag_catalog.go:198`, `validate_tools.go:261`, `validate_fallbacks.go:513` | Explanatory diagnostic prose also describes context-dependent remedies. Membership checks derive now; generated prose needs to preserve transport/sandbox qualifications. |

## Witnesses and how to mutate them

Run the free package guard with:

```sh
devbox run -- go test -mod=vendor ./pkg/backend/backendcaps -count=1
```

[registry.golden.json](../pkg/backend/backendcaps/testdata/registry.golden.json)
is the explicit review surface. Updating it is deliberate:

```sh
devbox run -- go test -mod=vendor ./pkg/backend/backendcaps -run '^TestRegistryGolden$' -update-backendcaps
```

That command **does not** update the independent
[HEAD enumeration oracle](../pkg/backend/backendcaps/testdata/head-enumerations.json),
the documentation, or the ledger. A future implementation/proof change must
reconcile those witnesses explicitly; editing the golden alone cannot waive
the corresponding guard. The historical source commit remains historical.

| Witness | Production mutation that must turn it red |
|---|---|
| `TestSeedZeroMove` | Change codex `permission-ask` from `typed-refused` to `shipped`; replace Claude's `Task` roster entry with `Agent`; alter a probe/preference rank or prompt composition. It compares independent pre-refactor literals, not generated expectations. |
| `TestRegistryGolden` | Change any cell detail/verdict/evidence without updating the golden. The entire inventory and full live observation are compared. |
| `TestLiveLedgerZeroMove` | Exchange a `pass` and a `never` between targets in `live_seed.json`, preserving the totals. The exact target-to-verdict and metadata comparison must fail. |
| `TestPublishedProofZeroMove` | Upgrade a proof cell from `unknown` to `proven`, or rename/re-axis a proof column while retaining its verdict. Both identity and value must agree with the nine published columns. |
| `TestInventoryIntegrity` | Misspell an implementation verdict as `shippped`, duplicate a backend/cell/rank, remove evidence or cite an absent test. |
| `TestCopiesAndUnknowns` | Return `inventory` directly from `Snapshot`, or return the native roster without cloning it. Independent before-images detect either leak. Normalizing unknown names into a registered name must also fail. |
| `model.TestBackendCapabilityConsumersConform` | Replace the provider-chain eligibility consumer with `return false`; the actual consumer must disagree with the registry on claude_code. |

Existing consumer tests remain independent behavioral witnesses: the Claude
14-name literal assertion, IR permission/interaction diagnostics, session
packer capability tests, detect credential isolation, and toolcatalog's real
registry conformance. No live model is needed or run for this migration.

## Epic wording and delivery boundary

The stream path `plans/debt-leap/plan/streams/backend-capability-registry.md`
is absent from this worktree. GitHub's API is unreachable from this session,
and the mission confines writes to this worktree. The remote epic body is
therefore **not edited**. Its stale future wording can be replaced with:

> The seven shipped backend registrations include opencode. The declarative
> capability registry records their existing implementation and proof cells;
> it does not extend the claw ↔ claude_code parity doctrine to the five
> CLI-agent backends. That doctrine and the 19 sub-issues remain open.

No issue is closed, no claim is posted remotely, and no commit is created.

## Review and validation journal

- Plan review: independent HEAD oracle, target-level live evidence, separate
  resume/persist predicates and exact-name semantics are retained. A reviewer
  from a different model family is unavailable in this session.
- Round 1 (two surfaces; opening estimate 6, ceiling 20 at 24 files): the
  consumer review executes **384 independent comparisons** against HEAD,
  including 54 backend spellings and byte-identical diagnostics. Two registry
  guard gaps are fixed: unchecked implementation vocabulary and proof-column
  identity. Three residual mechanical enumerations are derived as well.
- Round 2: the fixed guards survive re-attack. A native-roster alias mutation
  exposes a missing assertion; an independently cloned before-image closes it.
  No high or critical finding remains in either review.
- Round 3 (bounded terminal verification): the independent native-roster
  assertion and named metering evidence are re-read and tested; no new high
  or critical finding.
- **Ten executed mutations turn red on their intended assertions**, then are
  restored: permission membership, native roster spelling, swapped live target
  verdicts, published proof upgrade, golden-only detail, Snapshot alias,
  invalid verdict vocabulary, proof-column identity, native-roster alias, and
  a disconnected provider-chain consumer. No compile failure is counted as
  a mutation witness.

Validation uses devbox, an owned scratch `GOCACHE`, and a per-process Git
configuration isolated from the operator's signing settings. The validation
commands and final results are recorded below; no paid live target runs.

| Check | Result |
|---|---|
| `go build -mod=vendor ./...` | Pass through devbox with owned `GOCACHE`. The read-only host module stat-cache warning is non-fatal. |
| Full suites: backendcaps, automemory, toolcatalog, detect, ir, llmroute | Pass. |
| Full delegate suite | Pass, **358.739 s**, with `GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1`. The initial attempt inherits the unavailable operator GPG signer in two scratch-repository tests; no repository Git setting is changed. |
| Model consumer/session/MCP/provider regression tests | Pass, **21 tests**, including the real task builder, optional-session degradation, provider fallback and new consumer conformance witness. |
| Full model suite | Environment-blocked: `TestRefreshModelSpecs_UpdatesGlobalRegistry` panics because `httptest` cannot bind `[::1]:0` (`socket: operation not permitted`). Not reported as a green suite. |
| Targeted golangci-lint across all touched packages | Pass, **0 issues**. |
| Map freshness and mutation gate | Pass; generated package index includes backendcaps. |
| gofmt / `git diff --check` | Clean. |
| `task check` | Stops in `go vet ./...`: **pre-existing** `pkg/runner/usage_cap_launch_vars_test.go:991` references undefined `assertNodeDeathPark` (also lines 1037/1040). The same references exist in seed HEAD; no runner file is changed. The earlier pnpm cache-path failure is resolved by using the installed Corepack cache. |

These environment/baseline failures are not waived and no unrelated repair is
included. Re-run the full model suite where local sockets are available and
`task check` after the existing runner test helper is repaired. No live target
runs, no ledger moves, no commit.
