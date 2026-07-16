# Examples

This directory contains examples demonstrating the Codex Agent SDK for Go.

## API Overview

The SDK provides two main APIs for interacting with Codex:

### Top-Level Functions (One-Shot)

Simple, stateless functions for single queries:

- **`Query(ctx, content, ...opts)`** - Send `UserMessageContent` and receive streaming responses
- **`QueryStream(ctx, messages, ...opts)`** - Send multiple messages via `iter.Seq[StreamingMessage]`

These are ideal for:
- Simple question/answer interactions
- Batch processing where each query is independent
- Scripts and CLI tools

### Client API (Stateful)

A stateful client for multi-turn conversations and advanced control:

- **`NewClient()`** - Create a new client instance
- **`client.Start(ctx, ...opts)`** - Initialize the connection
- **`client.Query(ctx, content)`** - Send a message (conversation context preserved)
- **`client.ReceiveMessages(ctx)`** / **`client.ReceiveResponse(ctx)`** - Receive responses
- **`client.Interrupt(ctx)`** - Interrupt an ongoing response
- **`client.Close()`** - Clean up resources

This is ideal for:
- Multi-turn conversations with context
- Interactive applications
- Scenarios requiring interrupt capability

## Examples

| Example | Description |
|---------|-------------|
| `quick_start` | Basic usage of the `Query()` function |
| `query_stream` | Using `QueryStream()` with `iter.Seq[StreamingMessage]` |
| `multimodal_input` | Images and local file-path mentions with `UserMessageContent` |
| `client_multi_turn` | Client API with multi-turn conversations, interrupts, and advanced patterns |
| `sessions` | Managing conversation sessions |
| `agents` | Building custom agents |
| `filesystem_agents` | File system operations with agents |
| `mcp_calculator` | MCP server integration |
| `plugin_example` | Plugin system usage |
| `system_prompt` | Customizing system prompts |
| `developer_instructions` | Using `WithDeveloperInstructions()` for additional agent guidance |
| `personality` | Agent personality control with `WithPersonality()` |
| `service_tier` | API service tier selection with `WithServiceTier()` |
| `tools_option` | Configuring allowed tools |
| `sdk_tools` | SDK-defined tools with `WithSDKTools` and `NewTool` |
| `tool_permission_callback` | Custom tool permission handling |
| `stderr_callback` | Capturing CLI stderr output |
| `custom_logger` | Custom logging configuration |
| `max_budget_usd` | Setting cost limits |
| `setting_sources` | Configuration sources |
| `include_partial_messages` | Handling partial/streaming messages |
| `structured_output` | Using `WithOutputFormat()` for structured JSON responses |
| `parallel_queries` | Running concurrent `Query()` calls with errgroup |
| `pipeline` | Multi-step LLM orchestration with Go control flow |

## Running Examples

```bash
# Run a specific example
go run ./examples/quick_start

# Run client_multi_turn with a specific sub-example
go run ./examples/client_multi_turn basic_streaming
go run ./examples/client_multi_turn all
go run ./examples/multimodal_input /absolute/path/to/image.png /absolute/path/to/spec.pdf
```
