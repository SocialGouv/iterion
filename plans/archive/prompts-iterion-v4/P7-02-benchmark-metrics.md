# P7-02 — Construire le benchmark multi-recettes et les métriques comparables

Dépendances :
- `P7-01`
- `P4-02`
- `P6-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P7-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Comparer plusieurs recettes sur un même cas avec isolation de workspace et métriques persistées.

Travail demandé :
- implémenter un runner de benchmark multi-recettes ;
- lancer chaque run sur un workspace isolé ;
- collecter et persister les métriques minimales : coût, durée, appels modèle, itérations, retries, verdict final ;
- rendre les résultats relisibles localement ;
- ajouter des tests d’absence d’interférence entre runs.

Livrables :
- runner de benchmark ;
- store de métriques ;
- tests de comparaison et d’isolation.

Critères d’acceptation :
- au moins deux recettes peuvent être lancées sur la même PR sans interférence ;
- le benchmark permet réellement de comparer coût, latence et efficacité des recettes.
```
