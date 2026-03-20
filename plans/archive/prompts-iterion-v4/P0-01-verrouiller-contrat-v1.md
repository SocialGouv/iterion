# P0-01 — Verrouiller le contrat V1

Dépendances :
- aucune

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P0-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Verrouiller le contrat V1 pour éliminer les ambiguïtés restantes sur les nœuds, sessions, artefacts, budgets, recipes et transitions.

Travail demandé :
- unifier la terminologie DSL, IR, runtime, store et recipes ;
- expliciter les invariants critiques et les non-objectifs V1 ;
- supprimer les ambiguïtés sur joins, sessions, artefacts persistants, budgets et reprise humaine ;
- modifier directement les fichiers nécessaires, pas seulement proposer un plan.

Livrables :
- document mis à jour ;
- liste courte des décisions prises ;
- éventuelle liste des non-objectifs V1.

Critères d’acceptation :
- les sections DSL, IR, runtime et persistance emploient les mêmes termes ;
- `join`, `human`, `artifact persistent`, `outputs.<node>.history` et `session: inherit` sont définis sans contradiction ;
- aucune ambiguïté bloquante restante pour lancer l’implémentation.
```
