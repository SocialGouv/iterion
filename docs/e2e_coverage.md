# E2E coverage matrix

Where each iterion + claw-code-go feature is exercised. Scope is the
combined codebase: iterion's workflow engine + claw-code-go (vendored
via go.mod replace).

## Iterion live workflow tests (`task test:live`)

Tests in `e2e/live_test.go` (build tag `live`). Each runs a real
workflow against real LLMs and asserts on the resulting artifacts /
events.

| Test | Feature exercised | Backend |
|---|---|---|
| `TestLive_Lite_DualModel_PlanImplementReview` | multi-model plan/implement/review pipeline | claude_code + claw |
| `TestLive_Lite_SessionContinuity_ReviewFix` | session forks, review→fix loop, resume | claude_code |
| `TestLive_Full_ExhaustiveDSLCoverage` | every DSL feature (router modes, joins, human, tools) | mixed |
| `TestLive_Lite_SessionInheritValidation` | session-inherit between claude_code agents | claude_code |
| `TestLive_Lite_ClawComprehensive` | claw backend across providers, prompt cache obs, retry | claw |
| `TestLive_Lite_ClawBuiltinTools` | read_file, write_file, bash, glob, grep, file_edit, web_fetch | claw |
| `TestLive_Lite_ClawReasoningEffort` | reasoning_effort propagation to EventLLMRequest | claw + openai |
| `TestLive_Lite_ClawMCP` | MCP stdio server discovery + tool calls | claw |
| `TestLive_Lite_ClawLongContext` | 68k-token prompt, no truncation, marker echo | claw + anthropic |

## Claw-code-go integration tests (`go test`, no build tag)

Each new feature ships with a unit/integration test in
`/workspaces/iterion/.works/claw-code-go/internal/<pkg>/*_test.go` that
uses `httptest` for any external service interaction.

| Feature | Test location | What it asserts |
|---|---|---|
| Computer use (`read_image`) | `internal/tools/computer_use_test.go` | base64-from-file, file size cap, https-only URL, **HTTPS→HTTP redirect rejected**, mutual exclusion |
| Computer use (`screenshot` stub) | same | returns `*api.APIError{StatusCode:501}` |
| Session timeline | `internal/commands/session_timeline_test.go` | chronological order, message truncation, lineage tree, no-fork single node |
| OTLP exporter | `internal/apikit/telemetry/otlp/exporter_test.go` | size flush, interval flush, 5xx retry, 4xx drop, Stop drains |
| OAuth broker (PKCE) | `internal/mcp/oauth/pkce_test.go` | RFC 7636 verifier/challenge, S256 method, state uniqueness |
| OAuth broker (auth code) | `internal/mcp/oauth/broker_test.go` | happy path, state mismatch CSRF defense, refresh on expiry, revoke clears local on remote 4xx |
| Plugin marketplace | `internal/plugins/marketplace_test.go` | catalog fetch, sorted output, case-insensitive search, get-by-name, HTTP error propagation |
| Plugin installer | `internal/plugins/installer_test.go` | download + checksum + extract, **cumulative size cap**, path traversal rejected, idempotent uninstall |
| Plugin manager | `internal/plugins/manager_test.go` | install records state, idempotent install, uninstall drops state, search delegates, **concurrent install serializes** |
| `/store` slash command | `internal/commands/plugin_marketplace_test.go` | install/uninstall/search/list dispatch, no-provider graceful path |
| CLAUDE.md auto-load | `internal/commands/claudemd_loader_test.go` | ancestor walk, slash-block parsing, leaf-wins on conflict, dynamic registration |
| ctx propagation | `internal/runtime/conversation_lifecycle_test.go::TestExecuteTool_PropagatesCtxCancellation` | hook handler observes `context.Canceled` from upstream |
| SSE dynamic auth | `internal/mcp/sse_test.go` | static header, dynamic authFunc takes priority, authFunc error aborts request |

## Coverage gaps (deliberate)

These exist in claw-code-go but have no iterion-level live test. The
listed reason explains why an iterion live test would not add
coverage beyond what's already in place.

| Feature | Why no iterion live test |
|---|---|
| OAuth broker | Requires a real OAuth provider; httptest in claw covers the protocol. iterion doesn't currently consume the broker (no MCP servers with OAuth in fixtures). |
| Plugin marketplace | Requires a real catalog/tarball server; httptest covers it. iterion doesn't ship a marketplace integration. |
| OTLP exporter | Requires a real collector; httptest covers it. iterion has no OTLP wiring. |
| CLAUDE.md auto-load | Runs at the claw TUI boot, not in a workflow context. iterion uses its own command registry. |
| Bedrock / Vertex / Foundry providers | Requires real cloud creds (AWS/GCP/Azure). Code paths are unit-tested with mocked SDK clients. |
| Lifecycle hooks (PreToolUse/Post/Stop) | iterion doesn't currently install a `lifehooks.Runner` on its claw client. Adding one is a separate feature. |
| Permission modes `auto` / `dontAsk` | iterion runs in workflow mode (no interactive prompts). Modes are unit-tested at the claw level. |

## How to run

```bash
# All claw unit + integration tests (fast):
cd /workspaces/iterion/.works/claw-code-go
devbox run --config=/workspaces/iterion -- go -C $(pwd) test -race -short ./...

# All iterion live workflow tests (slow, costs API quota):
cd /workspaces/iterion
devbox run -- task test:live

# A single live target:
devbox run -- task test:live:claw          # comprehensive claw scenario
devbox run -- task test:live:claw-mcp      # MCP integration
devbox run -- task test:live:review        # session continuity
```
