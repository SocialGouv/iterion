# P6-01 — Construire le `ToolRegistry` et les adapters built-ins/MCP

Dépendances :
- `P5-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P6-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Normaliser les tools locaux et MCP sous une même abstraction.

Travail demandé :
- implémenter un `ToolRegistry` unique ;
- imposer la règle de namespacing `mcp.<server>.<tool>` ;
- gérer les collisions et la résolution de références ;
- faire en sorte que built-ins et MCP suivent le même contrat d’exécution ;
- ajouter des tests de résolution et collisions.

Livrables :
- registry tools ;
- adapters built-ins et MCP ;
- tests de résolution.

Critères d’acceptation :
- un tool déclaré dans un workflow peut être résolu sans ambiguïté ;
- les workflows peuvent référencer des tools sans dépendre de conventions implicites.
```
