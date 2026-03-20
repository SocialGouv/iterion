# P4-01 — Implémenter le scheduler de branches et les `join`

Dépendances :
- `P3-02`
- `P2-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P4-01` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Exécuter plusieurs branches sœurs, puis les regrouper selon une stratégie explicite.

Travail demandé :
- implémenter un scheduler borné pour les branches parallèles ;
- supporter `fan_out_all` ;
- implémenter `join wait_all` et `join best_effort` ;
- rendre visibles les métadonnées de branches échouées au niveau du `join` ;
- ajouter des tests de runs parallèles avec succès et échec partiel.

Livrables :
- scheduler de branches ;
- support des joins ;
- tests de parallélisme contrôlé.

Critères d’acceptation :
- le scénario de double review parallèle peut aller jusqu’au merge ;
- les joins agrègent les résultats de manière explicite et conforme à leur stratégie.
```
