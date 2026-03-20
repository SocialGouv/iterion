# P7-01 — Implémenter `RecipeSpec` et le chargement des presets

Dépendances :
- `P2-01`
- `P3-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P7-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Faire exister la notion de recette comme première classe au-dessus du workflow brut.

Travail demandé :
- implémenter `RecipeSpec` ;
- charger `WorkflowRef`, `PresetVars`, `PromptPack`, `Budget`, `EvaluationPolicy` ;
- permettre au runtime de lancer un workflow nu ou une recette paramétrée ;
- ajouter les tests de chargement et d’exécution des recipes.

Livrables :
- modèle `RecipeSpec` ;
- résolution des presets et prompt packs ;
- tests de support recette.

Critères d’acceptation :
- la recette encapsule bien variables, prompts, budgets et stratégie d’évaluation ;
- une recette devient plus qu’un alias de workflow : c’est une unité exécutable et comparable.
```
