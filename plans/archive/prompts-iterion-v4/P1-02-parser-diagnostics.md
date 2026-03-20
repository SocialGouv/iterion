# P1-02 — Implémenter le parser et les diagnostics de parse

Dépendances :
- `P1-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P1-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Rendre le DSL lisible par machine avec diagnostics précis, positionnés et testables.

Travail demandé :
- implémenter un parser déterministe et indent-sensitive pour la syntaxe V1 ;
- produire des diagnostics avec code, message, ligne et colonne ;
- ajouter des golden tests sur cas valides et invalides ;
- faire en sorte que les fixtures de référence se parsèment de manière stable.

Livrables :
- parser fonctionnel ;
- suite de tests de parse ;
- diagnostics stables.

Critères d’acceptation :
- les erreurs d’indentation, de mots-clés réservés et de structure sont détectées proprement ;
- les fixtures de référence parsées produisent un AST stable.
```
