# Iterion DSL — Common Workflow Patterns

Reusable patterns for building `.iter` workflows. Each pattern includes a skeleton snippet and explanation.

---

## 1. Linear Pipeline

The simplest pattern: nodes execute sequentially.

```iter
agent step_a:
  model: "${MODEL}"
  input: input_schema
  output: step_a_output

agent step_b:
  model: "${MODEL}"
  input: step_b_input
  output: step_b_output

workflow pipeline:
  entry: step_a
  step_a -> step_b with {
    data: "{{outputs.step_a}}"
  }
  step_b -> done
```

---

## 2. Judge-Gated Loop

An agent produces work, a judge evaluates it. If rejected, loop back with feedback. Bounded to prevent infinite execution.

```iter
agent worker:
  model: "${MODEL}"
  input: task_input
  output: task_output
  session: fresh

judge evaluator:
  model: "${MODEL}"
  input: eval_input
  output: eval_output
  session: fresh

workflow review_loop:
  entry: worker

  worker -> evaluator with {
    submission: "{{outputs.worker}}"
  }

  evaluator -> done when approved

  evaluator -> worker when not approved as refine_loop(5) with {
    feedback: "{{outputs.evaluator.summary}}",
    history: "{{outputs.worker.history}}"
  }
```

**Key points:**
- The loop is declared with `as refine_loop(5)` — max 5 iterations
- `{{outputs.worker.history}}` gives the agent all its previous attempts
- The `when` condition field (`approved`) must be `bool` in `eval_output`

---

## 3. Fan-Out / Await (Parallel Execution)

A router sends work to multiple agents in parallel. A downstream node waits for all results.

```iter
router distribute:
  mode: fan_out_all

agent analyzer_a:
  model: "${MODEL}"
  input: analysis_input
  output: analysis_output

agent analyzer_b:
  model: "${MODEL}"
  input: analysis_input
  output: analysis_output

judge synthesizer:
  model: "${MODEL}"
  await: wait_all
  input: synthesis_input
  output: synthesis_output

workflow parallel_analysis:
  entry: distribute

  budget:
    max_parallel_branches: 4

  distribute -> analyzer_a with { data: "{{input.data}}" }
  distribute -> analyzer_b with { data: "{{input.data}}" }

  analyzer_a -> synthesizer with {
    result_a: "{{outputs.analyzer_a}}"
  }
  analyzer_b -> synthesizer with {
    result_b: "{{outputs.analyzer_b}}"
  }

  synthesizer -> done
```

**Key points:**
- `fan_out_all` sends to ALL outgoing edges simultaneously
- `await: wait_all` on the synthesizer makes it wait for both branches
- Set `max_parallel_branches` in budget to control concurrency
- `session: inherit` and `session: fork` are forbidden on nodes with `await` (use `fresh` or `artifacts_only`)

---

## 4. Conditional Routing

A router forwards to different agents based on conditions from upstream output.

```iter
router dispatch:
  mode: condition

workflow conditional:
  entry: classifier

  classifier -> dispatch with {
    is_complex: "{{outputs.classifier.is_complex}}"
  }

  dispatch -> simple_handler when not is_complex
  dispatch -> complex_handler when is_complex

  simple_handler -> done
  complex_handler -> done
```

**Note:** `condition` mode uses `when` clauses on edges. The condition field must exist as a `bool` in the source output schema.

---

## 5. LLM Routing

An LLM decides which target to route to. No `when` conditions on edges.

```iter
prompt router_system:
  Given the input, decide which specialist to route to.
  - code_agent: for code-level issues
  - design_agent: for architecture/design issues

router smart_router:
  mode: llm
  model: "${MODEL}"
  system: router_system

workflow llm_routed:
  entry: smart_router

  smart_router -> code_agent
  smart_router -> design_agent

  code_agent -> done
  design_agent -> done
```

**Key points:**
- LLM router edges must NOT have `when` conditions (C022)
- LLM router needs at least 2 outgoing edges (C021)
- Use `multi: true` to allow the LLM to select multiple targets simultaneously

---

## 6. Human Gate

Pause execution for human approval before proceeding.

```iter
human approval_gate:
  input: approval_input
  output: approval_output
  instructions: approval_instructions
  interaction: human

workflow gated:
  entry: worker

  worker -> approval_gate with {
    submission: "{{outputs.worker}}"
  }

  approval_gate -> done when approved
  approval_gate -> fail when not approved
```

**Key points:**
- `interaction: human` (default for human nodes) always pauses for input
- Use `interaction: llm_or_human` to let an LLM auto-answer when confident
- Resume paused workflows with `iterion resume --run-id <id> --file f.iter --answers-file answers.json`

---

## 7. Delegation (Claude Code / Codex)

Use `delegate` instead of `model` to run the node as an external CLI agent.

```iter
agent implementer:
  delegate: "claude_code"
  input: task_input
  output: task_output
  system: impl_system
  user: impl_user
  session: fresh
  tools: [Read, Edit, Write, Bash, Glob, Grep]
  tool_max_steps: 25
```

**Key points:**
- `delegate: "claude_code"` (recommended) — bypasses LLM API, uses CLI subprocess. `delegate: "codex"` is also accepted but discouraged (compiler emits a `C030` warning); prefer `claude_code` for tool-using agents or `claw` + OpenAI (`model: "openai/gpt-5.4-mini"`) for read-only judges/reviewers
- Delegation supports `interaction` (forwarding human input to the subprocess)
- `readonly: true` marks the node as non-mutating for workspace safety
- Multiple mutating delegates cannot run in parallel (workspace safety constraint)

---

## 8. Tool Node in CI Loop

Use a `tool` node to run shell commands directly (no LLM), combined with a judge for feedback loops.

```iter
tool run_ci:
  command: "${CI_COMMAND}"
  output: ci_result

judge verify:
  model: "${MODEL}"
  input: verify_input
  output: verify_output

agent fixer:
  delegate: "claude_code"
  input: fix_input
  output: fix_output
  tools: [Read, Edit, Write, Bash]

workflow ci_fix:
  entry: fixer

  budget:
    max_iterations: 25

  fixer -> run_ci
  run_ci -> verify with {
    results: "{{outputs.run_ci}}",
    changes: "{{outputs.fixer}}"
  }
  verify -> done when passed
  verify -> fixer when not passed as ci_loop(5) with {
    feedback: "{{outputs.verify.summary}}",
    ci_logs: "{{outputs.run_ci.logs}}"
  }
```

---

## 9. Session Fork for Read-Only Extraction

Fork a session to let multiple readonly agents extract information without consuming the parent session.

```iter
agent worker:
  delegate: "claude_code"
  session: fresh
  tools: [Read, Edit, Write, Bash]

router extract_router:
  mode: fan_out_all

agent summarizer:
  delegate: "claude_code"
  session: fork
  readonly: true
  tools: [Read, Glob, Grep]

agent commit_namer:
  delegate: "claude_code"
  session: fork
  readonly: true
  tools: [Read, Bash]

workflow fork_extract:
  entry: worker

  worker -> extract_router
  extract_router -> summarizer with {
    _session_id: "{{outputs.worker._session_id}}"
  }
  extract_router -> commit_namer with {
    _session_id: "{{outputs.worker._session_id}}"
  }

  summarizer -> done
  commit_namer -> done
```

**Key points:**
- `session: fork` creates a non-consuming fork of the parent session
- `readonly: true` allows multiple agents to read in parallel without workspace safety conflicts
- `_session_id` is passed via `with` to specify which session to fork from

---

## 10. Round-Robin Alternation

Cycle through agents one at a time, useful for dual-model approaches.

```iter
router alternator:
  mode: round_robin

agent model_a:
  model: "claude-sonnet-4-20250514"
  input: task_input
  output: task_output

agent model_b:
  model: "gpt-4o"
  input: task_input
  output: task_output

workflow dual_model:
  entry: alternator

  alternator -> model_a
  alternator -> model_b

  model_a -> evaluator with { result: "{{outputs.model_a}}" }
  model_b -> evaluator with { result: "{{outputs.model_b}}" }

  evaluator -> done when accepted
  evaluator -> alternator when not accepted as alternate_loop(6)
```

**Key points:**
- `round_robin` sends to one target per iteration, cycling through them
- Needs at least 2 outgoing edges (C020)
- Combined with a loop, it alternates between models across iterations
