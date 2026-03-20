# Prompt prêt à l’emploi — plan détaillé pour un DSL de graphes d’agents LLM avec runtime Go et rendu Mermaid

## Rôle

Tu es un architecte logiciel senior, spécialisé en :

- conception de DSL et langages de configuration,
- moteurs d’orchestration et graphes d’exécution,
- systèmes multi-agents LLM,
- tooling Go,
- représentation intermédiaire canonique,
- génération de vues Mermaid à partir d’un graphe exécutable.

Tu dois produire un **plan détaillé, concret, structuré et pragmatique** pour concevoir un système complet répondant au besoin ci-dessous.

---

## Contexte produit

Je veux concevoir un système permettant de décrire et d’exécuter des **graphes de processus orientés agents LLM**, dans un style visuel textuel proche d’un diagramme à flèches.

L’objectif est d’avoir :

1. un **format auteur lisible par un humain**, visuel, simple à éditer à la main ;
2. une **compilation vers une représentation canonique interne** ;
3. un **runtime Go** pour exécuter les graphes ;
4. un **convertisseur vers Mermaid** pour obtenir automatiquement une vue visuelle du graphe.

Le système doit permettre des scénarios du type :

- mode `plan`,
- mode `act`,
- mode `plan+act`,
- aller-retour `planner <-> critic`,
- aller-retour `builder <-> reviewer`,
- boucle de raffinage `review for prod`,
- logique du type `is it ready? no -> reloop`,
- limitation configurable du nombre d’itérations,
- variables et prompts paramétrables,
- branchement de différents schémas d’aller-retour entre plusieurs agents.

Je ne veux **pas** un BPMN lourd ni un simple YAML à base de `depends_on` seulement. Je veux un format **textuel visuel avec flèches**, mais suffisamment strict pour être compilé et exécuté.

---

## Hypothèses de design à respecter

Le plan doit partir des hypothèses suivantes :

- le DSL auteur n’est **pas** la source d’exécution finale ;
- la source de vérité d’exécution doit être une **IR/AST canonique** ;
- Mermaid est une **vue dérivée**, pas la sémantique d’exécution ;
- le modèle d’exécution doit être pensé comme un **state graph avec cycles contrôlés**, pas juste comme un DAG ;
- les sorties des agents LLM doivent idéalement être **structurées par schéma** ;
- les boucles doivent être **bornées et nommées** ;
- les conditions de branchement doivent être explicites ;
- l’état partagé entre nœuds doit être clairement modélisé ;
- le runtime cible principal est **Go**.

---

## Ce que j’attends de la réponse

Je veux une réponse qui ressemble à un **document d’architecture / plan de conception**, pas à une simple liste d’idées.

Je veux un livrable avec :

1. une **vision d’ensemble du système**,
2. une **décomposition par composants**,
3. une **proposition de DSL V1**,
4. une **IR canonique**,
5. un **modèle d’exécution**,
6. une **stratégie de validation**,
7. une **stratégie de génération Mermaid**,
8. un **plan d’implémentation par phases**,
9. les **risques techniques / ambiguïtés à trancher**,
10. des **recommandations fermes**, avec arbitrages justifiés.

La réponse doit être **opinionated**, claire et concrète. Quand plusieurs options existent, il faut recommander une option principale et expliquer pourquoi.

---

## Format de sortie attendu

Structure ta réponse avec les sections suivantes, dans cet ordre exact.

# 1. Résumé exécutif

Donne une synthèse en 10 à 20 lignes couvrant :

- l’architecture recommandée,
- les choix structurants,
- pourquoi cette approche est la bonne,
- ce qu’il faut construire en premier.

# 2. Définition précise du problème

Explique précisément la nature du problème à résoudre.

Tu dois notamment distinguer :

- graphe statique vs graphe cyclique,
- orchestration de tâches vs orchestration d’agents LLM,
- DSL auteur vs IR canonique,
- exécution vs visualisation,
- contrôle de flux vs état métier / mémoire.

# 3. Principes d’architecture

Propose des principes d’architecture explicites, par exemple :

- séparation auteur / compilation / exécution / rendu,
- minimalisme du DSL,
- sémantique stricte,
- sorties structurées,
- boucles nommées et bornées,
- compatibilité future avec UI graphique,
- génération de vues multiples.

Pour chaque principe, explique :

- à quoi il sert,
- quel problème il évite,
- son impact sur l’implémentation.

# 4. Proposition de DSL V1

Propose un DSL textuel visuel avec flèches.

Le DSL doit permettre au minimum :

- déclaration de workflow,
- variables globales,
- prompts nommés,
- schémas de sortie,
- déclaration d’agents,
- arêtes avec `->`,
- conditions `when`,
- charge utile `with`,
- boucles nommées `as <loop_name>`,
- nœuds terminaux `done` et `fail`.

Je veux dans cette section :

- la philosophie du DSL,
- les primitives minimales,
- une proposition de syntaxe précise,
- les conventions de nommage,
- les contraintes syntaxiques,
- ce qui est volontairement exclu de la V1.

Ajoute ensuite **3 exemples complets** :

1. un exemple `plan`,
2. un exemple `plan+act`,
3. un exemple `review for prod` avec boucle de révision.

# 5. Sémantique d’exécution

Explique comment le runtime doit interpréter le DSL.

Couvre au minimum :

- unité d’exécution,
- état global du run,
- résolution des variables,
- cycle de vie d’un nœud,
- stockage des outputs,
- validation des sorties,
- évaluation des conditions,
- sélection des transitions,
- gestion des boucles,
- détection des terminaisons,
- gestion des erreurs.

Je veux que tu définisses explicitement :

- ce qu’est un run,
- ce qu’est un step,
- comment sont incrémentés les compteurs de boucle,
- comment est géré un nœud qui reçoit plusieurs entrées,
- si l’exécution est séquentielle ou potentiellement parallèle en V1,
- comment gérer un échec de schéma de sortie,
- comment modéliser `done` et `fail`.

# 6. IR / AST canonique

Propose une représentation canonique interne indépendante du DSL.

Je veux :

- une structure logique des objets principaux,
- les champs essentiels,
- les invariants,
- ce qui doit être résolu à la compilation,
- ce qui doit rester dynamique à l’exécution.

Donne ensuite un exemple d’IR canonique en **JSON** pour un workflow simple.

Puis propose une structure **Go** pour représenter cette IR, avec :

- types principaux,
- enums ou constantes recommandées,
- zones extensibles.

# 7. Modèle de données et état d’exécution

Décris précisément le modèle d’état.

Je veux une proposition couvrant :

- `inputs`,
- `vars`,
- `node outputs`,
- `loop counts`,
- `trace`,
- `artifacts`,
- `errors`,
- éventuellement `memory` ou `context snapshots`.

Explique quels champs doivent être :

- persistés,
- sérialisables,
- auditables,
- rejouables.

Ajoute une proposition de structure Go pour l’état de run.

# 8. Types de nœuds et responsabilités

Définis une taxonomie simple des nœuds pour la V1.

Par exemple, discute :

- `llm`,
- `tool`,
- `router`,
- `judge`,
- `done`,
- `fail`.

Pour chaque type, précise :

- son rôle,
- ses champs requis,
- ses entrées attendues,
- sa forme de sortie,
- ses contraintes.

Indique aussi ce que tu déconseilles de mettre en V1.

# 9. Prompts, templates et variables

Propose une stratégie propre pour gérer :

- prompts nommés,
- variables globales,
- interpolation,
- contexte injecté dans les agents,
- distinction entre prompt système, prompt utilisateur, contexte calculé,
- versionnage et réutilisation des prompts.

Je veux une recommandation claire sur :

- le mécanisme de templating,
- le moment où résoudre les variables,
- ce qu’il faut interdire pour éviter la complexité ou l’ambiguïté.

# 10. Schémas de sortie et validation

Explique comment gérer les sorties structurées.

Je veux que tu proposes :

- une stratégie de définition de schémas,
- le moment de la validation,
- la politique en cas d’échec de validation,
- les compromis entre souplesse et robustesse,
- les mécanismes de fallback éventuels.

Compare brièvement plusieurs approches possibles si utile, puis recommande une approche principale.

# 11. Patterns réutilisables de graphes

Propose une stratégie pour exprimer des patterns récurrents, par exemple :

- `refine_until_ready`,
- `plan_then_act`,
- `review_loop`,
- `critic_loop`,
- `retry_with_feedback`.

Explique si ces patterns doivent être :

- du sucre syntaxique du DSL,
- une bibliothèque compilée,
- une macro-expansion,
- ou juste des templates externes.

Donne une recommandation claire pour la V1.

# 12. Compilation DSL -> IR

Décris le pipeline de compilation.

Je veux :

- étapes du parsing,
- normalisation,
- résolution des références,
- vérification des nœuds,
- vérification des arêtes,
- validation des boucles,
- génération de l’IR finale.

Précise aussi quels diagnostics de compilation doivent être fournis à l’utilisateur.

# 13. Génération Mermaid

Explique comment traduire automatiquement l’IR en Mermaid.

Je veux une vraie stratégie de mapping couvrant :

- choix entre `flowchart TD`, `flowchart LR`, `stateDiagram-v2`,
- mapping des types de nœuds vers formes / classes / styles,
- représentation des boucles,
- représentation des conditions,
- traitement des `with { ... }`,
- stratégie pour éviter les diagrammes illisibles,
- gestion des vues `compact` vs `verbose`,
- génération possible de sous-graphes.

Ajoute au moins :

- un exemple de Mermaid généré,
- les limites de Mermaid par rapport à la sémantique réelle,
- les conventions de rendu recommandées.

# 14. Architecture du runtime Go

Propose une architecture technique Go concrète.

Je veux une décomposition en composants, par exemple :

- parser,
- compilateur,
- validateur,
- moteur d’exécution,
- registre des nœuds,
- adaptateur LLM,
- adaptateur tools,
- stockage de run state,
- générateur Mermaid,
- observabilité / traces.

Pour chaque composant, précise :

- rôle,
- API attendue,
- dépendances,
- degré de stabilité attendu.

Ajoute une proposition d’organisation de packages Go.

# 15. Plan d’implémentation par phases

Fais un vrai plan incrémental.

Je veux au moins les phases suivantes :

- Phase 0 : cadrage et décisions de design,
- Phase 1 : DSL minimal + parser,
- Phase 2 : IR + validateur,
- Phase 3 : runtime séquentiel minimal,
- Phase 4 : sorties structurées + validation,
- Phase 5 : boucles nommées + conditions,
- Phase 6 : rendu Mermaid,
- Phase 7 : patterns réutilisables,
- Phase 8 : observabilité / debug / replay,
- Phase 9 : durcissement produit.

Pour chaque phase, donne :

- objectif,
- périmètre,
- livrables,
- critères d’acceptation,
- risques,
- dépendances.

# 16. Arbitrages et décisions à trancher tôt

Liste les décisions structurantes à prendre dès le début.

Par exemple :

- DSL indentation-sensitive ou non,
- expressions de conditions maison ou embarquées,
- JSON Schema / CUE / autre pour validation,
- mode séquentiel vs parallèle en V1,
- gestion des multi-entrées,
- degré de liberté du templating,
- gestion des side effects outils,
- persistance des runs.

Pour chaque point, donne une recommandation nette.

# 17. Risques, pièges et anti-patterns

Je veux une section explicite sur les erreurs de conception probables.

Exemples attendus :

- DSL trop riche trop tôt,
- Mermaid utilisé comme source de vérité,
- absence de schémas structurés,
- conditions trop libres,
- couplage trop fort entre syntaxe auteur et runtime,
- mauvaise gestion des boucles,
- prompts non versionnés,
- absence de trace exploitable.

Pour chaque risque :

- explique le problème,
- donne son impact,
- propose une mitigation.

# 18. Recommandation finale ferme

Termine par une recommandation finale claire, sans ambiguïté, en répondant à ces questions :

- quelle architecture retenir,
- quelle forme de DSL retenir en V1,
- quel niveau d’ambition garder pour la première version,
- quel ordre de construction suivre,
- quels compromis accepter pour aller vite sans casser l’avenir.

---

## Contraintes de style pour la réponse

- Réponds en **français**.
- Sois **très structuré**, avec titres et sous-titres.
- Sois **concret et technique**, pas vague.
- Fais des **recommandations nettes**.
- Donne des **exemples de syntaxe**, d’IR JSON et de structures Go.
- N’essaie pas de couvrir 50 variantes ; choisis une trajectoire principale.
- Signale explicitement ce qui relève de la **V1** et ce qui doit être repoussé.
- Ne fais pas un cours général sur les LLM ; reste centré sur l’architecture du système.

---

## Niveau de détail attendu

Je veux une réponse suffisamment détaillée pour servir de base à :

- un document d’architecture,
- un ticketing de roadmap technique,
- un premier découpage d’implémentation,
- une discussion d’équipe sur les choix structurants.

La réponse doit être assez détaillée pour qu’un lead engineer puisse s’en servir pour lancer la conception.

---

## Bonus souhaités

Si pertinent, ajoute en annexe :

1. une **grammaire EBNF simplifiée** du DSL V1,
2. un **exemple de workflow complet** de bout en bout,
3. un **pseudo-code du moteur d’exécution**,
4. une **checklist de validation de workflow**,
5. une **stratégie de tests**.

---

## Instruction finale

Produis maintenant la réponse complète selon cette structure exacte, avec un niveau de précision élevé, des arbitrages clairs et une orientation fortement pragmatique.