# Iterion V1 Reference Fixtures

This document describes the role of each reference fixture, their relationships, and the V1 primitives they exercise.

## Overview

| Fixture | Purpose | Models | Parallelism | Human | Loops |
|---|---|---|---|---|---|
| `pr_refine_single_model` | Single-model baseline | 1 | no | no | refine(4) + recipe(3) |
| `pr_refine_dual_model_parallel` | Dual-model without compliance gate | 2 | yes (fan-out) | no | recipe(3) |
| `pr_refine_dual_model_parallel_compliance` | **V1 flagship workflow** | 2+ | yes (fan-out) | yes | refine(6) + recipe(3) |
| `recipe_benchmark` | Recipe comparison | N | yes (fan-out) | no | none |
| `ci_fix_until_green` | Iterative CI fix | 1 | no | no | fix(5) |
| `pr_refine_dual_model_parallel_delegate` | Delegation variant (claude-code + codex) | 2 | yes (fan-out) | no | recipe(3) |

## Fixture Details

### `pr_refine_single_model`

**Nominal path:** context → review → plan → compliance → act → verify → done/reloop.

A single model traverses the entire workflow. Serves as a **cost and quality baseline** for comparison with multi-model variants. Exercises fundamental primitives: agent, judge, bounded loop, publish, session fresh and inherit.

### `pr_refine_dual_model_parallel`

**Nominal path:** context → [claude_review | gpt_review] → [claude_plan | gpt_plan] → join → [claude_synthesis | gpt_synthesis] → join → merge → act → [claude_final | gpt_final] → join → verdict → done/reloop.

Lightweight variant of the flagship workflow. Two models in parallel, cross-synthesis, merge, act, parallel final review. **No intermediate compliance gate or human gate.** Exercises: router fan_out_all, join wait_all, multi-models.

### `pr_refine_dual_model_parallel_compliance`

**V1 flagship workflow.** Nominal path identical to `pr_refine_dual_model_parallel` but with:
- Compliance judge after plan merge
- Optional human gate for technical arbitration
- Alternating Claude/GPT refinement loop (max 6 iterations)
- Compliance recheck after integrating human clarifications

Exercises **all V1 primitives**: agent, judge, router, join, human, done, fail, local loop, global reloop, publish, session fresh/inherit/artifacts_only, multi-models, tools, budgets.

### `recipe_benchmark`

**Nominal path:** orchestrator → [recipe_a | recipe_b] → join → judge → done.

Executes two recipes in parallel on the same PR, aggregates results, compares via a judge. Used to **compare cost, quality, iterations and latency** between recipes. Extensible to N recipes.

### `pr_refine_dual_model_parallel_delegate`

**Nominal path:** identical to `pr_refine_dual_model_parallel`.

Delegation variant of the parallel dual-model workflow. Instead of calling LLM APIs directly (`model:`), each node delegates its work to an external CLI agent (`delegate:`). Claude nodes use `claude_code` (claude-code CLI via SDK), GPT nodes use `codex` (OpenAI Codex CLI via SDK). The graph, schemas, prompts and edges are identical to the API version. Exercises the `delegate` primitive in addition to router, join, publish, loop.

### `ci_fix_until_green`

**Nominal path:** diagnose → plan → act → run_ci → verify → done or reloop.

Iterative CI fix pattern. Diagnoses the failure, plans a fix, applies it, reruns CI, verifies. Reloops until CI is green (max 5 iterations). Exercises the `tool` node (direct command execution without LLM) and workflow-wide loops.

## Relationships Between Fixtures

```
pr_refine_single_model          ← simple baseline, 1 model
    ↓ (add parallelism)
pr_refine_dual_model_parallel   ← dual-model, no gate
    ↓ (delegation via SDK)
pr_refine_dual_model_parallel_delegate   ← delegation variant (claude-code + codex)
    ↓ (add compliance + human)
pr_refine_dual_model_parallel_compliance  ← complete flagship workflow
    ↓ (benchmark)
recipe_benchmark                ← compare variants

ci_fix_until_green              ← independent pattern (CI, not PR)
```

## Usage

Each fixture is designed to be usable in three contexts:
1. **Tests** — parseable and compilable to IR, serves as test cases for parser (P1), compiler (P2) and runtime (P3+).
2. **Product documentation** — readable as a specification, with inline comments describing the nominal path.
3. **Mermaid rendering** — compilable to a workflow diagram for visualization.
