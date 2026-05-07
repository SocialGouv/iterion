[← Documentation index](README.md) · [← Iterion](../README.md)

# Delegation

For tasks that need full tool access (file editing, shell commands, git operations), you can delegate agent execution to an external CLI agent instead of making direct LLM API calls:

```iter
agent implementer:
  delegate: "claude_code"          # recommended (codex is supported but discouraged)
  input: plan_schema
  output: result_schema
  system: implementation_prompt
  tools: [read_file, write_file, run_command, git_diff]
```

| Backend | Status | What it does |
|---------|--------|-------------|
| `claude_code` | recommended | Runs the `claude` CLI as a subprocess with full tool access |
| `claw` (default) | recommended for read-only / judges | In-process multi-provider LLM client (Anthropic, OpenAI, …) — use with `model: "openai/gpt-5.4-mini"` etc. |
| `codex` | **discouraged** | Runs the `codex` CLI as a subprocess. Cannot configure its tool set, tends to fill its own context window, and has weaker iterion integration. The compiler emits a `C030` warning per node. Kept for compatibility — prefer `claude_code` or `claw`+OpenAI in new workflows. |

> 💡 `claude_code` works with your Claude subscription (Pro/Max/Team/Enterprise) — no separate API key required. `claw` calls provider APIs directly and needs the corresponding API key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, …).

Delegation is useful for agents that need to *act* on the codebase (write files, run tests, execute commands). For agents that only need to *think* (review, judge, plan), use `model:` directly — it's lighter weight and faster.

You can mix both in the same workflow. A common pattern is using `model:` for reviewers and judges, and `delegate:` for implementers:

```iter
agent reviewer:
  model: "claude-sonnet-4-20250514"    # Direct API call — fast, read-only
  readonly: true

agent implementer:
  delegate: "claude_code"              # Full agent — can edit files
  tools: [read_file, write_file, patch, run_command]
```
