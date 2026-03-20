# E2E Test Scenarios

End-to-end suite validating that flagship workflows pass from start to finish
through the full pipeline: parse `.iter` → compile IR → runtime engine → store.

## Pipeline

Each test loads an `.iter` file from `examples/`, compiles it to IR,
injects a `scenarioExecutor` (stub configurable per node) and executes via
`runtime.Engine`. Assertions cover:

- **Final status** of the run (finished / failed / paused)
- **Artifacts** persisted (publish) and their versioning in loops
- **Events** emitted (sequence, coherence, completeness)
- **Verdicts** from judges (approved / green)
- **Loops** local (refine_loop, fix_loop) and global (full_recipe_loop)
- **Metrics** via `benchmark.CollectMetrics`

## Covered Scenarios

### 1. pr_refine_single_model

| Test | Path | Verified Primitives |
|------|------|---------------------|
| `TestSingleModel_HappyPath` | context → review → plan → compliance(OK) → act → verify(OK) → done | Sequential, publish, artifacts, metrics |
| `TestSingleModel_RefineLoop` | ... → compliance(KO) → refine → compliance_after(OK) → act → ... | Local loop `refine_loop(4)`, edge loop events |
| `TestSingleModel_GlobalReloop` | ... → verify(KO) → context (2nd pass) → ... → verify(OK) → done | Global reloop `full_recipe_loop(3)`, artifact versioning |

### 2. pr_refine_dual_model_parallel

| Test | Path | Verified Primitives |
|------|------|---------------------|
| `TestDualParallel_HappyPath` | context → [claude_review \| gpt_review] → [plans] → join → [synth] → join → merge → act → [final_reviews] → join → verdict(OK) → done | Fan-out, join wait_all, branch events, multi-model parallelism |
| `TestDualParallel_GlobalReloop` | ... → verdict(KO) → context (2nd pass) → ... → verdict(OK) → done | Global reloop with parallel branches |

### 3. pr_refine_dual_model_parallel_compliance

| Test | Path | Verified Primitives |
|------|------|---------------------|
| `TestCompliance_HappyPath_NoHumanGate` | ... → compliance_initial(OK) → tech_gate(no human) → act → ... → done | Conditional judge, no human pause |
| `TestCompliance_HumanGate` | ... → tech_gate(needs human) → PAUSE → resume(answers) → integrate → compliance_post(OK) → act → ... → done | Human pause/resume, checkpoint, interaction, artifact human_decisions |
| `TestCompliance_RefineLoop` | ... → compliance_initial(KO) → refine_claude → compliance_after_claude(OK) → act → ... → done | Loop `plan_refine_loop(6)`, Claude/GPT alternation |

### 4. ci_fix_until_green

| Test | Path | Verified Primitives |
|------|------|---------------------|
| `TestCIFix_HappyPath` | diagnose → plan → act → run_ci → verify(green) → done | Tool node, publish, metrics, verdict |
| `TestCIFix_FixLoop` | ... → verify(KO) → diagnose (2nd pass) → ... → verify(OK) → done | Loop `fix_loop(5)`, artifact versioning |
| `TestCIFix_LoopExhaustion` | ... → verify(KO) x5 → FAIL | Loop exhaustion, run_failed event |

### 5. Cross-cutting

| Test | Verification |
|------|-------------|
| `TestAllFixturesCompile` | All 5 fixtures compile without errors (parse + IR) |
| `TestEventSequenceCoherence` | Event rules: run_started first, run_finished/failed last, node_started/finished paired, seq monotonic |

## Primitive Coverage

| Primitive | Tests |
|-----------|-------|
| agent | All |
| judge | All |
| router fan_out_all | DualParallel, Compliance |
| join wait_all | DualParallel, Compliance |
| human pause/resume | Compliance_HumanGate |
| tool node | CIFix |
| done | All (happy paths) |
| fail | CIFix_LoopExhaustion |
| local loop | SingleModel_RefineLoop, Compliance_RefineLoop, CIFix_FixLoop |
| global reloop | SingleModel_GlobalReloop, DualParallel_GlobalReloop |
| publish / artifacts | All |
| artifact versioning | SingleModel_GlobalReloop, CIFix_FixLoop |
| budget / metrics | SingleModel_HappyPath, DualParallel_HappyPath, CIFix_HappyPath |
| session modes | Validated at compilation (fresh, inherit, artifacts_only) |
