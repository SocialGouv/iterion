# P8-02 — Générer Mermaid à partir de l’IR

Dépendances :
- `P2-01`
- `P0-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P8-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Produire une vue Mermaid fidèle au graphe exécutable, sans en faire la source de vérité.

Travail demandé :
- mapper les nœuds et edges de l’IR vers Mermaid ;
- proposer au moins une vue compacte et une vue détaillée ;
- rendre explicitement branches, joins, boucles, `human`, `done` et `fail` ;
- ajouter des tests ou golden outputs Mermaid ;
- brancher la commande `diagram` si nécessaire.

Livrables :
- générateur Mermaid ;
- golden outputs ou tests dédiés ;
- support des vues utiles.

Critères d’acceptation :
- le workflow de référence est lisible en Mermaid sans perte d’information critique ;
- les points de pause/reprise et les joins apparaissent explicitement ;
- Mermaid reste une vue dérivée fidèle, pas une sémantique parallèle.
```
