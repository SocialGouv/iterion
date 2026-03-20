# P5-02 — Relier hooks, sorties structurées et politique de retry

Dépendances :
- `P5-01`
- `P3-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P5-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Rendre observable et robuste l’exécution `goai` côté iterion.

Travail demandé :
- relier `WithOnRequest`, `WithOnResponse`, `WithOnToolCall`, `WithOnStepFinish` au modèle d’événements iterion ;
- implémenter la policy de retry automatique sur 429, timeout et 5xx ;
- faire échouer explicitement un nœud sur sortie structurée invalide ;
- ajouter les tests sur retry et échec de schéma.

Livrables :
- événements `llm_request`, `llm_retry`, `llm_step_finished` ;
- gestion stricte des outputs structurés ;
- tests associés.

Critères d’acceptation :
- les hooks `goai` remontent correctement en events iterion ;
- aucun mécanisme caché de réparation de JSON invalide n’est introduit ;
- le comportement LLM est traçable et robuste.
```
