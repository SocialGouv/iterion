# P9-01 — Assembler les recettes phares en end-to-end

Dépendances :
- `P4-02`
- `P5-02`
- `P6-02`
- `P7-02`
- `P8-01`
- `P8-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P9-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Vérifier que toutes les briques convergent sur les scénarios réellement importants.

Travail demandé :
- brancher les workflows de référence sur le runtime complet ;
- ajouter des scénarios E2E pour `pr_refine_single_model`, `pr_refine_dual_model_parallel`, `examples/pr_refine_dual_model_parallel_compliance.iter` et `ci_fix_until_green` ;
- vérifier cohérence des artefacts, boucles, verdicts et métriques ;
- documenter brièvement les scénarios couverts.

Livrables :
- suite E2E ;
- documentation courte des scénarios ;
- validation de cohérence des workflows phares.

Critères d’acceptation :
- les workflows phares passent de bout en bout avec traces, artefacts et verdicts cohérents ;
- les reloops globaux et locaux sont couverts ;
- les recettes phares deviennent la preuve que la plateforme tient ensemble.
```
