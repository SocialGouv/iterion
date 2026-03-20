# P3-01 — Construire le `RunStore` file-backed et le modèle d’événements

Dépendances :
- `P2-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P3-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Poser la persistance minimale d’un run, de ses artefacts et de ses événements.

Travail demandé :
- implémenter le layout `runs/<run_id>/...` décrit dans le plan ;
- exposer les opérations de base du `RunStore` ;
- sérialiser runs, events, artifacts et interactions humaines ;
- ajouter des tests de persistance et rechargement.

Livrables :
- store local file-backed ;
- API `RunStore` ;
- suite de tests de persistance.

Critères d’acceptation :
- un run, ses événements et ses artefacts sont lisibles et rejouables localement ;
- les événements minimaux listés dans le plan sont persistables sans perte de données.
```
