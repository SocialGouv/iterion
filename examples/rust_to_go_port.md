# rust_to_go_port — Workflow Design & Lessons Learned

## What this workflow does

`rust_to_go_port.iter` orchestrates the porting of a complete codebase from one language to another using dual-model AI agents. It was designed and refined through iterative live runs porting [claw-code](https://github.com/ultraworkers/claw-code) (Rust CLI) to Go.

The workflow is generic. Replace the repo paths and language-specific prompts and it adapts to any cross-language migration (Python to Go, JS to Rust, etc.) or more broadly to any multi-phase implementation task with iterative review loops.

## Architecture

```
Analysis → Planning → [Human Gate] → Implementation → Simplify → Commit → Test
                ↑                                                          ↓
                |                                                    Parity Scan
                |                                                          ↓
                |                                                       Review
                |                                                          ↓
                |                                                    Dual Verdict
                |                                                     ↙       ↘
                +——— outer_loop (next batch) ←—— batch_complete     not batch_complete
                                                                          ↓
                                                                       Fix Loop ←→ Review
                                                                          ↓ (exhausted)
                                                                   fallback → done
```

21 nodes, 27 edges, 6 visual groups. Two nested loops: an inner fix loop for correcting bugs in the current batch, and an outer loop (max 50) for progressing through successive feature batches. A terminal fallback ensures the workflow always exits cleanly.

## Core principles

### Batch-based progression

A large porting project has hundreds of features. Trying to port everything in one pass is intractable. The workflow breaks the work into dependency-ordered batches. Each batch goes through a full cycle: plan → implement → review → fix. When the batch is "good enough", the outer loop plans the next batch using feedback from the previous one.

The key insight: **the verdict must evaluate the current batch, not overall parity**. Early versions evaluated global parity (always false when 60% of features hadn't been attempted yet), causing infinite fix loops on issues that couldn't be fixed without new planning.

### Deterministic parity tracking

The initial feature manifest is a point-in-time snapshot. After several batches, it becomes stale — it doesn't reflect the code actually ported. Early versions asked the reviewer to perform a cumulative parity scan alongside its review, but this proved unreliable: the reported parity fluctuated wildly (93% → 87% → 68%) because the scan was subjective and the reviewer was already overloaded with three other roles.

The solution: a **deterministic `parity_scan` tool node** that runs a shell script counting Go source files, test files, lines, packages, and comparing with the Rust codebase. It produces a factual `line_ratio_percent` (Go lines / Rust lines) that the verdict uses for its `parity_percentage`. This is instant (<1s), perfectly reproducible, and monotonically increasing as code is added.

The verdict uses this deterministic ratio instead of a subjective LLM estimate. The reviewer focuses exclusively on reviewing the current batch — no more cumulative scanning duty.

### Convergence over perfection

The workflow optimizes for forward progress, not perfection. Several mechanisms prevent stagnation:

- **Blocker vs suggestion classification**: only production-breaking issues block a batch. Cosmetic differences (quoting style, whitespace, non-idiomatic patterns) are tracked as suggestions but don't trigger fix iterations.
- **Stagnation detection**: the verdict receives the previous verdict as input. If the same blockers appear across iterations, the batch is declared complete anyway — the stagnant issues become feedback for the next planning cycle.
- **Configurable acceptance threshold**: `strict` (zero issues), `normal` (zero blockers), or `lenient` (minor blockers OK). Adjustable per run via the `acceptance_threshold` variable or by the human gate.
- **Bounded fix loop with fallback**: the fix loop runs at most 5 times. When exhausted, a fallback edge routes to the next planning cycle rather than crashing.

Without these mechanisms, the first live run stagnated for 11+ fix iterations on cosmetic issues (backtick quoting, TrimSpace, format string style) that the fix agent would "fix" only for the review to find new cosmetic issues.

### Session continuity is everything

The single most impactful optimization was introducing session continuity across related phases:

- **Implementation** uses `session: inherit` across iterations — the agent remembers what it changed previously and builds on it.
- **Review** uses `session: fork` from implementation — the reviewer sees the implementer's full context (files changed, decisions made) without needing it summarized in the prompt. This is readonly.
- **Fix** uses `session: inherit` from the reviewer — the fixer has seen both the code AND the review, so it knows exactly what to fix and why.

Using `session: fresh` everywhere (the original design) meant every node started blind. The review couldn't verify actual code changes, and the fix couldn't see what the review found. Session continuity dramatically improved both review accuracy and fix quality.

**Corollary**: all session-continuous agents must use the same model and backend. Switching models breaks the KV cache. This is why the workflow uses a single backend (claude_code) for the entire hot path.

### Dual judgment with self-critique

The verdict uses two independent judges (Claude + Codex) that must agree. This catches single-model blind spots. But dual judgment creates a new problem: false positive blockers from either judge can stall the loop.

The solution is a two-phase evaluation in each judge:
1. **Rigorous evaluation**: find all issues, cite code
2. **Self-critique**: for each blocker, ask three questions:
   - "Would this break a real user in production?"
   - "Is this about the current batch or an unported future feature?"
   - "Has this exact issue appeared in the previous verdict already?"

False positives are reclassified as suggestions. Stagnant blockers are reclassified as suggestions. This pattern originated from a previous project where judges drifted on criteria, flagging correct implementations as wrong and costing entire pipeline iterations (~10 min each).

The synthesis node then applies a cross-judge filter: an issue flagged as blocker by both judges is very likely real; an issue flagged by only one judge is probably a suggestion.

### Human-in-the-loop without blocking

The `plan_gate` node uses `interaction: llm_or_human` — a lightweight LLM (GPT-5.4-mini) evaluates the plan's risk and decides whether to auto-approve or pause for human review.

High-risk batches (concurrency, FFI, architectural changes, large scope) trigger a pause. Straightforward batches auto-proceed. This keeps the workflow autonomous for routine work while catching decisions that benefit from human judgment.

The human can also adjust the `acceptance_threshold` at this point, tightening or loosening quality criteria for the upcoming batch.

In practice, the workflow ran autonomously for up to **2h25m** between human interactions — handling multiple batches of planning, implementation, review, and verdict without intervention.

### Combined judge+merge eliminates redundancy

The original design had a 8-node pipeline for plan validation: merge two plans → fan out to two validators → judge the validations → round-robin refinement if rejected. In practice, a single LLM node can evaluate two plans, validate them, and merge them in one structured output call. The feedback loop (plan rejected → re-plan) works directly from the judge's `issues[]` back to the planners.

This pattern generalizes: when a pipeline has merge → validate → judge steps on the same data, combine them into one node. The LLM handles all three roles in a single pass. Separate nodes add latency and edge complexity for no quality gain.

### Simplify pass for continuous quality

After each implementation, a `simplify` agent inherits the implementation session and performs code quality cleanup: dead code removal, deduplication, idiomatic Go patterns. This adds ~5 min per batch but prevents technical debt from accumulating across iterations.

Without simplification, each batch adds code that passes review (zero blockers) but carries 3-7 suggestions. Over 10+ batches, these accumulate into significant maintainability issues. The simplify pass addresses them in-flight rather than as post-hoc cleanup.

### Commit via session fork

The commit naming agent forks from the simplified session (not the raw implementation). It has full context of what was changed and cleaned up, and can generate an accurate conventional commit message without needing explicit input fields (summary, files_changed, etc.).

## Loop design

### Why two nested loops

A single loop from verdict back to planning (the original design) re-plans from scratch even for trivial bugs. This wastes 20+ minutes of planning for a one-line fix.

The two-tier structure separates concerns:
- **fix_loop(5)**: quick corrections — fix → commit → test → review → verdict
- **outer_loop(50)**: batch progression — verdict → plan → implement → simplify → review

The fix loop handles "the code is almost right, fix these specific bugs." The outer loop handles "this batch is done, plan the next set of features."

### Edge evaluation order matters

Edges from `review_verdict` are evaluated in declaration order. The first matching edge wins:

1. `→ done when overall_parity` — all features ported (terminal)
2. `→ plan_fanout when batch_complete` — batch done, plan next
3. `→ fix when not batch_complete as fix_loop(5)` — bugs to fix
4. `→ plan_fanout when not overall_parity as outer_loop(50)` — fallback re-plan
5. `→ done` — terminal fallback (all loops exhausted)

Edge 4 catches the case where fix_loop is exhausted but the batch still has issues. Edge 5 ensures the workflow always terminates cleanly when all loops are exhausted.

**General rule**: any node with conditional edges AND loop-bounded edges needs a terminal fallback to prevent deadlock on loop exhaustion.

### Loop exhaustion as fallback trigger

The Iterion runtime skips edges whose loop counter is exhausted rather than treating exhaustion as a fatal error. This enables graceful degradation: when the fix loop is exhausted, the runtime automatically selects the next matching edge (the outer loop) instead of crashing.

This is a fundamental pattern for nested loops in any workflow engine: exhaustion should be a routing signal, not an error.

## Model allocation strategy

Not all nodes need the same model. The workflow uses three tiers:

| Tier | Model | Used for |
|------|-------|----------|
| **Hot path** | Claude Opus 4.6 via claude_code | Implementation, review, fix, analysis, planning, commit |
| **Independent judge** | Codex | Plan consolidation, second verdict judge |
| **Lightweight gate** | GPT-5.4-mini via goai | Human gate decision (auto-approve vs pause) |

The hot path uses a single model and backend to preserve session continuity (KV cache). The independent judge uses a different model to avoid correlated blind spots. The gate uses a cheap, fast model because the decision is simple (approve/pause based on risk assessment).

## Adapting to other use cases

### Different language pairs

Replace repo paths and update `port_plan_system` with the relevant translation patterns (e.g., Python decorators → Go middleware, JS async/await → Go goroutines). The workflow structure stays identical.

### Non-porting tasks (implement + review)

Remove the analysis phase. Keep: plan → human gate → implement → review → verdict → fix loop. This pattern works for any task where an AI implements something and needs iterative quality review: feature development, refactoring, test writing, documentation generation.

### Stricter quality (security, compliance)

Set `acceptance_threshold: "strict"`. Add specialized review nodes (security review, performance review) in parallel with the code review. The dual-judge pattern scales to N judges — the synthesis prompt merges N verdicts.

### Faster iteration (prototyping)

Set `acceptance_threshold: "lenient"` and reduce `fix_loop(5)` to `fix_loop(2)`. Prioritize functional correctness over polish.

### Adding more reviewers

Fan out to multiple specialized reviewers (code, architecture, security) in parallel. Each produces blockers/suggestions. The synthesis node merges and deduplicates across all reviewers.

## Visual organization

Group annotations collapse the 19 nodes into 6 high-level boxes in the Iterion visual editor:

```
@group analysis: analyze, merge_analysis
@group planning: plan_fanout, claude_plan, codex_consolidate, plan_judge_merge, plan_gate
@group implementation: implement, simplify
@group checkpoint: commit_namer, commit_changes, run_go_tests, parity_scan
@group review: review_fanout, claude_review, codex_review_judge, review_verdict
@group fix: fix
```

Double-click any group to expand and see individual nodes.

## Empirical data

From a live run on an ~80-feature Rust codebase (73k lines) with ~40% initial Go parity:

### Per-phase timing

| Phase | Duration | Notes |
|-------|----------|-------|
| Analysis | ~8 min | Single agent reads both codebases |
| Planning (parallel) | ~8 min | Two agents plan + consolidate |
| Judge+Merge | ~3 min | Single evaluation pass |
| Human Gate | <1s or pause | LLM decides; pauses for complex batches |
| Implementation | ~15-20 min | Heaviest phase; writes real code |
| Simplify | ~5 min | Code quality cleanup, dead code removal |
| Commit + Test | ~2 min | Fork for naming, build + vet |
| Parity Scan | <1s | Deterministic Go/Rust line count comparison |
| Review (fork) | ~8-10 min | Reads both codebases via tools |
| Dual Verdict | ~5 min | Two judges in parallel |
| Fix iteration | ~8-12 min | Full cycle: fix + commit + test + review + verdict |

### Run-level stats

| Metric | Value |
|--------|-------|
| Total outer loop iterations | 10 batches |
| Total verdicts | 17 |
| Fix iterations (batch 1) | 5 |
| Resumes from failure | 6 (Codex crashes, model spec errors) |
| Longest autonomous stretch | 2h25m |
| Go code generated | 19,162 lines source + 12,126 lines tests |
| New Go packages created | 11 (hooks, plugin, apikit, worker, lane, policy, recovery...) |
| Commits in target repo | 20 |

First batch without fix loops: ~55 min. Each fix iteration: ~10 min. Full batch with 5 fix iterations: ~105 min. Resume from failure saves the full upstream cost (analysis + planning = ~20 min minimum).
