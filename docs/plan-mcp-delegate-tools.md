# Plan — Couche MCP générique pour les agents goai

## Résumé
- Remplacer le plan “delegate tools” centré Claude/Codex par une couche MCP générique : un agent goai doit pouvoir utiliser n’importe quel serveur MCP configuré, Claude Code et Codex n’étant que des presets optionnels.
- Garder `delegate:` comme chemin legacy subprocess, inchangé et documenté comme mode historique.
- Destination du plan final : `/workspaces/iterion/docs/plan-mcp-delegate-tools.md`.
- En Plan Mode, ce tour fige le contenu du plan ; l’écriture du fichier sera faite hors Plan Mode.

## Interfaces et comportement public
- Ajouter une déclaration top-level réutilisable :
```iter
mcp_server github:
  transport: http
  url: "https://api.githubcopilot.com/mcp"
```
- Ajouter un bloc `mcp:` au niveau `workflow` :
```iter
workflow my_flow:
  entry: implement
  mcp:
    autoload_project: true
    servers: [claude_code, codex, github]
    disable: [falcon]
```
- Ajouter un bloc `mcp:` au niveau `agent` et `judge` :
```iter
agent implement:
  model: "anthropic/claude-sonnet-4-6"  # optionnel
  mcp:
    inherit: true
    servers: [github]
    disable: [codex]
  tools: [mcp.github.create_issue]
```
- `mcp.<server>.<tool>` devient la seule forme publique des outils MCP ; ne pas introduire d’alias spéciaux `mcp.delegate.*`.
- Les presets natifs Iterion `claude_code` et `codex` sont disponibles par nom dans `mcp.servers`, même s’ils ne sont pas présents dans `.mcp.json`.
- Résolution du modèle goai pour `agent` et `judge` : `model:` explicite dans le `.iter`, sinon `ITERION_DEFAULT_SUPERVISOR_MODEL`, sinon erreur explicite.
- `tools:` reste l’allowlist effective d’un nœud : un serveur MCP peut être actif, mais ses outils ne sont appelables que s’ils sont listés dans `tools:`.

## Changements d’implémentation
- Introduire une couche MCP dédiée avec 4 responsabilités : chargement de config, catalogue de serveurs, client MCP par transport, et bridge vers `tool.Registry`.
- Charger les serveurs MCP depuis trois sources :
  - `.mcp.json` projet, activé par défaut
  - déclarations top-level `mcp_server`
  - presets natifs Iterion, dont `claude_code` et `codex`
- Ajouter une variable d’environnement `ITERION_MCP_AUTOLOAD`; si `0` ou `false`, ignorer l’auto-chargement de `.mcp.json`.
- Règle de composition des serveurs :
  - catalogue = project `.mcp.json` + `mcp_server` top-level + presets natifs
  - set workflow actif = tous les serveurs projet si `autoload_project` est vrai, puis `mcp.servers`, puis retrait de `mcp.disable`
  - set node actif = set workflow si `inherit` est vrai, puis ajout de `mcp.servers`, puis retrait de `mcp.disable`
- Règle de précédence des définitions :
  - une déclaration top-level `mcp_server <name>` override une entrée du `.mcp.json` de même nom
  - un preset natif n’est utilisé que si aucun serveur explicite du même nom n’existe
  - deux déclarations `.iter` du même nom sont une erreur de compilation
- Implémenter un client MCP générique avec découverte des outils par serveur et enregistrement sous `mcp.<server>.<tool>`.
- Support v1 des transports :
  - `stdio` obligatoire
  - `http` obligatoire
  - `sse` explicitement hors scope v1
- Ouvrir les connexions MCP à la demande au premier usage, puis mettre en cache client et liste d’outils pendant la durée du process.
- Étendre le parser, l’AST et l’IR pour supporter `mcp_server` top-level et les blocs `mcp:` workflow/node.
- Dans l’exécuteur goai, construire un registry d’outils effectif par nœud à partir des serveurs MCP actifs pour ce nœud, puis résoudre `tools:` normalement.
- Garder `executeDelegation` et `delegate.DefaultRegistry()` pour le legacy ; ne pas mélanger ce chemin avec la nouvelle couche MCP.
- Fournir un catalogue natif Iterion de presets MCP :
  - `claude_code` -> `claude mcp serve`
  - `codex` -> `codex mcp-server`
- Mettre à jour la doc et les exemples pour montrer :
  - auto-chargement générique via `.mcp.json`
  - ajout de presets natifs Iterion
  - activation/filtrage workflow
  - override local par nœud

## Tests et critères d’acceptation
- Parser et compilation :
  - accepte `mcp_server` top-level
  - accepte les blocs `mcp:` workflow/node
  - rejette doublons de noms et configs invalides par transport
- Résolution de config :
  - auto-chargement `.mcp.json` activé par défaut
  - `ITERION_MCP_AUTOLOAD=false` désactive bien le projet
  - une déclaration `.iter` override bien un serveur projet du même nom
  - les presets natifs `claude_code` et `codex` sont activables sans `.mcp.json`
- Runtime MCP :
  - découverte correcte des outils et enregistrement sous `mcp.<server>.<tool>`
  - cache client et lazy connect fonctionnent
  - les outils MCP d’un serveur désactivé ne sont pas résolus
  - les outils non listés dans `tools:` restent inaccessibles
- Modèle superviseur :
  - `model:` explicite reste prioritaire
  - fallback sur `ITERION_DEFAULT_SUPERVISOR_MODEL` fonctionne pour `agent` et `judge`
  - erreur ciblée si aucun modèle n’est disponible
- Non-régression :
  - le mode legacy `delegate:` continue à fonctionner sans changement
  - les exemples existants `delegate:` restent valides
- Live tests optionnels :
  - `claude mcp serve`
  - `codex mcp-server`
  - au moins un serveur MCP tiers générique en `stdio` ou `http`

## Hypothèses et choix par défaut
- Le but produit est une intégration MCP générique côté Iterion, pas une solution spéciale pour Claude/Codex.
- Les presets `claude_code` et `codex` restent fournis parce qu’ils sont utiles, officiels/populaires, et cohérents avec la vision “MCP natif Iterion”.
- Les outils MCP sont exposés via leurs noms natifs `mcp.<server>.<tool>` ; pas d’alias métier supplémentaires.
- Le fallback de modèle par env reste limité à `agent` et `judge` dans ce plan.
- La v1 supporte les serveurs MCP génériques sur `stdio` et `http`; `sse` est reporté.
