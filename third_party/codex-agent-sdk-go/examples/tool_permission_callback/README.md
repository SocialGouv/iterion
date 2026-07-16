# Tool Permission Callback Example

This example demonstrates how to use tool permission callbacks to control which tools Claude can use and modify their inputs for safety.

## Overview

The `CanUseTool` callback allows you to:
- **Allow/deny tools** based on tool type and input parameters
- **Modify tool inputs** to redirect operations to safe locations
- **Log tool usage** for auditing and debugging
- **Block dangerous operations** like system directory modifications

## Usage

```bash
go run main.go
```

The example will:
1. Connect to Claude with a custom permission callback
2. Send a query asking Claude to list files, create a program, and read it
3. Intercept each tool use request and apply permission rules
4. Display all tool usage with decisions made

## Permission Rules Implemented

### Read Operations (Always Allow)
- `Read`, `Glob`, `Grep` - Automatically allowed as read-only operations

### Write Operations (Conditional)
- **System directories** (`/etc/`, `/usr/`) - Denied
- **Unsafe paths** - Redirected to `./safe_output/` directory
- **Safe paths** (`/tmp/`, `./`) - Allowed

### Bash Commands (Filtered)
- **Dangerous patterns** - Denied:
  - `rm -rf`
  - `sudo`
  - `chmod 777`
  - `dd if=`
  - `mkfs`
- **Safe commands** - Allowed and logged

### Unknown Tools
- Denied by default (in production, you might prompt the user)

## Example Output

```
🔧 Tool Permission Request: Glob
   Input: {
     "pattern": "*"
   }
   ✅ Automatically allowing Glob (read-only operation)

🔧 Tool Permission Request: Write
   Input: {
     "file_path": "/etc/config.txt",
     "content": "..."
   }
   ❌ Denying write to system directory: /etc/config.txt

🔧 Tool Permission Request: Write
   Input: {
     "file_path": "hello.go",
     "content": "..."
   }
   ⚠️  Redirecting write from hello.go to ./safe_output/hello.go

🔧 Tool Permission Request: Bash
   Input: {
     "command": "rm -rf /"
   }
   ❌ Denying dangerous command: rm -rf /
```

## Customization

Modify the `myPermissionCallback` function to implement your own permission logic:

```go
func myPermissionCallback(
    ctx context.Context,
    toolName string,
    inputData map[string]any,
    permCtx *claudesdk.ToolPermissionContext,
) (claudesdk.PermissionResult, error) {
    // Your custom logic here

    // Allow with modifications
    return &claudesdk.PermissionResultAllow{
        Behavior:     "allow",
        UpdatedInput: modifiedInput,
    }, nil

    // Or deny
    return &claudesdk.PermissionResultDeny{
        Behavior: "deny",
        Message:  "Reason for denial",
    }, nil
}
```

## Security Considerations

- Always validate file paths to prevent directory traversal attacks
- Use allowlists rather than denylists when possible
- Log all permission decisions for audit trails
- Consider the full input parameters, not just simple pattern matching
- Test your permission rules thoroughly

## Related Examples

- `examples/client_multi_turn/` - Demonstrates basic client usage
