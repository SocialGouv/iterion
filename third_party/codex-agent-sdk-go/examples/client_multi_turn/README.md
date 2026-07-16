# Streaming Client Examples

Comprehensive examples demonstrating various patterns for building applications with the Claude SDK Go streaming interface.

## Overview

This example collection showcases 8 different patterns for interactive client usage:

1. **basic** - Basic streaming with context manager
2. **multi_turn** - Multi-turn conversations
3. **concurrent** - Concurrent send/receive using goroutines
4. **interrupt** - Interrupt capability demonstration
5. **manual** - Manual message stream handling with custom logic
6. **options** - Using ClaudeAgentOptions for configuration
7. **bash** - Tool use blocks when running bash commands
8. **control** - Control protocol capabilities (SetPermissionMode, SetModel)

## Usage

Run a specific example:
```bash
go run main.go <example_name>
```

Run all examples sequentially:
```bash
go run main.go all
```

List available examples:
```bash
go run main.go
```

## Examples

### Basic Streaming
Simple query and response pattern using the helper method `ReceiveResponse()`.

```bash
go run main.go basic
```

### Multi-Turn Conversation
Demonstrates maintaining context across multiple conversation turns.

```bash
go run main.go multi_turn
```

### Concurrent Send/Receive
Shows how to handle responses while sending new messages using goroutines and channels.

```bash
go run main.go concurrent
```

### Interrupt
Demonstrates how to interrupt a long-running task and send a new query.

```bash
go run main.go interrupt
```

### Manual Message Handling
Process messages manually with custom logic - extracts programming language names from responses.

```bash
go run main.go manual
```

### Custom Options
Configure the client with `ClaudeAgentOptions` including allowed tools, system prompts, and permission modes.

```bash
go run main.go options
```

### Bash Command
Shows tool use blocks when Claude executes bash commands.

```bash
go run main.go bash
```

### Control Protocol
Demonstrates runtime control capabilities like changing permission modes and models.

```bash
go run main.go control
```

## Key Patterns

### Context Management
All examples use `context.WithTimeout` for proper lifecycle management:

```go
ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
defer cancel()
```

### Client Lifecycle
```go
client := claudesdk.NewClient(log)
defer client.Close()

if err := client.Connect(ctx, options); err != nil {
    // handle error
}
```

### Message Processing
```go
// Iterator-based response (stops at ResultMessage)
for msg, err := range client.ReceiveResponse(ctx) {
    if err != nil {
        // handle error
        break
    }
    // process message
}

// Continuous message streaming (yields indefinitely)
for msg, err := range client.ReceiveMessages(ctx) {
    if err != nil {
        break
    }
    // process message
    if _, ok := msg.(*claudesdk.ResultMessage); ok {
        break // exit when done
    }
}
```

### Concurrent Operations
```go
var wg sync.WaitGroup
done := make(chan struct{})

wg.Add(1)
go func() {
    defer wg.Done()
    // Use iter.Pull2 for pull-based iteration in goroutines with select
    next, stop := iter.Pull2(client.ReceiveMessages(ctx))
    defer stop()
    for {
        select {
        case <-done:
            return
        default:
            msg, err, ok := next()
            if !ok || err != nil {
                return
            }
            // process message
        }
    }
}()

// ... do work ...
close(done)
wg.Wait()
```

## Notes

The queries in these examples are intentionally simplistic. In real applications, queries can be complex tasks where Claude SDK uses its agentic capabilities and tools (bash commands, file operations, web search, etc.) to accomplish goals.
