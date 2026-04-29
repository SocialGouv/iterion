# Roadmap progress — `next_session_roadmap.md`

Tracks tackled in this session, in execution order. Updated after each track.

| Track | Status | Notes |
|---|---|---|
| 0 — Expand RegisterClawBuiltins | ✅ done | 27 internal tools exposed under `pkg/api/tools/`, 5 façade packages (task, worker, team, mcp, lsp), 11 `RegisterClaw*` helpers + `RegisterClawAll`, auto-wired in `cli/run.go`. Unit tests green. Live test skipped (requires API key + 150 LOC fixture). |
| 1 — Lifecycle hooks | ✅ done | `pkg/api/hooks` façade exposed; `WithLifecycleHooks` option on `NewClawExecutor`; `model.WithBackendLifecycleHooks`; `Hooks` field on `GenerationOptions` with PreToolUse/PostToolUse/PostToolUseFailure/Stop dispatch in `executeToolsDirect` and `GenerateObjectDirect`; default `SafetyHook` (rm -rf /, fork bombs, dd of=/dev/sd, mkfs, chmod -R 777 /, shutdown) + `AuditHook` wired by `cli/run.go`. Unit tests green. Live test skipped. |
| 2 — OAuth into MCP | ✅ done | `pkg/api/mcp/oauth` façade exposes `Broker`, `Storage`, `NewBroker`, `WithStorage`, `WithAuthOpener`, `BearerHeaderFunc`. `pkg/api/mcp` exposes `Transport`/`TransportConfig` with new `AuthFunc`. iterion: `ir.MCPAuth` field, `mcp.AuthConfig`, `mcp.OAuthBroker`, `mcp.PrepareAuth`. `cli/run.go` builds a per-store-dir broker (refresh tokens persist under `<storeDir>/mcp_oauth.json`). `.mcp.json` parser extended to read `auth: {type, auth_url, token_url, client_id, scopes, revoke_url}`. `headerRoundTripper` injects fresh tokens on every request. Unit tests green. **Note**: `.iter` parser does NOT yet accept `auth:` blocks — for now, declare OAuth via `.mcp.json`. Live test skipped (would need a mock OAuth provider). |
| 5 — Recovery recipes | ⚠️ partial | Recipes module `runtime/recovery/`: `Recipe` interface, `Action`/`ActionKind` types, 5 default recipes (`RateLimitRecipe`, `ContextLengthRecipe`, `BudgetRecipe`, `TransientToolRecipe`, `PermanentToolRecipe`), `Classify` helper that recognises `*runtime.RuntimeError` codes + `*api.APIError` (429 → RATE_LIMITED, "context_length_exceeded" → CONTEXT_LENGTH_EXCEEDED), and 4 new error codes (`RATE_LIMITED`, `CONTEXT_LENGTH_EXCEEDED`, `TOOL_FAILED_TRANSIENT`, `TOOL_FAILED_PERMANENT`). Unit tests green. **Deferred**: engine-level dispatch in `runtime/engine.go` (consult recipes around `executor.Execute` failure) — current path still calls `failRunWithCheckpoint` directly. The recipes are usable as a public API today; wiring them into the engine retry loop is a follow-up. |
| 6 — Telemetry dashboard | ⚠️ partial | Two-command stack delivered: `docs/observability/docker-compose.yml` (otel-collector + Tempo + Prometheus + Grafana), pre-provisioned datasources, Grafana dashboard `iterion-workflow.json` with 7 panels (cost per node, tokens per model, retry rate, p50/p95/p99 duration, parallel branches, top-10 cost runs, tool calls). README documents required metric names as a schema contract. **Deferred**: emitter translation in iterion (`claw TelemetryEvent → iterion_*` Prometheus metrics) — dashboard is the contract; emitter is the next implementation step. |
| 4 — LLM permission classifier | ⚠️ partial | `internal/permissions/llm_classifier.go`: `LLMClassifier{Client, Model, Fallback, Cache, MaxTokens}` + `ClassifierCache{ttl, store}` (TTL-based, sha256 keys). Strict-JSON prompt + decision parser tolerates code fences. Fallback path skips LLM for fast cases (RuleClassifier short-circuit). Unit tests exercise fallback short-circuit, ask→LLM fallthrough, cache hit, malformed responses, and expiry. **Deferred**: ModeAuto integration to inject LLMClassifier via Option (currently the manager only accepts a static Classifier; wiring it via the existing `WithClassifier` option is straightforward — left for follow-up to keep this iteration moving). |
| 7 — Plugin sigstore | ⚠️ partial | `PluginEntry` extended with `signature_url`, `signature_bundle`, `certificate_identity`, `certificate_oidc_issuer` (schema-only). `docs/plugin_signing.md` documents the cosign flow and explains why crypto verification is deferred (sigstore-go dep choice has trust-policy implications). Catalog authors can populate these fields today; installer will start enforcing in a follow-up. |
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

- **Track 5**: wire recovery recipes into `runtime/engine.go` retry loop.
- **Track 6**: emitter that translates `claw TelemetryEvent` → `iterion_*` Prometheus metrics.
- **Track 4**: inject `LLMClassifier` via `WithClassifier` option in `permissions.Manager`.
- **Track 7**: pull in `github.com/sigstore/sigstore-go` and wire signature verification at install time. Requires trust-policy decision (keyless vs key-based).
- **Track 2**: extend the `.iter` parser/AST to accept top-level `auth:` blocks under `mcp_server` declarations (currently only `.mcp.json` parses OAuth).
- **Track 0**: add `TestLive_Lite_ClawSubagents` (~150 LOC fixture + test exercising `agent` tool spawning children).
