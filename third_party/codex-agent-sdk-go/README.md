# Codex Agent SDK Go

Go SDK for building agentic applications with the [Codex CLI](https://github.com/openai/codex).
It provides:

- `Query()` for one-shot requests
- `QueryStream()` for multi-message streaming input
- `Client` for stateful multi-turn sessions

## Requirements

- Go 1.26+
- [Codex CLI](https://github.com/openai/codex) v0.103.0+ in `PATH`

Compatibility constants are defined in `version.go`:

- `Version`
- `MinimumCLIVersion`

## Installation

```bash
go get github.com/ethpandaops/codex-agent-sdk-go
```

## Quick Start

### One-shot query

```go
package main

import (
	"context"
	"fmt"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for msg, err := range codexsdk.Query(ctx, codexsdk.Text("What is 2 + 2?")) {
		if err != nil {
			fmt.Printf("query error: %v\n", err)
			return
		}

		switch m := msg.(type) {
		case *codexsdk.AssistantMessage:
			for _, block := range m.Content {
				if text, ok := block.(*codexsdk.TextBlock); ok {
					fmt.Println(text.Text)
				}
			}
		case *codexsdk.ResultMessage:
			if m.Usage != nil {
				fmt.Printf("done - %d input, %d output tokens\n", m.Usage.InputTokens, m.Usage.OutputTokens)
			}
		}
	}
}
```

`Query()` accepts `UserMessageContent`.
Use `Text(...)` for plain prompts and `Blocks(...)` for structured multimodal input.

### Multi-turn client

```go
client := codexsdk.NewClient()
defer func() {
	if err := client.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close client: %v\n", err)
	}
}()

if err := client.Start(ctx,
	codexsdk.WithLogger(slog.Default()),
	codexsdk.WithPermissionMode("acceptEdits"),
); err != nil {
	return err
}

if err := client.Query(ctx, codexsdk.Text("What's the capital of France?")); err != nil {
	return err
}
for msg, err := range client.ReceiveResponse(ctx) {
	if err != nil {
		return err
	}
	_ = msg
}

if err := client.Query(ctx, codexsdk.Text("What's the population of that city?")); err != nil {
	return err
}
for msg, err := range client.ReceiveResponse(ctx) {
	if err != nil {
		return err
	}
	_ = msg
}
```

### WithClient helper

```go
err := codexsdk.WithClient(ctx, func(c codexsdk.Client) error {
	if err := c.Query(ctx, codexsdk.Text("Hello Codex")); err != nil {
		return err
	}

	for msg, err := range c.ReceiveResponse(ctx) {
		if err != nil {
			return err
		}
		_ = msg
	}

	return nil
},
	codexsdk.WithLogger(slog.Default()),
	codexsdk.WithPermissionMode("acceptEdits"),
)
if err != nil {
	return err
}
```

## API Overview

### Top-level entrypoints

| API | Description |
|---|---|
| `Query(ctx, content, opts...)` | One-shot query returning `iter.Seq2[Message, error]` |
| `QueryStream(ctx, messages, opts...)` | Streams `StreamingMessage` input and yields `Message` output |
| `ListModels(ctx, opts...)` | Returns the complete discovered model list across all CLI pages |
| `ListModelsResponse(ctx, opts...)` | Returns the complete discovered model-list payload across all CLI pages |
| `NewClient()` | Creates a stateful client for interactive sessions |
| `WithClient(ctx, fn, opts...)` | Helper that runs `Start()` + callback + `Close()` |
| `StatSession(ctx, sessionID, opts...)` | Read session metadata from local SQLite database |
| `ListSessions(ctx, opts...)` | List persisted local sessions from the Codex state database |
| `GetSessionMessages(ctx, sessionID, opts...)` | Read parsed persisted rollout messages, including task lifecycle events |

### `Client` methods

| Method | Description |
|---|---|
| `Start(ctx, opts...)` | Connect and initialize a session |
| `StartWithContent(ctx, content, opts...)` | Start and immediately send first message |
| `StartWithStream(ctx, messages, opts...)` | Start and immediately stream input messages |
| `Query(ctx, content, sessionID...)` | Send a user turn |
| `ReceiveResponse(ctx)` | Read messages until `ResultMessage` |
| `ReceiveMessages(ctx)` | Continuous message stream |
| `Interrupt(ctx)` | Stop in-flight generation |
| `SetPermissionMode(ctx, mode)` | Change permission mode during a session |
| `SetModel(ctx, model)` | Change model during a session |
| `GetMCPStatus(ctx)` | Fetch live MCP server connection status |
| `ListModels(ctx)` | Fetch the complete discovered model list across all CLI pages |
| `ListModelsResponse(ctx)` | Fetch the complete discovered model-list payload across all CLI pages |
| `RewindFiles(ctx, userMessageID)` | Rewind tracked files to earlier turn |
| `Close()` | Close and release resources |

### Message and content types

- Message types: `AssistantMessage`, `UserMessage`, `SystemMessage`, `TaskStartedMessage`, `TaskCompleteMessage`, `ThreadRolledBackMessage`, `ResultMessage`, `StreamEvent`
- Content blocks: `TextBlock`, `InputImageBlock`, `InputLocalImageBlock`, `InputMentionBlock`, `ThinkingBlock`, `ToolUseBlock`, `ToolResultBlock`, `UnknownBlock`

`GetSessionMessages` returns the typed persisted rollout events as recorded on disk. Task lifecycle records remain `TaskStartedMessage` and `TaskCompleteMessage`; they are not converted into `ResultMessage`.

`ListModels` returns the fully aggregated model list across all internal `model/list` pages. `ListModelsResponse` returns the same complete model set plus response metadata. Extra provider-specific `model/list` fields are preserved in `ModelInfo.Metadata`. The SDK also decorates known models when Codex does not return them directly:

- `metadata.modelContextWindow`
- `metadata.modelContextWindowSource` (`cli`, `official`, or `runtime`)
- `metadata.maxOutputTokens`
- `metadata.maxOutputTokensSource` (`cli` or `official`)

### Stream helpers

| Helper | Description |
|---|---|
| `Text(text)` | Text-only `UserMessageContent` |
| `Blocks(blocks...)` | Block-based `UserMessageContent` |
| `TextInput(text)` | Text content block |
| `ImageInput(url)` | App-server image block using a URL or data URL |
| `ImageFileInput(path)` | Local image-path block for app-server / exec conversion |
| `PathInput(path)` | Local file/path mention block |
| `SingleMessage(content)` | Single-message input stream |
| `MessagesFromSlice(msgs)` | Stream from `[]StreamingMessage` |
| `MessagesFromChannel(ch)` | Stream from channel |
| `NewUserMessage(content)` | Convenience `StreamingMessage` constructor |

## Multimodal Input

Codex content can be expressed as plain text or structured blocks.

```go
content := codexsdk.Blocks(
	codexsdk.TextInput("Summarize these local inputs."),
	codexsdk.PathInput("/absolute/path/to/spec.pdf"),
	codexsdk.PathInput("/absolute/path/to/notes.txt"),
)

for msg, err := range codexsdk.Query(ctx, content) {
	_ = msg
	_ = err
}
```

For image inputs:

```go
image, err := codexsdk.ImageFileInput("/absolute/path/to/image.png")
if err != nil {
	return err
}

content := codexsdk.Blocks(
	codexsdk.TextInput("Describe this image."),
	image,
)
```

Text-only one-shot queries stay on `codex exec` when the content can be represented there.
Structured content, image URLs/data URLs, and streaming input use app-server semantics.

## Options and Backend Behavior

Options are backend-dependent. Unsupported combinations fail fast with `ErrUnsupportedOption`.

### Backend selection

- `Query(...)` auto-selects backend:
  - Uses `exec` when all selected options are natively supported there.
  - Falls back to `app-server` when needed for selected options.
- `QueryStream(...)` uses app-server semantics unless you inject a custom `WithTransport(...)`.
- `Client.Start(...)` uses app-server transport semantics.

### Common options

| Option | Purpose |
|---|---|
| `WithLogger(logger)` | SDK logs (`*slog.Logger`) |
| `WithModel("gpt-5.4")` | Model selection |
| `WithCwd("/path")` / `WithCliPath("/path/codex")` / `WithEnv(...)` | Process/runtime setup |
| `WithPermissionMode("acceptEdits")` / `WithSandbox("workspace-write")` | Permission/sandbox behavior |
| `WithSystemPrompt("...")` / `WithSystemPromptPreset(...)` | System instructions |
| `WithDeveloperInstructions("...")` | Additional agent instructions (separate from system prompt) |
| `WithPersonality("pragmatic")` | Agent response personality (`"none"`, `"friendly"`, `"pragmatic"`) |
| `WithServiceTier("fast")` | API service tier (`"fast"`, `"flex"`) |
| `WithImages(...)` / `WithConfig(...)` | Codex-native CLI image inputs/config |
| `WithOutputSchema(json)` | Passes `--output-schema` |
| `WithOutputFormat(map[string]any{...})` | Structured output wrapper/schema for app-server flow |
| `WithSkipVersionCheck(true)` | Skip CLI version check |
| `WithInitializeTimeout(d)` | Initialize control request timeout |
| `WithStderr(func(string){...})` | Stderr callback |
| `WithTransport(customTransport)` | Custom transport injection |

### Tool and MCP options

| Option | Purpose |
|---|---|
| `WithHooks(hooks)` | Hook callbacks for tool/session events |
| `WithCanUseTool(callback)` | Per-tool permission callback |
| `WithOnUserInput(callback)` | Handle agent's `requestUserInput` questions (plan mode) |
| `WithTools(...)` / `WithAllowedTools(...)` / `WithDisallowedTools(...)` | Tool allow/block policy |
| `WithPermissionPromptToolName("stdio")` | Permission prompt tool name |
| `WithMCPServers(...)` | Register MCP servers |
| `WithSDKTools(tools...)` | Register SDK-defined tools via `NewTool` (uses `dynamicTools` API) |

### Session and advanced options

| Option | Purpose |
|---|---|
| `WithResume("session-id")` / `WithForkSession(true)` | Resume/fork sessions |
| `WithContinueConversation(true)` | Continue prior conversation |
| `WithEffort(codexsdk.EffortMinimal)` | Reasoning effort (`EffortNone`, `EffortMinimal`, `EffortLow`, `EffortMedium`, `EffortHigh`, `EffortMax`) |
| `WithIncludePartialMessages(true)` | Emit streaming deltas as `StreamEvent` |
| `WithAddDirs("/extra/path")` | Additional accessible directories |
| `WithExtraArgs(map[string]*string{...})` | Raw CLI flags |
| `WithCodexHome("/path/.codex")` | Override Codex home directory for `StatSession` |

### Important caveats

- `WithContinueConversation(true)` requires `WithResume(...)` on app-server paths.
- `WithPermissionPromptToolName(...)` only supports `"stdio"` on app-server paths.
- `WithAddDirs(...)` and `WithExtraArgs(...)` are unsupported on app-server paths.
- `WithOutputSchema(...)` and `WithOutputFormat(...)` serve different integration styles; choose one based on how you want structured output surfaced.

### Streaming delta subtypes

When `WithIncludePartialMessages(true)` is set, each `StreamEvent` carries an
`event.delta` map whose `type` distinguishes the source of the chunk:

| `delta.type` | Source | Payload field |
|---|---|---|
| `text_delta` | Assistant prose (`item/agentMessage/delta`, `item/plan/delta`) | `text` |
| `thinking_delta` | Model reasoning (`item/reasoning/textDelta`, `item/reasoning/summaryTextDelta`) | `thinking` |
| `command_output_delta` | Shell stdout/stderr from a `command_execution` item | `text` (+ `item_id`) |
| `file_change_delta` | Diff output from a `file_change` item | `text` (+ `item_id`) |

`command_output_delta` and `file_change_delta` both include an `item_id` field
inside the delta payload so consumers can correlate the chunk back to the
`ToolUseBlock` emitted at `item.started`. Render those into the tool view
rather than the assistant text stream — only `text_delta` should be treated as
visible assistant prose.

## Session Metadata

Read metadata from a local Codex session using `StatSession`:

```go
stat, err := codexsdk.StatSession(ctx, "550e8400-e29b-41d4-a716-446655440000",
	codexsdk.WithCodexHome("/custom/.codex"), // optional
	codexsdk.WithCwd("/home/user/project"),   // optional: filter by project
)
if err != nil {
	if errors.Is(err, codexsdk.ErrSessionNotFound) {
		log.Fatal("session not found")
	}
	log.Fatal(err)
}
fmt.Printf("Session: %s (tokens: %d)\n", stat.Title, stat.TokensUsed)
```

List sessions or read persisted rollout messages:

```go
sessions, err := codexsdk.ListSessions(ctx,
	codexsdk.WithCodexHome("/custom/.codex"), // optional
	codexsdk.WithCwd("/home/user/project"),   // optional: filter by project
)
if err != nil {
	log.Fatal(err)
}

messages, err := codexsdk.GetSessionMessages(ctx, sessions[0].SessionID)
if err != nil {
	log.Fatal(err)
}
fmt.Printf("Loaded %d messages from persisted rollout\n", len(messages))
```

`StatSession` reads from the Codex CLI's local SQLite database (`~/.codex/state_5.sqlite`).
No running CLI instance is required.

## Error Handling

```go
for msg, err := range codexsdk.Query(ctx, codexsdk.Text(prompt)) {
	if err != nil {
		var cliErr *codexsdk.CLINotFoundError
		if errors.As(err, &cliErr) {
			log.Fatalf("codex CLI not found: %v", cliErr.SearchedPaths)
		}

		var procErr *codexsdk.ProcessError
		if errors.As(err, &procErr) {
			log.Fatalf("codex failed (exit %d): %s", procErr.ExitCode, procErr.Stderr)
		}

		if errors.Is(err, codexsdk.ErrUnsupportedOption) {
			log.Fatalf("unsupported option combination: %v", err)
		}

		log.Fatal(err)
	}

	_ = msg
}
```

| Error | Description |
|---|---|
| `CLINotFoundError` | Codex CLI binary not found |
| `CLIConnectionError` | Connection/init failure |
| `ProcessError` | CLI process exited with error |
| `MessageParseError` | Failed to parse SDK message payload |
| `CLIJSONDecodeError` | JSON decode failure from CLI output |
| `ErrUnsupportedOption` | Option/backend combination is unsupported |
| `ErrSessionNotFound` | Session not found in local database |

## Examples

| Example | Description |
|---|---|
| [`quick_start`](./examples/quick_start) | Basic `Query()` usage |
| [`client_multi_turn`](./examples/client_multi_turn) | Stateful multi-turn client patterns |
| [`query_stream`](./examples/query_stream) | `QueryStream()` with streaming inputs |
| [`multimodal_input`](./examples/multimodal_input) | Images and local file-path mentions with `UserMessageContent` |
| [`mcp_calculator`](./examples/mcp_calculator) | In-process MCP server tools |
| [`mcp_status`](./examples/mcp_status) | Querying MCP server status |
| [`tool_permission_callback`](./examples/tool_permission_callback) | `WithCanUseTool` permission callback |
| [`user_input_callback`](./examples/user_input_callback) | `WithOnUserInput` user input callback (plan mode) |
| [`tools_option`](./examples/tools_option) | Tool allow/block configuration |
| [`structured_output`](./examples/structured_output) | Structured output patterns |
| [`extended_thinking`](./examples/extended_thinking) | `WithEffort(...)` usage |
| [`sessions`](./examples/sessions) | Resume/fork session behavior |
| [`parallel_queries`](./examples/parallel_queries) | Concurrent one-shot queries |
| [`pipeline`](./examples/pipeline) | Multi-step orchestration flow |
| [`sdk_tools`](./examples/sdk_tools) | SDK-defined tools with `WithSDKTools` |
| [`include_partial_messages`](./examples/include_partial_messages) | Real-time streaming deltas with `WithIncludePartialMessages` |
| [`error_handling`](./examples/error_handling) | Typed error handling |

Run examples:

```bash
go run ./examples/quick_start
go run ./examples/client_multi_turn basic_streaming
go run ./examples/query_stream
go run ./examples/multimodal_input /absolute/path/to/image.png /absolute/path/to/spec.pdf
```

## Build and Test

```bash
go build ./...
go test ./...
go test -race ./...
go test -tags=integration ./... # requires Codex CLI + working runtime environment
golangci-lint run
```
