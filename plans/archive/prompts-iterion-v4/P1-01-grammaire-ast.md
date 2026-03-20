# P1-01 — Définir la grammaire DSL et l’AST

Dépendances :
- `P0-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P1-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Poser la base syntaxique V1 pour `entry`, `input`, `output`, `system`, `user`, `tools`, `tool_max_steps`, `session`, `timeout`, `budget`, `human`, `join`, `with`, `when`, `done`, `fail` et les boucles.

Travail demandé :
- définir une grammaire V1 stricte, compilable et minimale ;
- créer un AST clair couvrant toutes les primitives obligatoires ;
- documenter explicitement ce qui reste hors V1 ;
- modifier directement les fichiers nécessaires.

Livrables :
- grammaire V1 ;
- structures AST ;
- exemples minimaux valides.

Critères d’acceptation :
- aucune primitive obligatoire du plan n’est orpheline de représentation AST ;
- la compilation AST -> IR pourra se faire sans conventions implicites.
```
