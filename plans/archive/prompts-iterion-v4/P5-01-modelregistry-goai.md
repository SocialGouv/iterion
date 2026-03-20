# P5-01 — Construire le `ModelRegistry` et l’adaptateur `goai`

Dépendances :
- `P2-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P5-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Brancher iterion sur les vraies interfaces modèle de `goai`.

Travail demandé :
- implémenter un `ModelRegistry` ;
- résoudre les modèles `provider/model-id` ;
- adapter les appels texte, structuré et tool loop ;
- mapper `WithSystem`, `WithMessages`, `WithTools`, `WithMaxSteps`, `WithTimeout`, `WithExplicitSchema` ;
- ajouter les tests d’intégration de base.

Livrables :
- registry modèles ;
- adaptateur `goai` ;
- tests initiaux d’intégration.

Critères d’acceptation :
- un nœud `agent` ou `judge` sait appeler un modèle texte ou structuré via `goai` ;
- les capacités modèle sont interrogées via le registry ;
- les nœuds LLM utilisent les interfaces réelles, pas des conventions ad hoc.
```
