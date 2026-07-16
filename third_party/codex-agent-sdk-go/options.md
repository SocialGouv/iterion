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

## Streaming Delta Subtypes (`WithIncludePartialMessages`)

When `WithIncludePartialMessages(true)` is set, the SDK emits `StreamEvent`
messages whose `event.delta.type` distinguishes the source of the chunk so
consumers do not misclassify tool output as assistant prose:

| `delta.type` | Source notification | Payload |
|---|---|---|
| `text_delta` | `item/agentMessage/delta`, `item/plan/delta` | `text` |
| `thinking_delta` | `item/reasoning/textDelta`, `item/reasoning/summaryTextDelta` | `thinking` |
| `command_output_delta` | `item/commandExecution/outputDelta` | `text`, `item_id` |
| `file_change_delta` | `item/fileChange/outputDelta` | `text`, `item_id` |

`command_output_delta` and `file_change_delta` include `item_id` in the delta
payload so consumers can attach the chunk to the corresponding `ToolUseBlock`
emitted at `item.started` instead of mixing it into the assistant text stream.

Prior to this change, all four notification kinds were translated to a generic
`text_delta`, leaving consumers unable to tell shell command output (e.g.
`gh issue view` HTTP traces) from real assistant prose. Consumers that already
matched on `delta.type == "text_delta"` continue to receive assistant prose
(and plan deltas) as before; tool output now arrives under its own subtype.

