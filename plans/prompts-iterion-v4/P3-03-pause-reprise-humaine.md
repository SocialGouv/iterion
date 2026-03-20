# P3-03 — Ajouter la pause/reprise humaine native

Dépendances :
- `P3-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P3-03` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Supporter un nœud `human` qui suspend le run puis le relance sans rejouer l’amont.

Travail demandé :
- implémenter le nœud `human` et le statut de suspension explicite ;
- persister les questions, réponses et métadonnées d’interaction ;
- permettre la reprise du run sans réexécuter les étapes déjà validées ;
- rendre les réponses humaines exploitables comme artefacts ou inputs structurés ;
- ajouter les tests sur pause, answers et resume.

Livrables :
- reprise humaine fonctionnelle ;
- modèle `PendingInteraction` ;
- tests dédiés.

Critères d’acceptation :
- le run entre dans un statut suspendu explicite ;
- la reprise repart exactement du point prévu par le design retenu ;
- les checkpoints humains sont un mécanisme natif du runtime.
```
