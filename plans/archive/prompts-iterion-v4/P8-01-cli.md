# P8-01 — Construire la CLI opérable

Dépendances :
- `P3-03`
- `P7-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P8-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Offrir la surface minimale pour valider, lancer, inspecter et reprendre des runs.

Travail demandé :
- implémenter `iterion run`, `iterion validate`, `iterion inspect`, `iterion resume` ;
- supporter des sorties lisibles humainement et des sorties structurées ;
- brancher la reprise humaine via fichier de réponses ou équivalent non interactif ;
- ajouter des tests d’intégration CLI.

Livrables :
- CLI fonctionnelle ;
- tests d’intégration ;
- commandes reliées au runtime réel.

Critères d’acceptation :
- un utilisateur peut exécuter la majorité du cycle sans écrire de code ;
- la reprise humaine est faisable en mode non interactif ;
- workflows et recipes sont opérables sans API externe.
```
