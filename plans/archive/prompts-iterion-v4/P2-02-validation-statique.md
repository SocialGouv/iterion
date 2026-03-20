# P2-02 — Ajouter la validation statique et les diagnostics de compilation

Dépendances :
- `P2-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P2-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Rejeter à la compilation les workflows mal câblés, ambigus ou incompatibles avec la sémantique V1.

Travail demandé :
- vérifier joins, edges, loops, sessions, `with`, conditions booléennes et artefacts persistants ;
- produire des diagnostics de compilation précis ;
- couvrir les règles bloquantes du plan V4 par des tests dédiés ;
- rejeter explicitement `session: inherit` juste après un `join`.

Livrables :
- validateur IR ;
- diagnostics de compilation stables ;
- tests positifs et négatifs.

Critères d’acceptation :
- les ambiguïtés d’edges et de fallback sont détectées ;
- les références impossibles ou incohérentes sont signalées avant runtime ;
- les erreurs structurelles majeures ne passent plus au runtime.
```
