# Contexte de reprise — Pipeline Boards

Tu reprends un chantier dans le dépôt SocialGouv/iterion.

## Références

- Dépôt local : `/home/victor/Workspace/iterion`
- Branche : `feat/pipeline-board-projection`
- Pull request : https://github.com/SocialGouv/iterion/pull/193
- Commit principal : `43a1f365` — `feat(studio): add bot-scoped pipeline boards`

## Contexte fonctionnel

L’issue https://github.com/SocialGouv/iterion/issues/125 proposait un board représentant l’exécution d’un pipeline et de ses enfants, avec des colonnes correspondant notamment aux interactions humaines et la possibilité de répondre directement depuis le board.

L’issue avait été fermée après la PR #152, mais celle-ci traitait surtout l’import depuis une forge auto-hébergée vers le backlog natif. Elle ne réalisait pas réellement la projection d’exécution proposée.

La décision retenue est additive :

- `/board` reste le backlog partagé, éditable et utilisé par le dispatcher ;
- un second produit est ajouté sous `/pipelines` ;
- chaque bot découvert possède un pipeline board déterministe ;
- ce board est une projection du runtime, pas un second stockage mutable ;
- les cartes ne sont donc pas déplaçables par drag-and-drop.

## Fonctionnalité implémentée

### Studio

- `/pipelines` liste les boards disponibles ;
- `/pipelines/:bot` affiche le board d’un bot ;
- entrée dédiée dans la navigation, les palettes de commandes et la page d’un bot ;
- formulaire d’ajout de tâche, avec démarrage immédiat optionnel ;
- affichage des runs racines et enfants ;
- réponse directe aux interactions humaines ;
- reprise des pauses opérateur ;
- liens vers la console du run exact.

### Colonnes dérivées

1. Todo
2. Running
3. Une colonne par interaction humaine déclarée dans le workflow
4. Des colonnes dynamiques pour les interactions de workflows enfants
5. Other input
6. Needs attention
7. Done

### API

- `GET /api/v1/pipeline-boards`
- `GET /api/v1/pipeline-boards/{bot}`
- `POST /api/v1/pipeline-boards/{bot}/tasks`

Le serveur construit un read model agrégé à partir :

- des issues natives explicitement associées au bot ;
- de leur historique de tentatives ;
- de leur run courant ;
- des runs manuels/API/scheduled associés au bot ;
- des descendants reliés récursivement par `ParentRunID`.

## Sémantique importante

- `Issue.LastRunID` reste le pointeur canonique vers la dernière tentative terminée.
- Un run plus récent encore en cours peut être associé grâce à son `Source.IssueID`.
- Les anciennes tentatives ne doivent pas réapparaître comme des runs autonomes.
- Un enfant reste affiché sur le board de sa racine, même s’il exécute lui-même un autre bot.
- Une pause inconnue sur un run racine va dans `Other input`.
- Une pause inconnue sur un enfant crée une colonne dynamique nommée.
- Une réponse humaine cible toujours le `run_id` et le `node_id` exacts.
- En cloud, le board est résolu depuis l’équipe active ; il ne doit jamais retomber sur un store local global.
- Les identifiants de colonnes encodent sans perte le couple workflow/node afin d’éviter les collisions.

## Filiation des runs

La PR propage aussi `ParentRunID` dans :

- les subbots CLI imbriqués, récursivement ;
- les launches locaux et détachés de `runview` ;
- les forks, tout en conservant `ForkedFrom` ;
- la création et la reprise au niveau du runtime.

## Performance et sécurité

- `ListRunRecordsCtx` évite de charger chaque run deux fois lors de la projection.
- Le contexte tenant est conservé pendant la lecture des runs.
- Le POST d’ajout de tâche vérifie l’origine.
- Le bot vient du chemin URL et ne peut pas être remplacé dans le body.
- La projection est limitée à 500 cartes et une profondeur de 20, avec avertissement de topologie en cas de troncature.

## Documentation

- ADR-073 documente le second pipeline board.
- ADR-073 affine et remplace partiellement les décisions D1–D2 de l’ADR-071.
- La documentation du native tracker décrit les routes et la séparation entre `/board` et `/pipelines`.
- OpenAPI et les types TypeScript générés sont à jour.

## Validation effectuée

- `DISPLAY= go test ./...` : succès
- `go vet ./...` : succès
- build Go : succès
- tests race ciblés locaux : succès
- typecheck, ESLint et build Studio : succès
- suite Studio complète sous Node 24 : 77 fichiers, 606 tests réussis
- CI GitHub entièrement verte, notamment :
  - test
  - race
  - mongo-conformance
  - cloud-e2e
  - vendor-check
  - govulncheck
  - fs-scan
  - helm-lint

## Limites connues

- Les anciens runs sans métadonnée de bot ni lien fiable vers une issue ne sont pas attribués par heuristique.
- La topologie est dérivée de la source actuelle du bot, pas d’un snapshot historique.
- Les anciennes filiations manquantes ne peuvent pas être reconstruites avec certitude.
- Cette première version réutilise les issues natives comme tâches ; elle n’introduit pas encore de store `PipelineInstance`.

## État du workspace

La branche est poussée et la PR est ouverte. Le fichier non suivi `iterion.bak` appartient à l’utilisateur : ne pas le modifier, le supprimer ni l’ajouter au commit.

Avant toute modification, commence par lire la PR #193, son diff et ses éventuels nouveaux commentaires. Ne réimplémente pas la fonctionnalité depuis zéro. Si une review demande des changements, conserve les invariants ci-dessus, ajoute les tests de non-régression correspondants, puis pousse les corrections sur la même branche.
