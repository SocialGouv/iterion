# P0-02 — Figer les workflows et fixtures de référence

Dépendances :
- `P0-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P0-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Transformer les cas d’usage phares en fixtures de référence maintenues et cohérentes avec le contrat V1.

Travail demandé :
- aligner `examples/pr_refine_dual_model_parallel_compliance.iter` sur le contrat V1 ;
- définir les autres workflows de référence avec leurs artefacts, boucles et conditions ;
- rendre chaque fixture exploitable pour tests, compilation et Mermaid ;
- créer les fichiers ou squelettes manquants si nécessaire.

Livrables :
- fixture de référence mise à jour ;
- squelettes ou spécifications concrètes pour `pr_refine_single_model`, `pr_refine_dual_model_parallel`, `recipe_benchmark` et `ci_fix_until_green` ;
- courte note décrivant le rôle de chaque fixture.

Critères d’acceptation :
- chaque workflow phare a un nom canonique, un objectif, des inputs, des outputs et un chemin nominal ;
- la fixture de référence peut servir à la fois de base de test, de doc produit et de rendu Mermaid.
```
