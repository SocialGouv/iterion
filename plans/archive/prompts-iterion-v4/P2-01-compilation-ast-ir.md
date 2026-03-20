# P2-01 — Compiler l’AST vers une IR canonique

Dépendances :
- `P1-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P2-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Construire la source de vérité d’exécution indépendante du DSL auteur.

Travail demandé :
- créer une IR canonique orientée exécution ;
- implémenter le pipeline AST -> IR ;
- normaliser les références `vars`, `input`, `outputs`, `artifacts` ;
- représenter proprement nœuds, edges, joins, loops et hooks de recipes ;
- ajouter les tests de compilation de base.

Livrables :
- types IR ;
- compilateur AST -> IR ;
- tests de compilation initiaux.

Critères d’acceptation :
- le workflow de référence compile vers une IR complète et déterministe ;
- l’IR devient la seule source de vérité du runtime.
```
