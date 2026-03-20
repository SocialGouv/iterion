# E2E Test Scenarios

Suite end-to-end validant que les workflows phares passent de bout en bout
via le pipeline complet : parse `.iter` → compile IR → runtime engine → store.

## Pipeline

Chaque test charge un fichier `.iter` depuis `examples/`, le compile en IR,
injecte un `scenarioExecutor` (stub configurable par node) et execute via
`runtime.Engine`. Les assertions portent sur :

- **Statut final** du run (finished / failed / paused)
- **Artefacts** persistes (publish) et leur versionnement en boucle
- **Events** emis (sequence, coherence, completude)
- **Verdicts** des judges (approved / green)
- **Boucles** locales (refine_loop, fix_loop) et globales (full_recipe_loop)
- **Metriques** via `benchmark.CollectMetrics`

## Scenarios couverts

### 1. pr_refine_single_model

| Test | Chemin | Primitives verifiees |
|------|--------|---------------------|
| `TestSingleModel_HappyPath` | context → review → plan → compliance(OK) → act → verify(OK) → done | Sequential, publish, artefacts, metriques |
| `TestSingleModel_RefineLoop` | ... → compliance(KO) → refine → compliance_after(OK) → act → ... | Boucle locale `refine_loop(4)`, edge loop events |
| `TestSingleModel_GlobalReloop` | ... → verify(KO) → context (2e pass) → ... → verify(OK) → done | Reloop global `full_recipe_loop(3)`, artifact versioning |

### 2. pr_refine_dual_model_parallel

| Test | Chemin | Primitives verifiees |
|------|--------|---------------------|
| `TestDualParallel_HappyPath` | context → [claude_review \| gpt_review] → [plans] → join → [synth] → join → merge → act → [final_reviews] → join → verdict(OK) → done | Fan-out, join wait_all, branch events, multi-model parallelisme |
| `TestDualParallel_GlobalReloop` | ... → verdict(KO) → context (2e pass) → ... → verdict(OK) → done | Reloop global avec branches paralleles |

### 3. pr_refine_dual_model_parallel_compliance

| Test | Chemin | Primitives verifiees |
|------|--------|---------------------|
| `TestCompliance_HappyPath_NoHumanGate` | ... → compliance_initial(OK) → tech_gate(no human) → act → ... → done | Judge conditionnel, pas de pause humaine |
| `TestCompliance_HumanGate` | ... → tech_gate(needs human) → PAUSE → resume(answers) → integrate → compliance_post(OK) → act → ... → done | Human pause/resume, checkpoint, interaction, artifact human_decisions |
| `TestCompliance_RefineLoop` | ... → compliance_initial(KO) → refine_claude → compliance_after_claude(OK) → act → ... → done | Boucle `plan_refine_loop(6)`, alternance Claude/GPT |

### 4. ci_fix_until_green

| Test | Chemin | Primitives verifiees |
|------|--------|---------------------|
| `TestCIFix_HappyPath` | diagnose → plan → act → run_ci → verify(green) → done | Tool node, publish, metriques, verdict |
| `TestCIFix_FixLoop` | ... → verify(KO) → diagnose (2e pass) → ... → verify(OK) → done | Boucle `fix_loop(5)`, artifact versioning |
| `TestCIFix_LoopExhaustion` | ... → verify(KO) x5 → FAIL | Exhaustion de boucle, run_failed event |

### 5. Cross-cutting

| Test | Verification |
|------|-------------|
| `TestAllFixturesCompile` | Les 5 fixtures compilent sans erreur (parse + IR) |
| `TestEventSequenceCoherence` | Regles d'events : run_started en premier, run_finished/failed en dernier, node_started/finished apparies, seq monotone |

## Couverture des primitives

| Primitive | Tests |
|-----------|-------|
| agent | Tous |
| judge | Tous |
| router fan_out_all | DualParallel, Compliance |
| join wait_all | DualParallel, Compliance |
| human pause/resume | Compliance_HumanGate |
| tool node | CIFix |
| done | Tous (happy paths) |
| fail | CIFix_LoopExhaustion |
| boucle locale | SingleModel_RefineLoop, Compliance_RefineLoop, CIFix_FixLoop |
| reloop global | SingleModel_GlobalReloop, DualParallel_GlobalReloop |
| publish / artifacts | Tous |
| artifact versioning | SingleModel_GlobalReloop, CIFix_FixLoop |
| budget / metriques | SingleModel_HappyPath, DualParallel_HappyPath, CIFix_HappyPath |
| session modes | Valides a la compilation (fresh, inherit, artifacts_only) |
