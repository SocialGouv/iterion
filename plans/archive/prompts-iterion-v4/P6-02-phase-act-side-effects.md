# P6-02 — Implémenter la phase `act` et la policy de side effects

Dépendances :
- `P6-01`
- `P4-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P6-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Autoriser les mutations de workspace de façon contrôlée, auditable et testable.

Travail demandé :
- implémenter la phase `act` avec lecture, patch, commandes et collecte de diff/tests ;
- faire respecter une allowlist de commandes ;
- produire les artefacts `command_results`, `test_results`, `applied_patch` ou `git_diff_after_act` ;
- rendre explicite le refus d’une commande non allowlistée ;
- ajouter les tests sur acceptation et refus de side effects.

Livrables :
- phase `act` fonctionnelle ;
- policy tools appliquée ;
- artefacts d’action persistés ;
- tests associés.

Critères d’acceptation :
- une recette peut modifier le workspace sans perdre traçabilité ni sécurité ;
- tous les effets de bord utiles sont auditables.
```
