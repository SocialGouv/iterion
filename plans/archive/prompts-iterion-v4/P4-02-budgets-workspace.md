# P4-02 — Faire respecter budgets partagés et sécurité de workspace

Dépendances :
- `P4-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P4-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Garantir un parallélisme sûr avec budget global first-come-first-served et contrainte de mutation de workspace.

Travail demandé :
- implémenter le budget global partagé ;
- émettre `budget_warning` quand un seuil est franchi ;
- faire échouer proprement avec `budget_exceeded` quand le budget est épuisé ;
- empêcher l’exécution parallèle de branches mutantes sur un même `WorkingDir` ;
- ajouter des tests dédiés sur budget et sécurité de workspace.

Livrables :
- contrôle budgétaire global ;
- garde-fous de mutation ;
- tests de non-régression.

Critères d’acceptation :
- une branche peut épuiser le budget global et faire échouer l’autre ;
- le runtime refuse les topologies dangereuses de mutation concurrente ;
- le parallélisme reste borné, déterministe et sûr.
```
