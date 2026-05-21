[← Documentation index](README.md) · [← Iterion](../README.md)

# Why not just prompt-orchestration?

A recurring question when someone first sees Iterion is: *"why a DSL? Can't I get the same thing with a single Claude Code session that calls sub-agents in a loop?"*

The short answer: **yes for creative flexibility, no for the operational guarantees**. The longer answer is below.

## TL;DR

Prompt-orchestration — a tool-using agent (Claude Code, Cursor, plain ChatGPT with code-interpreter…) deciding at runtime *what to do next* and dispatching sub-agents — is fast to author and wonderfully flexible. It is the right tool when the topology of your work changes every run and you'll only run it once or twice.

Iterion picks the other side of the tradeoff. The `.iter` source compiles to a deterministic IR before anything executes. That IR is what unlocks the things you cannot get from a prompt: checkpoint and resume after a 3 a.m. crash, hard cost/token/duration budgets enforced by the runtime, replayable event logs, single-writer workspace safety across parallel branches, bounded loops, schema-checked I/O between nodes, and a long-running dispatcher that turns a tracker into a queue of workflow runs.

Those guarantees aren't a *nice-to-have*. They're the difference between a workflow you can hand to a teammate or leave running over the weekend, and a one-shot experiment that you have to babysit.

## What prompt-orchestration gets right

Be honest about it — there are real cases where Iterion is over-engineered:

- **One-shot exploration.** "Find out why this test flakes." A `.iter` file would be ceremony. A Claude Code session is the right shape.
- **Topology that genuinely changes every run.** If the next step truly depends on what was just discovered — not "branch on a boolean" but "decide between 15 possible directions" — pushing that decision into a router is artificial. A reasoning model orchestrating its own next move is the cleaner abstraction.
- **Zero setup.** No binary to install, no DSL to learn, no `.iter` file to version.
- **Fastest authoring.** A good prompt + a model with subagent dispatch is, for prototype-grade work, faster than designing nodes and edges.

If your work matches all three of *one-shot*, *unpredictable topology*, and *throwaway*, prompt-orchestration is almost certainly the right tool. Skip the rest of this page.

## The structural guarantees that need an IR

The moment you want any of the properties below, prompt-orchestration starts hitting walls that more prompting cannot patch — because the orchestrator is *itself* a stochastic LLM rebuilding its plan from scratch on every invocation.

### 1. Deterministic DAG

The same `.iter` file produces the same graph of nodes and edges every time. The compiler ([pkg/dsl/ir/compile.go](../pkg/dsl/ir/compile.go)) takes the AST to an IR; the validator ([pkg/dsl/ir/validate.go](../pkg/dsl/ir/validate.go)) emits diagnostic codes C001–C086 for structural problems *before* you spend a token.

With a prompt orchestrator, the topology is re-decided on every run. That's a feature for exploration and a bug for reproducibility — you can't diff "what changed between run 7 and run 8" if both runs invented their own plan.

### 2. Checkpoint and resume after crash

A 90-minute autonomous run that dies at minute 80 resumes from minute 75. The engine writes a checkpoint after every successful node ([pkg/runtime/engine.go](../pkg/runtime/engine.go)), and `iterion resume` restarts from the failing node without re-executing upstream work. The full failure matrix is in [resume.md](resume.md).

With prompt-orchestration, a crash means re-running the planner — paying again for everything the previous attempt already did, and hoping the new plan resembles the old one.

### 3. Hard budgets

`max_cost_usd`, `max_tokens`, `max_duration`, `max_iterations`, `max_parallel_branches` are enforced by the runtime ([pkg/runtime/budget.go](../pkg/runtime/budget.go)), not by hoping the model remembers the instruction. The mutex-protected tracker covers all parallel branches; an over-spend cuts the run with a `BUDGET_EXCEEDED` error, not a polite stop.

"Please don't spend more than $5" in a prompt is a soft suggestion. The next surprise context window says hi.

### 4. Inter-node schema validation

Each node declares a schema for its output. The next node receives validated input. A field renamed or omitted by the LLM fails at the boundary, not three nodes downstream with a cryptic null-pointer trace.

With prompt-orchestration, every "the LLM forgot a field" surfaces as a quiet bug in the *consumer*. You spend debugging time figuring out *which* hop dropped it.

### 5. Bounded loops + cycle detection

`src -> dst as loop_name(5)` says: this edge can fire at most 5 times. The validator rejects undeclared cycles. Loop exhaustion is a typed error, not a runaway bill.

A prompt loop relies on the model remembering "I've done this 4 times already." Models do not reliably remember.

### 6. Workspace single-writer safety

The runtime sequences mutating branches — only one agent (or human) is allowed to modify the workspace at a time, while read-only branches run in parallel ([pkg/runtime/routing.go](../pkg/runtime/routing.go)). Two parallel sub-agents from a single prompt orchestrator can — and will, given enough runs — race on the same files and produce broken merges.

### 7. Replayable event log

Every interesting event is appended to `events.jsonl` with a monotonic sequence number — `run_started`, `node_started`, `llm_request`, `tool_called`, `artifact_written`, `human_input_requested`, `run_paused`, `budget_warning`, `edge_selected`, `run_finished`. The on-disk format is documented in [persisted-formats.md](persisted-formats.md), and `iterion report` rebuilds a chronological narrative from it.

Prompt-orchestration leaves you with transcripts. Searchable, but not structured. Audit, debug, and asymptote measurement all suffer.

### 8. Long-running dispatch (tracker → run)

`iterion dispatch config.yaml` is a daemon that polls an issue tracker (native kanban, GitHub Issues, Forgejo) and dispatches one workflow run per eligible issue, with retry, stall detection, per-state concurrency, and lifecycle hooks. See [dispatcher.md](dispatcher.md).

Approximating this from a prompt requires a cron, a state machine, and a place to remember which issues are in flight. At that point you've reimplemented part of Iterion in shell.

### 9. Per-run sandbox isolation

`sandbox: auto` runs every claude_code and tool node inside a long-lived container that bind-mounts the worktree at `/workspace`, with an HTTP CONNECT proxy enforcing a network allowlist. See [sandbox.md](sandbox.md).

A prompt orchestrator running on your laptop has the full filesystem and the full network. That's fine for personal use, not fine for unattended runs you might come back to having destroyed something.

### 10. Backend portability

The same `.iter` runs on `claude_code`, `codex`, or the in-process `claw` backend (which itself talks Anthropic, OpenAI, Bedrock, Vertex…). Swapping a model family is a one-line edit. See [backends.md](backends.md) and [delegation.md](delegation.md).

A prompt-orchestrator is shaped to one host agent's idioms. Porting it to another is a rewrite.

### 11. Asymptote measurement

`iterion bench asymptote` runs the same workflow N times against a fixed corpus and plots quality-vs-iteration to show convergence, ceiling, and variance. See [asymptote-bench.md](asymptote-bench.md). This *requires* a deterministic outer structure — otherwise you're measuring two different workflows.

## A three-question heuristic

1. Will I run this more than five times?
2. Does it need to finish unattended — overnight, over a weekend, in CI?
3. If it fails at 3 a.m., do I need to know *why*, and pick up where it stopped?

Any **yes** → write a `.iter`. All **no** → a Claude Code session with sub-agent dispatch is probably faster.

## The hybrid path

The choice isn't binary. Iterion *welcomes* dynamic sub-agent orchestration **inside** a node — `backend: claude_code` lets the executing agent decide internally which sub-agents to spawn via its Task tool, which files to read, which commands to run. The structural envelope (the DAG, the budget, the schemas, the checkpoint, the sandbox) wraps a freely-orchestrating interior.

In practice most non-trivial Iterion workflows look exactly like this: a small graph of structurally-pinned nodes (plan, implement, judge, fix) whose implementation work each delegates to a Claude Code session that itself dispatches sub-agents as it sees fit. You get the operational guarantees at the outer layer and the creative flexibility at the inner one. The tradeoff this page describes only bites if you try to run the *whole thing* — including the boundaries between phases — as one undifferentiated prompt loop.

## See also

- [why-iterion.md](why-iterion.md) — origin story and the asymptote thesis that motivates the engine
- [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md) — Goodhart's law in workflow design, façade patterns, prompt + judge anti-patterns
- [architecture.md](architecture.md) — compiler pipeline and runtime engine
- [resume.md](resume.md), [dispatcher.md](dispatcher.md), [sandbox.md](sandbox.md) — the three operational guarantees most operators reach for first
