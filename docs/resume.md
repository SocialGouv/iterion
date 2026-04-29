# Resume — Restarting Failed, Cancelled, and Paused Runs

## Overview

Iterion saves a **checkpoint** after every successful node execution. When a run
fails or is cancelled, the checkpoint is preserved so that `iterion resume` can
restart from the failing node without re-executing upstream nodes.

Three run statuses support resume:

| Status                 | Trigger                          | Answers required? | `--force` useful? |
|------------------------|----------------------------------|-------------------|-------------------|
| `paused_waiting_human` | Human node reached               | Yes               | No                |
| `failed_resumable`     | Any non-terminal failure         | No                | Yes (if .iter changed) |
| `cancelled`            | User interrupt (SIGINT/Ctrl-C)   | No                | Yes (if .iter changed) |

## CLI Usage

```bash
# Resume a paused run (requires answers)
iterion resume --run-id <id> --file workflow.iter --answers-file answers.json

# Resume a failed run (no answers needed)
iterion resume --run-id <id> --file workflow.iter

# Resume after editing the .iter file (e.g., fixing a bug)
iterion resume --run-id <id> --file workflow.iter --force
```

## Checkpoint Semantics

- **After each node**: a best-effort checkpoint is saved with `NodeID` = the just-completed node.
- **On failure**: a final checkpoint is saved with `NodeID` = the failing node. The failing node's output is **not** in the checkpoint (it failed before producing output).
- **On cancellation**: a checkpoint is saved with `NodeID` = the node that was about to execute.
- **On resume**: `execLoop` starts from `checkpoint.NodeID`, re-executing that node.

The checkpoint contains: `NodeID`, `Outputs` (per-node), `LoopCounters`, `RoundRobinCounters`, `ArtifactVersions`, `Vars`. Budget is **reset** on resume (upstream nodes are not re-executed, so cost is minimal).

## Exhaustive Failure Matrix

Every failure path in the engine, its resulting status, and whether it's resumable.

### Main Execution Loop (`engine.go`)

| Failure | Status | Resumable | Restart node |
|---------|--------|-----------|--------------|
| Node not found in workflow graph | `failed_resumable` | Yes | The missing node ID |
| Reached `FailNode` (intentional) | `failed` | **No** | — |
| Node execution error (LLM, delegate) | `failed_resumable` | Yes | The failing node |
| Output schema validation error | `failed_resumable` | Yes | The node with bad output |
| Edge selection: no matching edge | `failed_resumable` | Yes | The node with no outgoing edge |
| Edge selection: loop exhausted | `failed_resumable` | Yes | The node at loop boundary |

### Context / Timeout (`helpers.go`)

| Failure | Status | Resumable | Restart node |
|---------|--------|-----------|--------------|
| Context cancelled (SIGINT) | `cancelled` | Yes | Current node |
| Context deadline exceeded (timeout) | `failed_resumable` | Yes | Current node |

### Budget (`helpers.go`)

| Failure | Status | Resumable | Restart node |
|---------|--------|-----------|--------------|
| Budget exceeded (100%+) pre-execution | `failed_resumable` | Yes | Node that was about to run |
| Budget hard limit (90%+) pre-execution | `failed_resumable` | Yes | Node that was about to run |
| Budget exceeded (100%+) post-execution | `failed_resumable` | Yes | Node that just ran |

### Router Nodes (`engine.go` → `fan_out.go`, `routing.go`)

| Failure | Status | Resumable | Restart node |
|---------|--------|-----------|--------------|
| Fan-out: no outgoing edges | `failed_resumable` | Yes | Router node |
| Fan-out: workspace safety violation | `failed_resumable` | Yes | Router node |
| Fan-out: convergence point not found | `failed_resumable` | Yes | Router node |
| Fan-out: branch execution failure (wait_all) | `failed_resumable` | Yes | Router node |
| Fan-out: branch context cancelled | `failed_resumable` | Yes | Router node |
| Round-robin: no outgoing edges | `failed_resumable` | Yes | Router node |
| LLM router: execution failure | `failed_resumable` | Yes | Router node |
| LLM router: invalid selection | `failed_resumable` | Yes | Router node |

### Resume-Time Failures (`resume.go`)

| Failure | Status | Resumable | Restart node |
|---------|--------|-----------|--------------|
| Edge selection after human answers | `failed_resumable` | Yes | Human node |
| Human auto_or_pause execution error | `failed_resumable` | Yes | Human node |
| Schema validation in auto mode | `failed_resumable` | Yes | Human node |

### Non-Resumable Failures

| Failure | Status | Why not resumable |
|---------|--------|-------------------|
| Reached `FailNode` | `failed` | Intentional workflow termination |
| First node fails (no prior checkpoint) | `failed` | No state to resume from |
| `failed` (legacy, pre-checkpoint era) | `failed` | No checkpoint was saved |

## `--force` Flag

By default, resume validates that the `.iter` source file has not changed since the run started (via SHA-256 hash). If it has changed, resume is refused.

The `--force` flag bypasses this check. This is useful when:
- Fixing a bug in the workflow that caused the failure
- Changing a model spec after a model-related failure
- Adjusting budget limits after a budget exceeded error

The hash mismatch is logged as a warning when `--force` is used.

## ask_user Pause/Resume

When an LLM calls the `ask_user` tool mid-run, iterion pauses the workflow and
surfaces the question to the dev's terminal. Resume routes the answer back to
the same node.

The mechanics differ between in-process (`claw`) and CLI (`claude_code`,
`codex`) backends because only the in-process path can persist conversation
state.

### `claw` backend — native conversation persistence

At the moment the LLM emits `tool_use(ask_user)`, the generation layer captures:

- The full `[]api.Message` history (the original user prompt, every prior
  tool_use/tool_result pair, and the assistant message holding the pending
  `ask_user` tool_use).
- The `tool_use.id` of the pending call.

These travel up through `delegate.ErrAskUser` → `delegate.Result`
(`PendingConversation`, `PendingToolUseID`) → `model.ErrNeedsInteraction` →
`store.Checkpoint` (`BackendConversation`, `BackendPendingToolUseID`).

On resume, the runtime relays the persisted blob back through `nodeInput`
(`_resume_conversation`, `_resume_pending_tool_use_id`, `_resume_answer`) into
`delegate.Task.Resume*`. The claw backend then takes a resume path: it skips
the system+user prompt rendering and instead replays the persisted conversation
plus a single user message containing a `tool_result` content block answering
the captured `tool_use`. The agent loop continues from where it left off.

Multi-turn `ask_user` accumulates naturally: each pause snapshots the live
message slice, which already contains every prior tool_result. A run with
three pauses persists a conversation of growing length, never losing earlier
exchanges.

### CLI backends (`claude_code`, `codex`) — prompt-side fallback

CLI backends spawn `claude` / `codex` subprocesses and cannot persist API
message state across pauses. They use the existing fallback: the runtime
injects `_prior_ask_user_question` / `_prior_ask_user_answer` into `nodeInput`,
and `prependPriorAskUser` adds a `[PRIOR INTERACTION]` block to the user
prompt so the (stateless) LLM knows what it asked and what the human answered.

For `claude_code`, the `ask_user` tool is exposed natively via an in-process
MCP self-server (`iterion __mcp-ask-user` subcommand) plus a `PreToolUse` hook
that captures the question and short-circuits the SDK session. The
`[INTERACTION PROTOCOL]` JSON-output suffix remains active as a graceful
fallback if the LLM bypasses the native tool.

### Field reference

| Layer | Field | Type |
|-------|-------|------|
| `delegate.ErrAskUser` | `Question`, `PendingToolUseID`, `Conversation` | string, string, json.RawMessage |
| `delegate.Task` | `ResumeConversation`, `ResumePendingToolUseID`, `ResumeAnswer` | json.RawMessage, string, string |
| `delegate.Result` | `PendingConversation`, `PendingToolUseID` | json.RawMessage, string |
| `model.ErrNeedsInteraction` | `Conversation`, `PendingToolUseID` | json.RawMessage, string |
| `store.Checkpoint` | `BackendConversation`, `BackendPendingToolUseID` | json.RawMessage, string |
| `nodeInput` (runtime↔executor) | `_resume_conversation`, `_resume_pending_tool_use_id`, `_resume_answer` | json.RawMessage, string, string |

The `json.RawMessage` keeps the conversation shape backend-specific; only the
claw backend marshals/unmarshals it as `[]api.Message`.

## Implementation Details

### Store Methods

- `SaveCheckpoint(id, cp)` — saves checkpoint without changing status (best-effort, after each node)
- `FailRunResumable(id, cp, error)` — atomically sets `failed_resumable` + checkpoint + error
- `UpdateRunStatus(id, status, error)` — clears checkpoint for `running`/`finished`/`failed`, preserves for `failed_resumable`/`cancelled`

### Engine Options

- `WithForceResume(bool)` — enables `--force` behavior
- `WithWorkflowHash(string)` — sets the hash for change detection

### Key Files

| File | Role |
|------|------|
| `pkg/store/run.go` | `RunStatusFailedResumable` constant, `Checkpoint` struct |
| `pkg/store/store.go` | `FailRunResumable()`, `SaveCheckpoint()`, checkpoint preservation logic |
| `pkg/runtime/engine.go` | Best-effort checkpoint after each node, `WithForceResume` option |
| `pkg/runtime/helpers.go` | `buildCheckpoint()`, `failRunWithCheckpoint()`, `failRunErrWithCheckpoint()`, `handleContextDoneWithCheckpoint()` |
| `pkg/runtime/resume.go` | `Resume()` dispatch, `resumeFromPause()`, `resumeFromFailure()`, `checkWorkflowHash()` |
| `pkg/cli/resume.go` | CLI validation, `--force` flag, status-dependent answer requirements |
