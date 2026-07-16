---
name: rgaa-dsfr
description: Exploiter le Système de Design de l'État (DSFR) comme baseline d'accessibilité RGAA via les outils MCP DSFR. Lire cette skill quand le code cible utilise des classes `fr-*` ou réimplémente des composants (boutons, champs, modales, accordéons, navigation) — le DSFR fournit le balisage accessible de référence à comparer.
---

> Le DSFR (https://www.systeme-de-design.gouv.fr/) est le design system officiel
> de l'État français. Ses composants sont conçus RGAA-conformes : ils sont la
> **référence** à laquelle comparer le code audité.

# RGAA via le DSFR

Quand le repo cible utilise le DSFR (présence de classes `fr-*`, du package
`@gouvfr/dsfr`, ou d'un fork des composants), le balisage accessible attendu est
déjà spécifié par le design system. Au lieu d'inférer le bon ARIA, **récupérer la
référence officielle** et signaler tout écart.

## Détecter l'usage du DSFR

- Classes CSS `fr-*` dans le HTML/JSX (`fr-btn`, `fr-input-group`, `fr-accordion`…).
- Dépendance `@gouvfr/dsfr` (ou `@codegouvfr/react-dsfr`) dans `package.json`.
- Import de la feuille `dsfr.min.css` / des composants react-dsfr.

Si aucun de ces signaux n'est présent, cette skill ne s'applique pas : auditer
avec les skills `rgaa-criteria-*` génériques.

## Outils MCP DSFR disponibles

Ces outils ne sont disponibles que si le serveur MCP `dsfr` est câblé dans le run.
S'ils sont absents, **dégrader proprement** vers la revue statique générique (les
skills `rgaa-criteria-*`) — ne pas bloquer le run.

- `mcp__dsfr__list_components` — liste tous les composants/fondamentaux/modèles
  (nom, titre FR, description, sections documentées). Point d'entrée.
- `mcp__dsfr__search_components({ query })` — recherche par mot-clé
  (« tableau », « formulaire », « fr-btn »…).
- `mcp__dsfr__get_component_doc({ name, section })` — doc d'un composant ;
  `section` ∈ `overview|code|design|accessibility|demo` (défaut `code`).
- `mcp__dsfr__get_component_code({ name })` — snippets HTML prêts à l'emploi +
  liste dédupliquée des classes `fr-*`. **La référence de balisage.**
- `mcp__dsfr__get_component_accessibility({ name })` — interactions clavier,
  règles d'accessibilité (à faire / à ne pas faire), contrastes, restitution
  lecteur d'écran, **critères RGAA applicables**. À lire pour chaque composant audité.
- `mcp__dsfr__get_color_tokens({ context, usage, family })` — tokens de couleur
  par contexte (`background|text|artwork`) avec correspondances clair/sombre ;
  pour vérifier les contrastes 3.1/3.2/3.3.

## Méthode

1. Identifier le composant DSFR concerné (ex. un champ → `input`, une fenêtre →
   `modal`, un menu → `navigation`).
2. `get_component_accessibility({ name })` pour connaître les exigences RGAA
   officielles (clavier, ARIA, contraste) du composant.
3. `get_component_code({ name })` pour le balisage de référence + les classes
   `fr-*` attendues.
4. Comparer le code audité à la référence : tout attribut ARIA manquant, classe
   `fr-*` détournée, ou interaction clavier absente est une non-conformité —
   citer le critère RGAA renvoyé par l'outil.
5. Pour les couleurs custom hors palette DSFR, vérifier les ratios via
   `get_color_tokens` (le DSFR garantit déjà les seuils sur ses tokens).

## Garde-fous

- Le DSFR est un **accélérateur**, pas un blanc-seing : un composant `fr-*` mal
  recâblé (ex. `fr-btn` sur un `<div>` non focusable) reste non conforme. Toujours
  croiser avec les critères `rgaa-criteria-*`.
- Ne pas imposer le DSFR à un repo qui ne l'utilise pas : c'est une référence
  d'accessibilité, pas une migration de design system.

<!-- TODO(skill-dup): this RGAA skill is also shipped in bots/rgaa-audit/skills/rgaa-dsfr.md and bots/app-dev/skills/rgaa-dsfr.md — iterion has no skill-sharing primitive yet, keep the copies in sync. -->
