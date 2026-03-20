# P9-02 — Durcir annulation, timeouts, compatibilité et ergonomie d’erreur

Dépendances :
- `P9-01`

Prompt :
```text
Tu travailles dans le repo iterion, dans `/workspaces/iterion`.
Le lot à réaliser est `P9-02` du plan V4.
Source de vérité : `plans/plan-iterion-v4-platform-ready.md`.

Mission :
Finaliser la V1 avec les protections opératoires et la qualité d’usage minimale.

Travail demandé :
- ajouter cancel, timeouts et fin propre des runs ;
- stabiliser les formats persistés utiles pour store, events et artefacts ;
- améliorer les erreurs et diagnostics côté utilisateur ;
- ajouter des tests sur timeout, cancel et compatibilité.

Livrables :
- durcissement runtime/CLI ;
- tests dédiés ;
- documentation courte sur les formats persistés stabilisés.

Critères d’acceptation :
- les runs peuvent être interrompus proprement ;
- les formats persistés critiques sont documentés et suffisamment stables pour l’outillage futur ;
- la V1 est exploitable sans comportements implicites ni échecs opaques.
```
