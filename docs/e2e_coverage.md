# E2E coverage matrix

> **Superseded as the inventory** by
> [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md), the single
> feature×coverage table (families, statuses, cited tests). This page is kept
> for its claw-code-go-specific detail and the deliberate-gap rationales, which
> the matrix cites rather than repeats.

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
| `TestLive_Full_ExhaustiveDSLCoverage` | every DSL feature (router modes, await convergence, human, tools, compute) | mixed |
| `TestLive_Lite_SessionInheritValidation` | session-inherit between claude_code agents | claude_code |
| `TestLive_Lite_ClawComprehensive` | claw backend across providers, prompt cache obs, retry | claw |
| `TestLive_Lite_ClawBuiltinTools` | read_file, write_file, bash, glob, grep, file_edit, web_fetch | claw |
| `TestLive_Lite_ClawReasoningEffort` | reasoning_effort propagation to EventLLMRequest | claw + openai |
| `TestLive_Lite_ClawMCP` | MCP stdio server discovery + tool calls | claw |
| `TestLive_Lite_ClawLongContext` | 68k-token prompt, no truncation, marker echo | claw + anthropic |

## Current checked-in adapter/integration tests (`go test`, no build tag)

This matrix lists test paths that exist in this workspace. Some claw-code-go
features are covered through iterion adapter tests around the vendored API;
features whose old claw-only tests are not present here are listed as
historical gaps below instead of pointing at non-existent files.

| Feature | Test location | What it asserts |
|---|---|---|
| Computer use (`read_image`) | `pkg/backend/tool/claw_builtins_test.go` (`TestRegisterClawComputerUse_ReadImageRoundTrip`, `TestRegisterClawComputerUse_ReadImagePropagatesError`, `TestRegisterClawComputerUse_ReadImageRejectsHTTPRedirect`) plus `e2e/live_test.go` (`TestLive_Lite_ClawReadImage`) | base64-from-file, missing-input error propagation, **HTTPS→HTTP redirect rejected**, live workflow tool use |
| Computer use (`screenshot` / `computer_use`) | `pkg/backend/tool/claw_builtins_test.go` (`TestRegisterClawComputerUse_ScreenshotPropagatesUnavailable`, `TestRegisterClawComputerUse_ComputerUsePropagatesUnavailable`) | headless unavailable errors propagate for screenshot and action dispatch |
| Session timeline / run inspection | `pkg/cli/cli_test.go` (`TestInspect_WithEvents`, `TestInspect_RunningNodeSectionsIncludeLiveEvents`, node-section inspect tests) | stored events are listed, per-node traces/tools/events preserve recent live events and node metadata |
| OTLP exporter | `pkg/benchmark/otlp_test.go`; `pkg/cloud/tracing/tracing_test.go` | missing endpoint handling, non-blocking event observer, nil Stop, OTLP env endpoint setup and shutdown |
| OAuth broker / PKCE wiring | `pkg/backend/mcp/oauth_test.go`; `pkg/auth/oidc/connector.go` (`GenerateStateAndPKCE`) | broker construction/storage paths, auth closure validation, endpoint security, PKCE state/verifier/challenge generation used by OIDC flows |
| MCP SSE transport/auth wiring | `pkg/dsl/parser/parser_mcp_test.go` (`TestMCPServer_SSETransport`), `pkg/dsl/ir/compile_test.go` (`TestValidateMCPAuth_Unsupported`), `pkg/backend/mcp/oauth_test.go` | SSE transport parses, oauth2 auth blocks validate, auth functions are attached for MCP clients |
| Pi RPC transport + embedded extension | `pkg/backend/delegate/pi_rpc_test.go`, `pi_async_test.go`, `pi_mcp_test.go` (the `TestPiRPCLive*` cases skip when pi/Node fixtures are unavailable) | RPC/print result parity, tool hooks, cancellation/failure typing, shared permission decisions, blocking + async questions, HTTP/SSE/stdio MCP bridges, bounded startup, and SSE cross-origin refusal |

## Coverage gaps (deliberate)

These exist in claw-code-go but have no iterion-level live test. The
listed reason explains why an iterion live test would not add
coverage beyond what's already in place.

| Feature | Why no iterion live test |
|---|---|
| OAuth broker | Requires a real OAuth provider; iterion covers broker construction, auth-function validation, and MCP auth wiring, but does not run a live third-party OAuth flow in e2e fixtures. |
| Plugin marketplace / installer / manager and `/store` command | Historical claw-code-go coverage is not present in this workspace, and iterion does not ship a marketplace integration or plugin installer surface to exercise in live workflows. |
| OTLP exporter | Requires a real collector for an end-to-end live test. iterion server/runner wire OTLP/HTTP through `pkg/cloud/tracing` when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set; checked-in tests cover exporter setup and the benchmark OTLP observer path. |
| CLAUDE.md auto-load | Historical claw TUI command-loader coverage is not present in this workspace. The behavior runs at claw TUI boot, not in an iterion workflow context; iterion uses its own command registry. |
| Bedrock / Vertex / Foundry providers | Requires real cloud creds (AWS/GCP/Azure). Code paths are unit-tested with mocked SDK clients. |
| Lifecycle hooks / ctx propagation into claw hook handlers | Historical claw runtime lifecycle coverage is not present in this workspace. iterion does not currently install a `lifehooks.Runner` on its claw client; adding one is a separate feature. |
| Permission modes `auto` / `dontAsk` | iterion runs in workflow mode (no interactive prompts). Modes are unit-tested at the claw level. |

## How to run

```bash
# Checked-in iterion unit + integration tests (fast):
devbox run -- go test -race -short ./...

# All iterion live workflow tests (slow, costs API quota):
devbox run -- task test:live

# A single live target:
devbox run -- task test:live:claw          # comprehensive claw scenario
devbox run -- task test:live:claw-mcp      # MCP integration
devbox run -- task test:live:review        # session continuity
```
