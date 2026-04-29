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
