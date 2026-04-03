# Option Changes (Unsupported Options Removed)

Date context: February 20, 2026 (`codex-cli 0.103.0`)

This SDK removed options that were unsupported on both built-in backends (`exec` and `app-server`).

## Removed From Public API

The following constructors were removed from `options.go`:

- `WithThinking`
- `WithMaxBudgetUSD`
- `WithMCPConfig`
- `WithSandboxSettings`
- `WithFallbackModel`
- `WithBetas`
- `WithSettings`
- `WithMaxBufferSize`
- `WithUser`
- `WithAgents`
- `WithSettingSources`
- `WithPlugins`
- `WithEnableFileCheckpointing`

Their backing fields were also removed from `CodexAgentOptions` (`internal/config/options.go`).

## Example Cleanup

Examples that depended on removed options and no longer represented supported behavior were removed from test discovery by deleting their `main.go` entrypoints:

- `examples/agents/main.go`
- `examples/filesystem_agents/main.go`
- `examples/plugin_example/main.go`
- `examples/setting_sources/main.go`

## Rewritten Examples

These examples were kept but updated to use supported behavior:

- `examples/extended_thinking/main.go`
  - now demonstrates `WithEffort` (supported) instead of removed thinking controls.
- `examples/max_budget_usd/main.go`
  - now demonstrates client-side soft budget logic using `ResultMessage.Usage` token counts.
- `examples/stderr_callback/main.go`
  - removed unsupported startup flags and still demonstrates stderr callback capture.

## Remaining Important Caveat

`WithExtraArgs` remains available for `Query(...)` when it stays on the `exec` backend.
It is still unsupported on `app-server` paths (`Client.Start`, `QueryStream`, or `Query` when routed to app-server).

