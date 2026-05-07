[← Documentation index](README.md) · [← Iterion](../README.md)

# Examples

The [`examples/`](../examples/) directory contains workflows of increasing complexity. Start simple and work your way up:

## 🟢 Starter

| File | Description |
|------|-------------|
| [`skill/minimal_linear.iter`](../examples/skill/minimal_linear.iter) | 28 lines — single agent with conditional pass/fail |
| [`skill/human_gate.iter`](../examples/skill/human_gate.iter) | Human approval gate pattern |
| [`skill/loop_with_judge.iter`](../examples/skill/loop_with_judge.iter) | Simple bounded loop with judge evaluation |
| [`skill/parallel_fan_out_join.iter`](../examples/skill/parallel_fan_out_join.iter) | Basic fan-out/join parallelism |

## 🟡 Intermediate

| File | Description |
|------|-------------|
| [`pr_refine_single_model.iter`](../examples/pr_refine_single_model.iter) | PR refinement: review → plan → compliance → act → verify loop |
| [`ci_fix_until_green.iter`](../examples/ci_fix_until_green.iter) | Automated CI fix loop: diagnose → plan → fix → rerun tests |
| [`session_review_fix.iter`](../examples/session_review_fix.iter) | Session continuity with `inherit` and `fork` modes |
| [`llm_router_task_dispatch.iter`](../examples/llm_router_task_dispatch.iter) | LLM-driven routing decisions |

## 🔴 Advanced

| File | Description |
|------|-------------|
| [`pr_review.iter`](../examples/pr_review.iter) | Parallel dual-reviewer PR analysis with judge synthesis |
| [`pr_refine_dual_model_parallel.iter`](../examples/pr_refine_dual_model_parallel.iter) | Dual-model parallel review with router/join |
| [`dual_model_plan_implement_review.iter`](../examples/dual_model_plan_implement_review.iter) | Enterprise dual-LLM orchestration with round-robin routing and delegation |
| [`recipe_benchmark.iter`](../examples/recipe_benchmark.iter) | Model/prompt benchmarking with recipe presets |

See [`examples/FIXTURES.md`](../examples/FIXTURES.md) for detailed documentation on each example.
