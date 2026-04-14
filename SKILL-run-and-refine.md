---
name: iterion-run-and-refine
description: >
  Use when asked to test, run, debug, or optimize an .iter workflow against
  real data. Covers the full cycle: launch a run, observe behavior, diagnose
  failures, fix the workflow or engine, resume, and iterate until the workflow
  runs reliably and produces quality results.
  Triggers on: "run this workflow", "test the .iter", "debug the run",
  "optimize the pipeline", "why did the workflow fail", "improve parity",
  "the workflow is stuck in a loop".
version: 0.1.0
---

# Running and Refining Iterion Workflows

This skill covers the practice of taking an .iter workflow from "validates OK" to "runs reliably and produces quality results at scale." It is based on hard-won experience running multi-hour, multi-batch workflows against real codebases.

## The core loop

The refinement process is not linear. It follows a tight observe → diagnose → fix → resume cycle:

```
1. Launch the workflow
2. Monitor events and artifacts
3. When something fails or stagnates:
   a. Read the error or inspect the verdict
   b. Diagnose the root cause (workflow design? prompt? engine bug? infra?)
   c. Fix the .iter file (or engine code if needed)
   d. Resume from the last checkpoint with --force
4. Repeat until the workflow runs end-to-end
5. Observe multi-iteration behavior (loops, batch progression)
6. Refine prompts and routing based on what the LLM actually does
```

This is not a one-shot process. Expect 5-15 iterations before a complex workflow runs smoothly.

## Launching a run

```bash
# Load environment (API keys, model names)
set -a && source .env && set +a

# Run with variables
./iterion run examples/my_workflow.iter \
  --var repo_path="/path/to/repo" \
  --store-dir .my-store \
  --log-level info \
  --timeout 4h
```

Run in the background to keep working while it runs:
```bash
# Same command, but backgrounded — note the output file path for tail -f
```

## Monitoring a running workflow

### Quick status check
```python
import json
with open('.my-store/runs/<run_id>/run.json') as f:
    d = json.load(f)
print(f"Status: {d['status']}")
cp = d.get('checkpoint', {})
print(f"Checkpoint: {cp.get('node_id', '?')}")
print(f"Loops: {cp.get('loop_counters', {})}")
```

### Event timeline
```python
import json
with open('.my-store/runs/<run_id>/events.jsonl') as f:
    for line in f:
        e = json.loads(line)
        # type, node_id, data are the key fields
```

Key event types to watch:
- `edge_selected` — shows routing decisions (condition, loop iteration)
- `delegate_finished` — duration, tokens, formatting_pass_used, parse_fallback
- `run_failed` — the error message and failing node
- `run_paused` — human input needed
- `artifact_written` — check published artifacts for quality

### Artifact inspection
```bash
ls .my-store/runs/<run_id>/artifacts/
# Each node that publishes has a directory with versioned JSON files
```

Read the latest verdict to understand progress:
```python
import json, glob
files = sorted(glob.glob('.my-store/runs/<run_id>/artifacts/review_verdict/*'))
with open(files[-1]) as f:
    data = json.load(f)['data']
# Inspect batch_complete, parity_percentage, blockers, suggestions
```

## Resuming from failures

This is the most important capability. Every failure is an opportunity to fix and resume without losing prior work.

```bash
# Resume a failed run (re-executes the failing node)
./iterion resume --run-id <id> --file workflow.iter --store-dir .my-store

# Resume after editing the .iter file (--force bypasses hash check)
./iterion resume --run-id <id> --file workflow.iter --store-dir .my-store --force

# Resume a paused run with human answers
./iterion resume --run-id <id> --file workflow.iter --store-dir .my-store \
  --answers-file answers.json
```

**Critical: always use `--force` after editing the .iter file.** Without it, the hash mismatch rejects the resume.

**Answers must use correct JSON types.** `--answer 'proceed=true'` passes a string. Use `--answers-file` with a JSON file for booleans:
```json
{"proceed": true, "plan_summary": "Approved", "questions": []}
```

## Common failure patterns and fixes

### 1. Model spec format mismatch

**Symptom:** `invalid spec "claude-opus-4-6" (expected "provider/model-id")`

**Cause:** Nodes using goai directly (human nodes with `interaction: llm`, goai-based agents) require `provider/model-id` format (e.g., `anthropic/claude-opus-4-6`). Backend delegates (`backend: "claude_code"`) use their own auth and accept bare model names.

**Fix:** Use `provider/model-id` format on human nodes. Use `backend:` instead of `model:` on agent/judge nodes that should go through CLI delegation.

### 2. Structured output empty or invalid

**Symptom:** `structured output invalid: missing required field "X"` despite the delegate running successfully (exit_code=0, high token count).

**Cause:** The Claude Code CLI did real work (file edits, tool calls) but the SDK didn't capture structured output. Common with backend agents where tools are implicit.

**Diagnosis:** Check `delegate_finished` event: `raw_output_len=0` or `parse_fallback=true` with `formatting_pass_used=false` means the recovery didn't trigger.

**Fix (engine):** The delegate should attempt a recovery formatting pass when output is empty or fallback-only. This resumes the session with `WithOutputFormat` to extract the structured result.

**Fix (workflow):** If the node is a backend agent doing heavy tool work, ensure the prompt explicitly asks to "produce the structured output at the end."

### 3. Tool node environment issues

**Symptom:** `go: not found`, `sh: syntax error`, command not found.

**Cause:** Tool nodes execute in a minimal `sh` shell without the user's PATH, devbox, nvm, etc.

**Fix:** Either prefix the command with the full PATH (`export PATH=/path/to/bin:$PATH && ...`) or use a wrapper script. Avoid complex inline shell — use an external script for anything beyond simple commands.

**Also:** `${ENV_VAR}` in tool commands is resolved at compile-time. If the env var is missing at compile time, the literal `${...}` string becomes the command. Use `{{input.field}}` via edge mapping for runtime values.

### 4. Loop stagnation

**Symptom:** The fix loop runs 5+ times with the same issues reappearing. Parity doesn't increase.

**Diagnosis:** Compare consecutive verdicts. If the same blockers appear, the fix agent is failing to resolve them (or the review keeps finding new issues of similar nature).

**Fixes:**
- **Blocker vs suggestion classification:** Only production-breaking issues should block. Cosmetic differences (format strings, quoting style, whitespace) should be suggestions that don't trigger fix iterations.
- **Stagnation detection:** Pass the previous verdict to the current verdict. If same blockers reappear, declare batch_complete=true and move on.
- **Acceptance threshold:** Make it configurable (strict/normal/lenient) so the human gate can adjust per batch.
- **Self-critique in judges:** Before each FAIL, the judge should ask: "Would this break production? Is this about the current batch? Has this appeared before?"

### 5. Verdict evaluates global parity instead of batch parity

**Symptom:** `overall_parity` is always false because 60% of features haven't been attempted yet. The workflow loops endlessly trying to fix issues that require new planning, not fixes.

**Fix:** Separate `batch_complete` (current batch passes) from `overall_parity` (everything done). The verdict evaluates the batch; the routing uses `batch_complete` for fix-vs-plan decisions.

### 6. No fallback when loops exhaust

**Symptom:** `NO_OUTGOING_EDGE` or `LOOP_EXHAUSTED` when fix_loop reaches its max.

**Cause:** When a loop-bounded edge is exhausted, the runtime skips it. If no other edge matches, the node has no exit.

**Fix:** Always add a fallback edge after loop-bounded edges. The edge evaluation order in the .iter file is the edge priority:
```iter
verdict -> done when overall_parity           # 1. done if complete
verdict -> plan when batch_complete            # 2. next batch
verdict -> fix when not batch_complete as fix_loop(5)  # 3. fix bugs
verdict -> plan when not overall_parity as outer_loop(50)  # 4. fallback
verdict -> done                                # 5. terminal fallback
```

### 7. Codex backend instability

**Symptom:** `no result message received` from Codex, intermittent failures ~1/3 calls.

**Current mitigation:** `failed_resumable` + resume catches every failure. The resume re-executes the fan-out, retrying both branches.

**Consideration:** For critical paths, consider using Claude for both judges instead of Codex. Use Codex only for independent consolidation where a failure can be retried cheaply.

## Parity tracking in porting workflows

Getting accurate progress measurement was one of the hardest problems.

### What doesn't work

- **Static manifest:** The initial analysis produces a feature manifest. After 5 batches, it's stale — it shows 38% parity while the code has tripled in size.
- **Reviewer cumulative scan:** Asking the reviewer to also scan the entire repo for cumulative parity produces wildly inconsistent results (93% → 87% → 68%) because the reviewer is already overloaded with review + verdict + fix planning.
- **Line counting:** A shell script counting Go/Rust lines gives a number (44%) but it doesn't measure feature parity — it measures code volume.

### What works

A **dedicated parity scanner agent** that runs after tests and before review. Its only job is to read both codebases and produce an updated feature manifest. It:
- Has tools to list and read files in both repos
- Receives the previous manifest as context
- Publishes an updated `feature_manifest` artifact (overwriting the stale one)
- Is not overloaded with other duties

This adds ~5-8 min per batch but gives the verdict and planners an accurate view of progress.

## Session continuity patterns

Session management is the single biggest lever for quality.

| Phase | Session mode | Why |
|-------|-------------|-----|
| Implementation | `inherit` | Remembers previous iterations |
| Simplify | `inherit` from impl | Sees what was just changed |
| Commit namer | `fork` from simplify | Reads context without modifying |
| Review | `fork` from simplify | Sees the code, readonly |
| Fix | `inherit` from reviewer | Knows both the code AND the review findings |

**Critical rule:** All session-continuous agents must use the same model and backend. Switching models breaks the KV cache.

## Quality control: the simplify pass

Without it, each batch accumulates 3-7 suggestions (non-idiomatic patterns, dead code, duplication). Over 10+ batches, this becomes significant technical debt.

A `simplify` agent between implementation and commit:
- Inherits the implementation session
- Focuses on the files just changed
- Removes dead code, deduplicates, applies idiomatic patterns
- Does NOT add features or change behavior
- Adds ~5 min per batch

## Human-in-the-loop: when to pause

The `llm_or_human` interaction mode on a plan gate lets an LLM decide whether to auto-approve or pause for human review. In practice:
- Simple batches (basic type ports, straightforward features) auto-proceed
- Complex batches (concurrency, architectural changes, large scope) trigger a pause
- The longest autonomous stretch observed was **2h25m** — multiple batches of plan → implement → review → verdict without intervention

## Practical tips

1. **Start with `--log-level info`** — debug and trace produce too much noise for long runs.

2. **Keep a terminal open with `tail -f` on the output** — you'll see node transitions, delegation starts/finishes, and edge selections in real-time.

3. **Check `run.json` frequently** — the checkpoint and loop counters tell you exactly where the workflow is and how many iterations remain.

4. **Don't over-engineer prompts upfront** — run the workflow, see what the LLM actually does, then adjust. Most prompt improvements came from observing actual verdicts and seeing where the judge was too strict or too lenient.

5. **Budget generously on first runs** — `max_cost_usd: 500`, `max_tokens: 50000000`, `max_iterations: 100`. Tighten after you understand the workflow's behavior.

6. **The `--force` flag is your best friend** — every fix to the .iter file can be applied immediately via resume without losing work.

7. **Keep the answers file ready** — for workflows with human gates, pre-create an approval JSON file so you can resume quickly:
   ```json
   {"proceed": true, "plan_summary": "Approved", "questions": []}
   ```

8. **Watch for the `parse_fallback` field** — if `delegate_finished` shows `parse_fallback: true`, the structured output wasn't captured cleanly. The recovery formatting pass should handle this, but if it recurs, the prompt may need to be more explicit about producing JSON output.

9. **Verdicts are your compass** — every verdict artifact tells you what the LLM thinks about the work. Read them. If the verdict says "92% parity" but you know the code is incomplete, the verdict prompt needs adjustment.

10. **Don't fight loop exhaustion — use it** — when a fix loop exhausts, let the fallback route to the next planning cycle. The stagnant issues become feedback for better planning. Fighting to fix every last issue in one batch wastes iterations.

## What to improve next

Based on observed limitations, here are areas where the approach could be better:

- **Automatic retry on transient backend failures** — Codex crashes ~1/3 of the time. An automatic retry (with backoff) before marking as failed_resumable would reduce manual intervention.

- **Parallel review + parity scan** — currently sequential (test → parity_scanner → review). Both could run in parallel since they're independent reads of the same codebase.

- **Cross-batch learning** — the planner receives `previous_feedback` but doesn't see the full history of what worked and what didn't across all batches. A summary of past batch outcomes would help planning.

- **Adaptive fix loop bounds** — instead of a fixed max (5), the fix loop could observe its own progress (are blockers decreasing?) and decide whether another iteration is worthwhile.

- **Cost tracking per batch** — the budget tracks global cost but doesn't break it down per batch. Knowing that "batch 5 cost $12 and fixed 3 features" would help optimize the workflow.

## See Also

- [SKILL.md](SKILL.md) — DSL reference for writing .iter files
- [examples/rust_to_go_port.md](examples/rust_to_go_port.md) — detailed design notes for a production workflow
- [docs/resume.md](docs/resume.md) — exhaustive failure matrix and resume semantics
- [examples/](examples/) — workflow examples of increasing complexity
