# Roadmap progress — `next_session_roadmap.md`

Tracks tackled in this session, in execution order. Updated after each track.

| Track | Status | Notes |
|---|---|---|
| 0 — Expand RegisterClawBuiltins | ✅ done | 27 internal tools exposed under `pkg/api/tools/`, 5 façade packages (task, worker, team, mcp, lsp), 11 `RegisterClaw*` helpers + `RegisterClawAll`, auto-wired in `cli/run.go`. Unit tests green. **Live test added in 2026-04-29 session**: `TestLive_Lite_ClawSubagents` exercises `RegisterClawSubagents` end-to-end (parent + 2 children via `agent` tool), validates registration + tool dispatch contract; passes in 3.4 s. ExecuteAgent in iterion's stateless path returns metadata only (no real spawn) — that requires the per-node ConversationLoop work in Track 5; the live test is honest about what it proves. |
| 1 — Lifecycle hooks | ✅ done | `pkg/api/hooks` façade exposed; `WithLifecycleHooks` option on `NewClawExecutor`; `model.WithBackendLifecycleHooks`; `Hooks` field on `GenerationOptions` with PreToolUse/PostToolUse/PostToolUseFailure/Stop dispatch in `executeToolsDirect` and `GenerateObjectDirect`; default `SafetyHook` (rm -rf /, fork bombs, dd of=/dev/sd, mkfs, chmod -R 777 /, shutdown) + `AuditHook` wired by `cli/run.go`. Unit tests green. Live test skipped. |
| 2 — OAuth into MCP | ✅ done | `pkg/api/mcp/oauth` façade exposes `Broker`, `Storage`, `NewBroker`, `WithStorage`, `WithAuthOpener`, `BearerHeaderFunc`. iterion: `ir.MCPAuth`, `mcp.AuthConfig`, broker wired in `cli/run.go` (refresh tokens persist under `<storeDir>/mcp_oauth.json`). **Closed in 2026-04-29 session**: `.iter` parser now accepts `auth: { type, auth_url, token_url, client_id, scopes, revoke_url }` blocks under `mcp_server`; AST `MCPAuthDecl` + IR mapping in `compileMCPAuth`; compile-time `validateMCPAuth` flags non-oauth2 schemes and missing required fields. Unit tests + parser tests green. |
| 5 — Recovery recipes | ✅ done | Recipes module `runtime/recovery/` (5 default recipes + `Classify` + 4 error codes) **wired into the engine in 2026-04-29 session**. New `runtime.RecoveryDispatch` / `RecoveryAction` / `Compactor` surface; `runtime/engine.go::handleNodeFailure` consults the dispatcher on every executor failure with per-(node, code) attempt tracking that resets on success. RetrySameNode honours `Delay` with ctx cancellation; PauseForHuman writes a synthetic `recovery` interaction so `iterion resume --answers-file` works. Wired by default in `cli/run.go`. **Real compaction landed in 2026-04-29 session**: new `model/session.go` adds a per-(runID, nodeID) `nodeSessionStore` — claw backend reads prior messages from ctx via `withRuntimeContext`, prepends them to `opts.Messages`, then captures the final accumulated list via the new `TextResult.Messages` field. `GenerateTextDirect` returns a partial result on error so compaction has something to shrink even on failed attempts. `ClawExecutor.Compact` invokes the new `pkg/runtime.CompactMessages` façade (re-exporting `CompactSessionPure` from claw-code-go) on the stored session; sessions are evicted on successful node finish. 8 new session tests + 5 dispatch tests + 9 recovery tests green. |
| 6 — Telemetry dashboard | ✅ done | Two-command stack (otel-collector + Tempo + Prometheus + Grafana) with 7-panel dashboard. **Closed in 2026-04-29 session**: `benchmark/PrometheusExporter` emits the 7 metrics the dashboard queries. Wired via `model.EventHooks` (LLM request/retry/response, tool calls, per-node tokens/cost via new `OnNodeFinished`) and `runtime.WithEventObserver` (parallel-branches gauge fed by new `branch_finished` event in `fan_out.go`). `model.ChainHooks` composes the exporter with `StoreEventHooks` so events still land in `events.jsonl`. Cost computed from `cost/cost.go` price table (Anthropic + OpenAI). Enabled via `ITERION_PROMETHEUS_ADDR=:9464`; Prometheus scrape config preconfigured for `host.docker.internal:9464`. **Backend coverage closed in 2026-04-29 session**: `model/cost.go` extracted into a leaf `cost/` package so `delegate/` can call `cost.Annotate` without an import cycle. `delegate/claude_code.go` and `delegate/codex.go` now annotate their output with `_tokens` / `_model` / `_cost_usd` from the SDKs' `ResultMessage.Usage` blocks (claude_code) and `thread.token_usage.updated` events (codex). Same Prometheus counters fire for all three backends. `docs/observability/README.md` Backend coverage table updated. 5 exporter tests + 8 cost tests green. |
| 4 — LLM permission classifier | ✅ done | `internal/permissions/llm_classifier.go` already done; **wired in 2026-04-29 session**. `permissions.Manager` gained `SetClassifier(c Classifier)` (consulted between policy and legacy paths; classifier `Ask`/error falls through, `Allow`/`Deny` short-circuit). `pkg/permissions` re-exports `Classifier`/`LLMClassifier`/`RuleClassifier`/`ClassifierCache` + `NewRuleClassifier`/`NewClassifierCache`. iterion-side adapter `tool.ClassifierChecker` wraps the claw classifier into iterion's `tool.ToolChecker` and chains over the existing static policy. Enabled via `ITERION_LLM_CLASSIFIER_MODEL=anthropic/claude-haiku-4-5` (or any provider/model spec); classifier reuses iterion's model registry to resolve the API client and uses a 30-min TTL cache. 6 manager tests + 6 adapter tests green. |
| 7 — Plugin sigstore | ✅ done | `PluginEntry` schema (`signature_url`, `signature_bundle`, `certificate_identity`, `certificate_oidc_issuer`) is now load-bearing. **Closed in 2026-04-29 session**: new `internal/plugins/verifier.go` defines a `SignatureVerifier` interface with a `CosignVerifier` impl that shells out to the `cosign` CLI (chosen over `sigstore-go` to keep transitive dep growth at zero — operators who care about signing already have cosign on PATH). Auto-detect: keyless mode when `certificate_identity` or `certificate_oidc_issuer` is set, key-based mode otherwise (PEM via `CLAW_PLUGIN_PUBLIC_KEY` env or programmatic `PublicKeyPEM`/`PublicKeyFile`). Opt-in via `CLAW_REQUIRE_SIGNED=1` (rejects entries without signature material). `Installer.Install` invokes the verifier after the SHA-256 check and aborts before extraction on failure. `Marketplace.Fetch` summary changed from "WARNING: not yet implemented" to "INFO: N/M signed; verification runs at install time". 5 new tests (mock verifier + auto-detect short-circuits) + the existing marketplace summary test (updated) all green. `docs/plugin_signing.md` rewritten to document the operator controls. |
| 3 — Marketplace Option A | ✅ done | New `cmd/claw-store-init` scaffolder builds a local plugin marketplace at `~/.claw/marketplace` (or custom `--dir`) with: `catalog.json` template, `serve.go` static-HTTP fronter, `README.md`. Smoke-tested. Use `CLAW_MARKETPLACE_URL=http://localhost:8080` to point `/store` slash commands at the local catalog. |
| 8 — Editor Inspector | ✅ done | `pnpm install` + `pnpm build` green (tsc + vite). 9148 modules transformed without TypeScript errors. Verified zero remaining references to deleted modals (`EditEdgeModal`, `EditItemModal`, `EditPromptModal`, `EditSchemaModal`, `EditVarModal`). All 6 Inspector components present (`Inspector`, `InspectorEdge`, `InspectorNode`, `InspectorMulti`, `InspectorEditItem`, `InspectorEmpty`). E2E Playwright tests not yet set up — recommended as a future addition. |
| 10 — Codex sandbox | ✅ already done | `delegate/codex.go::codexSandboxForAllowedTools` already translates per-node `task.AllowedTools` to a least-privilege codex sandbox: empty/read-only tools → `read-only`, any mutating tool (Bash/Edit/Write/NotebookEdit) → `workspace-write`. Wired via `codexsdk.WithSandbox`. `danger-full-access` deliberately never auto-selected. No additional work needed for this track. |
| 9 — IR ↔ editor bidirectional | ⏭️ skipped | Roadmap-flagged as ⚠️ scope risk (4-5 sessions). Requires formatter-aware AST: capture byte ranges + comments at parse, reuse original spans in unparse for unmodified nodes, plus an editor `POST /workflow` endpoint and round-trip preservation tests. Deferred to a dedicated session. The current `unparse/` is functional but lossy on comments — sufficient for read-only visualization. |

## Push state

- claw-code-go: not pushed. Both repos remain in working state for user review before push.
- iterion: not pushed. The `replace github.com/SocialGouv/claw-code-go => ./.works/claw-code-go` directive in `go.mod` is still active.
- Both repos: `task check` (iterion) and `go test ./...` (claw-code-go) green.
- User will push manually after reviewing the diffs.

## Skipped / blocked items

- **Track 9 — IR ↔ editor bidirectional**: skipped per user direction. Scope is 4-5 sessions (formatter-aware AST). To unblock: either accept lossy unparse (current behavior, no comments survive a round trip) or schedule a dedicated session for the parser/unparse work.

## Deferred items (partial track follow-ups)

Tracks 0 / 2 / 4 / 5 / 6 / 7 closed across the 2026-04-29 sessions. The
roadmap's 10 tracks are fully delivered; only Track 9 remains
intentionally skipped (formatter-aware AST is a dedicated multi-session
project — see `next_session_roadmap.md`).

### Notes on the four 2026-04-29 follow-ups

- **Track 7 sigstore**: implemented via cosign subprocess rather than
  pulling in `github.com/sigstore/sigstore-go` (~30 transitive deps).
  Future session may swap in the in-process binding if a deployment
  needs offline / private-Rekor verification — the
  `SignatureVerifier` interface is the swap-in point.
- **Track 0 follow-up**: `TestLive_Lite_ClawSubagents` validates
  registration + tool dispatch but **not** real subagent spawning,
  because iterion's claw backend does not run a per-node
  `ConversationLoop`. ExecuteAgent returns a metadata spec; the LLM
  reports it back. Real spawning would build on the Track 5
  session-per-node groundwork.
- **Track 6 backend coverage**: handled by extracting `cost/` as a
  leaf package and calling `cost.Annotate` from the claude_code and
  codex delegates after their SDKs surface usage blocks. Codex
  catches usage from `thread.token_usage.updated` system events;
  claude_code from `ResultMessage.Usage`. Fully symmetric coverage
  across all three backends.
- **Track 5 real compaction**: `model.nodeSessionStore` keeps an
  accumulated message list per `(runID, nodeID)`. The claw backend
  prepends prior messages on retry and captures the new accumulated
  list at the end of each attempt (success or failure — partial
  results are returned by `GenerateTextDirect` on error). On
  `RecoveryCompactAndRetry`, `ClawExecutor.Compact` calls the new
  `pkg/runtime.CompactMessages` façade (re-exporting
  `internal/runtime.CompactSessionPure`) on the stored list, and the
  next retry sends the summarised history. Sessions evict on node
  finish.
