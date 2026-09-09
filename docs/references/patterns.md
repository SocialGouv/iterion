# Iterion DSL — Common Workflow Patterns

Reusable patterns for building `.bot` workflows. Each pattern is a complete
workflow that compiles as written (the repository's tests compile every
snippet on this page), so it can be copied into a `.bot` and adapted. A
`${MODEL:-…}` model spec keeps the model overridable from the environment.

---

## 1. Linear Pipeline

The simplest pattern: nodes execute sequentially.

```iter
schema request:
  request: string

schema step_a_output:
  data: string

schema step_b_input:
  data: string

schema step_b_output:
  result: string

agent step_a:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: request
  output: step_a_output

agent step_b:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: step_b_input
  output: step_b_output

workflow pipeline:
  entry: step_a
  step_a -> step_b with {
    data: "{{outputs.step_a.data}}"
  }
  step_b -> done
```

---

## 2. Judge-Gated Loop

An agent produces work, a judge evaluates it. If rejected, loop back with feedback. Bounded to prevent infinite execution.

```iter
schema task_input:
  task: string
  feedback: string

schema task_output:
  result: string

schema eval_input:
  submission: json

schema eval_output:
  approved: bool
  summary: string

agent worker:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: task_input
  output: task_output
  session: fresh

judge evaluator:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
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
- When the loop's five iterations are spent, the back-edge is declined and
  the evaluator has no edge left, so the run fails `NO_OUTGOING_EDGE`; add a
  bare `evaluator -> <exit>` edge (the loop-exhaustion exit, exempt from
  C010) to route the exhausted case somewhere useful instead

---

## 3. Fan-Out / Await (Parallel Execution)

A router sends work to multiple agents in parallel. A downstream node waits for all results.

```iter
schema analysis_input:
  data: string

schema analysis_output:
  findings: string

schema synthesis_input:
  result_a: json
  result_b: json

schema synthesis_output:
  summary: string

router distribute:
  mode: fan_out_all

agent analyzer_a:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: analysis_input
  output: analysis_output

agent analyzer_b:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: analysis_input
  output: analysis_output

judge synthesizer:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
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
- `session: inherit` and `session: fork` are forbidden on nodes with `await` (use `fresh`, `artifacts_only`, or `persist`)
- `{{input.data}}` on `distribute -> analyzer_*` is **router pass-through**: every router mode copies its input onto its output (an `llm` router also records the selection on that map), and an edge `with` mapping resolves `{{input.*}}` against that source output. Here `distribute` is the **entry**, so its input is the run payload. A mid-graph router only has what its incoming `with` mappings supplied — map the field onto it, or use `{{vars.*}}`. It is not a fallback to run-level inputs.

---

## 4. Conditional Routing

A router forwards to different agents based on conditions from upstream output.

```iter
schema classification:
  is_complex: bool

schema handled:
  result: string

agent classifier:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: classification

router dispatch:
  mode: condition

agent simple_handler:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: handled

agent complex_handler:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: handled

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
schema handled:
  result: string

prompt router_system:
  Given the input, decide which specialist to route to.
  - code_agent: for code-level issues
  - design_agent: for architecture/design issues

router smart_router:
  mode: llm
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  system: router_system

agent code_agent:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: handled

agent design_agent:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: handled

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
schema work_output:
  summary: string

schema approval_input:
  submission: json

schema approval_output:
  approved: bool
  notes: string

prompt approval_instructions:
  Review the submission below and approve it, or reject it with a note.

agent worker:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  output: work_output

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
- Resume paused workflows with `iterion resume --run-id <id> --file f.bot --answers-file answers.json`

---

## 7. Delegation (coding-agent CLIs)

Pin `backend` to run the node through an external coding-agent CLI instead of
leaving executor selection to credential detection. `claude_code`, `codex`,
`pi`, `kimi`, and `grok` are supported delegates; all but `claude_code` are
explicit opt-ins. A separate `model:` pin is optional and does not select the
backend.

```iter fragment
agent implementer:
  backend: "claude_code"
  input: task_input
  output: task_output
  system: impl_system
  user: impl_user
  session: fresh
  tools: [read_file, file_edit, write_file, bash, glob, grep]
  tool_max_steps: 25
```

**Key points:**
- `backend: "claude_code"`, `codex`, `pi`, `kimi`, and `grok` select supported coding-agent CLIs with different capability boundaries. For an in-process review, set both `backend: "claw"` and a model such as `model: "openai/gpt-5.4-mini"`.
- Delegation supports `interaction` (forwarding human input to the subprocess)
- `readonly: true` marks the node as non-mutating for workspace safety
- Multiple mutating delegates cannot run in parallel (workspace safety constraint)
- A `tools:` list uses the snake_case built-in names; it constrains `claw`
  and is inert on CLI backends, which keep their full native toolset

---

## 8. Tool Node in CI Loop

Use a `tool` node to run shell commands directly (no LLM), combined with a judge for feedback loops.

```iter
schema ci_result:
  passed: bool
  logs: string

schema verify_input:
  results: json
  changes: json

schema verify_output:
  passed: bool
  summary: string

schema fix_input:
  feedback: string
  ci_logs: string

schema fix_output:
  changes: string

tool run_ci:
  ## The exit code becomes a field: a failing suite is a RESULT the judge
  ## reads, not a node failure — and stdout is the JSON the schema declares.
  command: `if ${CI_COMMAND:-make test} >/tmp/ci.log 2>&1; then ok=true; else ok=false; fi; jq -Rs --argjson passed "$ok" '{passed: $passed, logs: .[-20000:]}' </tmp/ci.log`
  output: ci_result

judge verify:
  model: "${MODEL:-anthropic/claude-sonnet-4-6}"
  input: verify_input
  output: verify_output

agent fixer:
  backend: "claude_code"
  input: fix_input
  output: fix_output

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

**Key points:**
- A tool node's stdout is its output: print a JSON object matching the
  `output:` schema (`{"passed": true, "logs": "…"}`). Any other stdout is
  silently wrapped as `{"result": "…"}` and the declared fields are absent
  downstream, and a non-zero exit fails the node — hence the wrapper, which
  turns the exit code into a field and keeps only the log's last 20 000
  characters (the judge reads that field; nothing else bounds it)

---

## 9. Session Fork for Read-Only Extraction

Fork a session to let multiple readonly agents extract information without consuming the parent session.

```iter
agent worker:
  backend: "claude_code"
  session: fresh
  tools: [read_file, file_edit, write_file, bash]

router extract_router:
  mode: fan_out_all

agent summarizer:
  backend: "claude_code"
  session: fork
  readonly: true
  tools: [read_file, glob, grep]

agent commit_namer:
  backend: "claude_code"
  session: fork
  readonly: true
  tools: [read_file, bash]

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
schema task_input:
  task: string

schema task_output:
  result: string

schema verdict:
  accepted: bool

router alternator:
  mode: round_robin

agent model_a:
  model: "anthropic/claude-sonnet-4-6"
  input: task_input
  output: task_output

agent model_b:
  backend: "claw"
  model: "openai/gpt-5.5"
  input: task_input
  output: task_output

judge evaluator:
  model: "anthropic/claude-sonnet-4-6"
  output: verdict

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
