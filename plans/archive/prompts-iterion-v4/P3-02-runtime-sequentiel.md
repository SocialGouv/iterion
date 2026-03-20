# P3-02 — Implémenter le runtime séquentiel minimal

Dépendances :
- `P3-01`
- `P2-02`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P3-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Exécuter un workflow linéaire avec transitions, outputs, boucles et nœuds terminaux.

Travail demandé :
- implémenter le moteur séquentiel ;
- exécuter les nœuds `agent`, `judge`, `router`, `tool`, `done`, `fail` dans le cadre minimal nécessaire ;
- persister outputs, artefacts et compteurs de boucle ;
- émettre les événements clés ;
- couvrir un chemin linéaire et une boucle bornée par des tests.

Livrables :
- runtime séquentiel ;
- tests end-to-end minimaux ;
- persistance correcte des outputs et événements.

Critères d’acceptation :
- un run simple compile, s’exécute et laisse une trace exploitable ;
- les événements `node_started`, `edge_selected`, `node_finished` et `run_finished` sont correctement émis.
```
