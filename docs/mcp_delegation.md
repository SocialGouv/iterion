# MCP Delegation: API vs CLI Agent Execution

iterion supports two execution models for agent and judge nodes:

1. **API (default)** — iterion calls the LLM provider API directly via goai, manages tool loops, and enforces structured output.
2. **Delegation** — iterion delegates the entire task to an external CLI agent (claude-code or codex) which manages its own tool execution internally.

## Configuration Diff

| Aspect | API approach | MCP delegation |
|--------|-------------|----------------|
| **DSL property** | `model: "anthropic/claude-opus-4-6"` | `delegate: "claude_code"` |
| **Execution** | goai calls provider API, manages tool loop | CLI agent runs autonomously |
| **Tool management** | iterion resolves & executes tools via registry | CLI agent handles its own tools |
| **Structured output** | goai's GenerateObject with JSON Schema | Schema injected in prompt |
| **API keys** | Env vars consumed by goai (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) | Env vars consumed by CLI |
| **Cost control** | Fine-grained token/cost tracking via events | CLI manages internally; estimates returned |
| **Session support** | `fresh` / `inherit` / `artifacts_only` | Always fresh (CLI subprocess limitation) |
| **Prerequisites** | API keys only | CLI tools installed (`claude`, `codex`) + API keys |
| **Retry policy** | Exponential backoff with jitter (configurable) | CLI handles its own retries |
| **Observability** | Full event hooks (OnLLMRequest, OnLLMResponse, OnToolCall) | Limited to delegation start/end |

## DSL Syntax

### API approach (existing)

```
agent claude_review:
  model: "${CLAUDE_MODEL}"
  input: review_request
  output: review_result
  system: pr_review_system
  user: pr_review_user
  session: fresh
  tools: [git_diff, read_file, list_files, search_codebase, tree]
  tool_max_steps: 10
```

### MCP delegation approach

```
agent claude_review:
  delegate: "claude_code"
  input: review_request
  output: review_result
  system: pr_review_system
  user: pr_review_user
  session: fresh
  tools: [git_diff, read_file, list_files, search_codebase, tree]
  tool_max_steps: 10
```

The only change is replacing `model:` with `delegate:`. All other properties (prompts, schemas, tools, edges) remain identical. The `tools` list is forwarded to the CLI agent as allowed tools.

## Available Backends

| Backend | CLI | Provider |
|---------|-----|----------|
| `claude_code` | `claude` (claude-code) | Anthropic |
| `codex` | `codex` | OpenAI |

## When to Use Which

### Use API when:
- You need fine-grained token and cost tracking
- You need structured output guaranteed by JSON Schema
- You need session inheritance between nodes (`session: inherit`)
- You want full observability (per-request/response hooks)
- Latency matters (direct API calls are faster than CLI subprocess)

### Use delegation when:
- The CLI agent has richer built-in tool support (file editing, bash, git)
- You want the agent to run autonomously with its own agentic loop
- The task benefits from the CLI agent's built-in context management
- You want to leverage CLI-specific features (e.g. claude-code's MCP integrations)

## Setup

### Install CLI tools

```bash
# claude-code
npm install -g @anthropic-ai/claude-code

# codex (OpenAI)
npm install -g @openai/codex
```

### Environment variables

Both approaches require the same API keys:

```bash
ANTHROPIC_API_KEY=sk-ant-...
OPENAI_API_KEY=sk-...
```

### MCP server configuration (`.mcp.json`)

The MCP servers are declared for tool discovery:

```json
{
  "mcpServers": {
    "claude_code": {
      "command": "claude",
      "args": ["mcp", "serve"],
      "type": "stdio"
    },
    "codex": {
      "command": "codex",
      "args": ["--mcp"],
      "type": "stdio"
    }
  }
}
```

## Reference Fixtures

| Fixture | Execution model | Description |
|---------|----------------|-------------|
| `pr_refine_dual_model_parallel.iter` | API | Claude & GPT via direct API calls |
| `pr_refine_dual_model_parallel_mcp.iter` | Delegation | Claude via claude-code, GPT via codex |

Both fixtures share the same graph structure, schemas, prompts, and edges. The only difference is the execution backend — `model:` vs `delegate:`.

## Tool List Syntax

Tool references in the DSL now support qualified MCP names with dots:

```
tools: [git_diff, mcp.claude_code.search, read_file]
```

Bare names are resolved via the tool registry (exact match, then MCP shorthand). Qualified names like `mcp.server.tool` are matched exactly.

## Running Live Tests

```bash
# API-based live test (requires ANTHROPIC_API_KEY + OPENAI_API_KEY)
devbox run -- task test:live

# Both API and MCP tests run under TestLive* pattern.
# MCP test skips automatically if claude/codex CLIs are not installed.
```
