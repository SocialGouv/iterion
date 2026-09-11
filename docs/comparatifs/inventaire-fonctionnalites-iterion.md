---
title: "Fonctionnalités d’Iterion"
description: "139 critères dans seize familles, avec les capacités, sources et limites d’Iterion."
aside: false
pageClass: "comparison-page comparison-inventory"
---

<div lang="fr">

# 🧭 Inventaire approfondi des fonctionnalités d’Iterion

<img class="comparison-logo" src="./assets/logos/iterion.png" alt="Logo Iterion" width="32"> **139 critères recensés · 16 familles · 35 bots présents dans le dépôt**

11 septembre 2026 · Capacités, usages et limites de la version étudiée.

**Iterion couvre davantage que les agents, les boucles et Git : il réunit aussi supervision active, mémoire entre missions, interactions asynchrones, récupération des fichiers, plugins, planification et coordination du travail.** Cet inventaire détaille ces capacités pour choisir les bons critères de comparaison.

← [Comparatif prospects](index.md) · [Tableau de présence — 10 solutions](matrice-fonctionnalites.md#disponibilite) · [Iterion face à n8n](iterion-vs-n8n.md)

> **Lecture des statuts.** ✅ Fonction documentée dans la version étudiée. 🟡 Fonction présente avec une restriction matérielle de backend, de déploiement, de portée ou d’activation. 🧩 Méthode fournie sous forme de bot, utilisant les fonctions du moteur. Les évolutions différées sont regroupées à part et exclues du total.
>
> **Niveau de preuve.** Lecture de la documentation et vérifications ciblées du code au commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b` ; aucun essai d’exécution pendant cet inventaire. Une coche ne signifie ni parité entre tous les backends, ni disponibilité dans une offre managée, ni certification de fiabilité. Le projet se décrit encore comme expérimental. [current-state]

## ⭐ Les capacités à explorer

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 1" tabindex="0">

| Capacité à rendre visible | Ce qu’elle permet d’expliquer à un prospect |
|---|---|
| 👁️ [Supervision active](#supervision) | Un agent peut suivre une mission et lui adresser des corrections pendant le travail. |
| 🙋 [Questions asynchrones](#humain) | L’agent peut avancer pendant que l’utilisateur prépare une réponse, puis attendre au point voulu. |
| 📚 [Mémoire et connaissances](#memoire) | Des connaissances choisies peuvent être réutilisées entre runs, bots ou projets. |
| ⏪ [Récupération et retour arrière](#reprise) | On peut reprendre le graphe, examiner les erreurs et restaurer certains états des fichiers. |
| 🧩 [Skills et plugins](#extensions) | L’équipe peut distribuer ses méthodes, outils et connaissances techniques. |
| ⚡ [Déclencheurs et planification](#declenchement) | Une mission peut partir d’une forge, d’un horaire, d’une carte ou d’un autre résultat. |
| 📋 [Board et configuration partagée](#board) | L’équipe peut suivre le travail et déléguer des réglages précis à des utilisateurs métier. |
| 🔐 [Permissions et protection des données](#protection) | Les accès, secrets et transformations de données deviennent des paramètres explicites du workflow. |

</div>


Ces familles sont des axes de qualification. Leur présence dans Iterion ne prouve pas leur absence chez un concurrent.

## 🗺️ Parcourir l’inventaire

[📝 Concevoir et partager une méthode](#concevoir) · [🔀 Orchestrer agents, code et événements](#orchestrer) · [🧠 Modèles, sessions et contexte](#modeles) · [🙋 Faire intervenir et guider un humain](#humain) · [👁️ Superviser une mission en cours](#supervision) · [📚 Mémoire et connaissances réutilisables](#memoire) · [🧩 Skills, plugins et intégrations techniques](#extensions) · [⏪ Reprendre, réparer et revenir en arrière](#reprise) · [🌿 Travailler et livrer dans un dépôt](#git) · [⚡ Déclencher, planifier et enchaîner](#declenchement) · [📋 Coordonner le travail d’une équipe](#board) · [🔐 Permissions, secrets et données](#protection) · [🏢 Équipes, accès et consommation](#administration) · [🔎 Observer et examiner les résultats](#observer) · [⚙️ Déployer et évaluer la plateforme](#exploiter) · [🤖 Méthodes fournies sous forme de bots](#bots)

<a id="concevoir"></a>

## 📝 Concevoir et partager une méthode

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 2" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="a01"></a>A01 | **Workflows déclaratifs `.bot`** | ✅ | Décrire agents, outils, données et transitions dans un fichier versionnable. Le déterminisme du graphe ne rend pas les réponses LLM identiques. [dsl] |
| <a id="a02"></a>A02 | **Éditeur visuel et source synchronisés** | ✅ | Canvas, bibliothèque de nœuds, inspecteur, validation en direct, annulation et rétablissement. [visual-editor] |
| <a id="a03"></a>A03 | **Création guidée de bots** | ✅ | Modèles de départ, formulaire de mission, paramètres et premier essai ; création équivalente par CLI. [visual-editor] |
| <a id="a04"></a>A04 | **Bundles portables `.botz`** | ✅ | Archive ZIP déterministe réunissant workflow, prompts, skills, pièces jointes et manifeste ; version minimale du moteur déclarable. [bundles] |
| <a id="a05"></a>A05 | **Variables, presets et prompts réutilisables** | ✅ | Paramétrer une même méthode au lancement et distribuer des spécialisations sans copier tout le workflow. [bundles] [dsl] |
| <a id="a06"></a>A06 | **Édition des bots d’équipe** | ✅ | Créer, dupliquer et modifier les fichiers d’un bundle dans le Studio cloud ; contrôle des conflits à l’enregistrement. [visual-editor] |
| <a id="a07"></a>A07 | **Import d’un workflow Claude existant** | 🟡 | Convertisseur statique de fichiers JavaScript Claude vers un brouillon `.bot`, avec rapport des éléments non convertis. Ce n’est pas un import n8n. [import] |
| <a id="a08"></a>A08 | **Schémas et diagnostics statiques** | ✅ | Valider références, propriétés, données et contraintes de graphe avant exécution ; les sorties typées restent à vérifier sur le fond. [dsl] |
| <a id="a09"></a>A09 | **Export de diagramme Mermaid** | ✅ | Produire une représentation du workflow en vue compacte, détaillée ou complète pour la documentation et la revue. [cli-reference] |

</div>


<a id="orchestrer"></a>

## 🔀 Orchestrer agents, code et événements

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 3" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="o01"></a>O01 | **Agents et juges explicites** | ✅ | Distinguer production et évaluation dans le graphe ; un juge est une évaluation LLM configurable, pas une preuve de justesse. [dsl] |
| <a id="o02"></a>O02 | **Outils déterministes et scripts** | ✅ | Nœuds de commandes ou scripts JavaScript, Python, shell/Bash ; restitution de stdout et gestion des erreurs. [dsl] |
| <a id="o03"></a>O03 | **Calculs sans modèle ni shell** | ✅ | Nœud `compute` : expressions bornées, transformations, map/filter/reduce et sorties typées. [dsl] |
| <a id="o04"></a>O04 | **Routage conditionnel, LLM et round-robin** | ✅ | Choisir une branche à partir de règles, d’un arbitrage du modèle ou d’une alternance déclarée. [routers] |
| <a id="o05"></a>O05 | **Branches parallèles et collecte** | 🟡 | `fan_out_all`, `fan_out_each`, collecte `wait_all` ou `best_effort` ; les écritures concurrentes du workspace sont restreintes. [routers] [groups-iteration-subbots] |
| <a id="o06"></a>O06 | **Boucles bornées et parcours ordonnés** | ✅ | Boucles de correction et `foreach` avec compteurs ; un tableau vide nécessite une garde si le corps ne doit jamais s’exécuter. [groups-iteration-subbots] |
| <a id="o07"></a>O07 | **Groupes paramétrés réutilisables** | ✅ | Déclarer un sous-graphe et l’instancier avec paramètres et préfixes distincts. [groups-iteration-subbots] |
| <a id="o08"></a>O08 | **Sous-bots avec leurs propres exécutions** | 🟡 | Appeler un autre bot, récupérer sa sortie et isoler son travail ; budgets, mémoire et superviseurs ont des limites parent/enfant. [groups-iteration-subbots] [memory-and-knowledge] [supervisors] |
| <a id="o09"></a>O09 | **Pools de ressources et limites de concurrence** | ✅ | Déclarer des ressources et réserver un slot avec `needs`, par exemple pour limiter les worktrees simultanés. [groups-iteration-subbots] [dsl] |
| <a id="o10"></a>O10 | **Émettre et attendre un événement interne** | 🟡 | `emit`/`wait` à l’intérieur d’un run, événement conservé pour les attentes tardives et timeout obligatoire. Le stationnement durable sur événement externe est différé. [event-primitives] |

</div>


<a id="modeles"></a>

## 🧠 Modèles, sessions et contexte

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 4" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="b01"></a>B01 | **Six backends d’exécution** | 🟡 | `claw`, `claude_code`, `pi`, `codex`, `kimi`, `grok` ; capacités et niveau de support différents. [backends] [delegation] |
| <a id="b02"></a>B02 | **Choix du modèle et du backend par nœud** | ✅ | Mélanger les exécuteurs dans un workflow ; choisir le modèle indépendamment du backend et appliquer des overrides au lancement. [backends] |
| <a id="b03"></a>B03 | **Repli entre fournisseurs ou identifiants** | 🟡 | Chaînes de fallback configurables lorsque l’accès initial ne convient pas ; les changements doivent rester observables. [backends] |
| <a id="b04"></a>B04 | **Repli entre backends et route de saut** | 🟡 | Repli déclaré vers un autre exécuteur ou une route `skip` prévue par le workflow ; prérequis validés, Codex exclu de la première version du repli inter-backends. [backends] |
| <a id="b05"></a>B05 | **Sessions fraîches, héritées ou bifurquées** | 🟡 | Choisir la continuité conversationnelle entre étapes ; reprise et fork de session non câblés pour Kimi/Grok. [dsl] [delegation] |
| <a id="b06"></a>B06 | **Session conservée au retour dans un nœud** | 🟡 | `session: persist` réutilise la conversation du même nœud dans une boucle ; Claude Code, pi et Codex, sur le tronc du workflow. [session-persist] |
| <a id="b07"></a>B07 | **Transmission par artefacts uniquement** | ✅ | `artifacts_only` transmet le résultat utile sans imposer l’historique conversationnel de l’étape précédente. [dsl] |
| <a id="b08"></a>B08 | **Compaction du contexte** | 🟡 | Seuil et conservation d’échanges récents configurables ; dépend de l’exécuteur et peut accompagner une récupération d’erreur. [dsl] [run-recovery] |
| <a id="b09"></a>B09 | **Curseurs de comportement** | ✅ | Dials qualitatifs ou numériques injectés dans les prompts : profondeur, prudence, style… Ils orientent le modèle sans imposer une permission ou garantir une qualité. [cursors] |
| <a id="b10"></a>B10 | **Réglage de l’effort de raisonnement** | 🟡 | Choisir `reasoning_effort` par nœud ou par donnée transmise ; le niveau est adapté aux valeurs acceptées par le modèle et le backend, indépendamment des curseurs. [ultracode] [effort-code] |
| <a id="b11"></a>B11 | **Sous-agents et workflows créés pendant un nœud** | 🟡 | Le mode `ultracode` ouvre l’orchestration dynamique sur claw et Claude Code selon modèle, harness et outils ; distinct du fan-out et des sous-bots déclarés. Pi ne fournit pas cette surface. [ultracode] [subagents-code] [pi-code] |

</div>


<a id="humain"></a>

## 🙋 Faire intervenir et guider un humain

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 5" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="h01"></a>H01 | **Formulaires de validation typés** | ✅ | Nœuds `human` avec texte, choix, booléens, nombres et listes ; contexte à relire séparé des réponses attendues. [human-in-the-loop] |
| <a id="h02"></a>H02 | **Questions de l’agent pendant son travail** | 🟡 | Outil `ask_user` avec réponse via Studio, CLI ou API ; support dépendant du backend et du transport. [human-in-the-loop] [delegation] |
| <a id="h03"></a>H03 | **Questions sans bloquer l’agent** | 🟡 | `ask_user_async` laisse l’agent poursuivre, puis lui transmet les réponses ; interaction asynchrone déclarée sur le tronc, pas dans les branches fan-out. [async-interaction] |
| <a id="h04"></a>H04 | **Point explicite d’attente des réponses** | 🟡 | `await_answers` synchronise les questions en cours ; timeout requis pour le nœud, qui reste un run actif pendant l’attente. [async-interaction] |
| <a id="h05"></a>H05 | **Réponse automatique ou escalade humaine** | 🟡 | Modes `llm` et `llm_or_human` selon la politique du workflow ; l’automatisation de la réponse n’équivaut pas à une approbation humaine. [human-in-the-loop] |
| <a id="h06"></a>H06 | **Consignes ajoutées à un run actif** | 🟡 | Inbox et steering pour réorienter le travail aux points de prise en compte du backend ; l’action en cours n’est pas instantanément annulée. [backends] [supervisors] |
| <a id="h07"></a>H07 | **Fichiers et images au lancement ou à une réponse** | 🟡 | Pièces jointes persistées, MIME, taille et hash ; vision selon le modèle. Les review gates n’acceptent pas les mêmes uploads que les nœuds humains ordinaires. [attachments] [human-in-the-loop] |
| <a id="h08"></a>H08 | **Dialogue de revue avant intégration** | 🟡 | Review companion, demandes de correction et éventuellement environnement de revue ; politique `human_required` ou verdict agent explicitement choisi. [review-merge-gate] |
| <a id="h09"></a>H09 | **Pause opérateur avec checkpoint** | 🟡 | Demander une pause à la prochaine frontière sûre et reprendre ensuite ; chemin local en processus, pause distante cloud non implémentée. [pause-code] |

</div>


<a id="supervision"></a>

## 👁️ Superviser une mission en cours

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 6" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="s01"></a>S01 | **Agent superviseur d’un autre agent** | 🟡 | Observer un nœud agent/judge et injecter des conseils pendant l’exécution ; les superviseurs déclarés dans un sous-bot ne sont pas raccordés aujourd’hui. [supervisors] |
| <a id="s02"></a>S02 | **Moniteurs, cadence et budget de supervision** | ✅ | Réagir aux événements, erreurs ou signaux de coût avec cooldown et nombre maximal d’évaluations ; coûts de supervision à prendre en compte. [supervisors] |
| <a id="s03"></a>S03 | **Attachement d’un superviseur à une session** | 🟡 | Attacher en CLI à un run local, ou à une session Claude Code avec transcript et hooks préparés ; attachement distant cloud différé. [supervisors] |
| <a id="s04"></a>S04 | **Modifier budgets et itérations en cours de run** | 🟡 | Relever les limites ou accorder des itérations supplémentaires, avec accusé de prise en compte et trace ; application aux frontières sûres. [dsl] [steering-code] |

</div>


<a id="memoire"></a>

## 📚 Mémoire et connaissances réutilisables

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 7" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="m01"></a>M01 | **Mémoire documentaire entre exécutions** | ✅ | Conserver des connaissances sous forme de documents Markdown, au-delà des artefacts d’un seul run. [memory-and-knowledge] |
| <a id="m02"></a>M02 | **Sept périmètres de visibilité** | 🟡 | Run privé, bot, projet, plusieurs projets, utilisateur, organisation, catalogue global ; stockage et contexte qualifient l’accès, le global est en lecture seule pour les organisations. [memory-and-knowledge] |
| <a id="m03"></a>M03 | **Lecture, écriture et liste par l’agent** | 🟡 | `memory_read`, `memory_write`, `memory_list`, droits configurables ; vérifier l’adaptateur et le backend utilisés. [memory-and-knowledge] [memory-code] |
| <a id="m04"></a>M04 | **Index et chargement sélectif en contexte** | ✅ | Injecter un index, précharger certains documents et réinjecter la mémoire avant compaction selon la configuration. [memory-and-knowledge] |
| <a id="m05"></a>M05 | **Mémoire automatique du bot** | 🟡 | `auto_memory` conserve un `MEMORY.md` par bot et dépôt ; opt-in sur claw/Claude Code/pi, avec restrictions pour les sandboxes cloud sans read-back. [memory-and-knowledge] |
| <a id="m06"></a>M06 | **Quotas et garde sur les secrets de mémoire** | 🟡 | Quotas agrégés et par espace/document ; détection de secrets structurés, sans garantie de détection de toute donnée confidentielle. [memory-and-knowledge] |
| <a id="m07"></a>M07 | **Import et export de mémoire** | ✅ | Archives tar.gz via CLI/API et stratégies skip, overwrite ou rename pour conserver ou transférer les connaissances. [memory-and-knowledge] |

</div>


<a id="extensions"></a>

## 🧩 Skills, plugins et intégrations techniques

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 8" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="x01"></a>X01 | **Bibliothèque de skills éditables** | 🟡 | Skills globales, projet ou embarquées ; CLI de gestion et éditeur local, transport des skills sélectionnées vers les runners cloud. [skills-library] |
| <a id="x02"></a>X02 | **Skills sélectionnées par workflow ou nœud** | ✅ | Nom et description dans le contexte, contenu chargé à la demande ; une skill absente produit un avertissement. [skills-library] |
| <a id="x03"></a>X03 | **Plugins à contributions déclaratives** | 🟡 | MCP, skills, réécriture de sorties, commandes, agents, hooks et cycle de vie ; toutes ces contributions ne sont pas prises en charge par tous les backends. [plugins] |
| <a id="x04"></a>X04 | **Plugins privés d’organisation depuis Git** | ✅ | Sources privées et sources publiques, résolues puis transportées pour les runs locaux ou cloud. [plugins] |
| <a id="x05"></a>X05 | **Compression des sorties de commandes** | 🟡 | Plugin RTK et modes `compress` pour réduire le texte envoyé au modèle ; disponibilité du plugin et activation propre à chaque surface. [plugins] |
| <a id="x06"></a>X06 | **Client MCP dans les workflows** | 🟡 | Déclarer des serveurs MCP, les hériter ou les désactiver par nœud ; transports et support dépendent du backend. [dsl] [delegation] |
| <a id="x07"></a>X07 | **Serveur MCP pour piloter Iterion** | ✅ | `iterion mcp` expose lancement, suivi, réponses humaines et opérations de board, en local ou vers une instance distante configurée. [mcp-server] |
| <a id="x08"></a>X08 | **Recherche web et lecture de pages** | 🟡 | Outils natifs ou fournisseurs/MCP configurés, dont DuckDuckGo, Brave, SearXNG et Firecrawl ; couverture variable selon le backend. [web-search] |

</div>


<a id="reprise"></a>

## ⏪ Reprendre, réparer et revenir en arrière

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 9" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="r01"></a>R01 | **Checkpoints de l’état du workflow** | 🟡 | Reprendre les états éligibles avec sorties, compteurs et position ; une étape interrompue peut être réexécutée. [resume] |
| <a id="r02"></a>R02 | **Reprise des sous-bots après redémarrage** | 🟡 | Rattacher le parent à l’exécution enfant enregistrée, au lieu de relancer systématiquement un nouvel enfant ; selon l’état récupérable. [subbot-reattach] |
| <a id="r03"></a>R03 | **Retour à un nœud antérieur** | 🟡 | `rewind` invalide les sorties dépendantes ; `--auto` recherche le nœud modifié. Contraintes sur branches et états de départ. [resume] |
| <a id="r04"></a>R04 | **Snapshots des fichiers hors historique Git** | 🟡 | Tracker filesystem branché sur CLI/Studio local, états dédupliqués aux frontières d’exécution ; le runner cloud ne branche pas ce tracker. Distinct des checkpoints workspace Git poussés. [workspace-versioning] [tracker-wiring-code] [runner-code] |
| <a id="r05"></a>R05 | **Restauration ciblée et sauvegarde avant retour** | 🟡 | À partir des snapshots locaux disponibles, restaurer les fichiers produits par le run en protégeant des changements de l’opérateur ; chemins ignorés ou trop volumineux exclus. [workspace-versioning] [resume] |
| <a id="r06"></a>R06 | **Actions avec postcondition vérifiable** | ✅ | Vérifier une condition avant et après commande, sauter une action déjà satisfaite, choisir une politique required/recover/best_effort. [verified-actions] [verified-code] |
| <a id="r07"></a>R07 | **Réparation bornée d’une action** | 🟡 | Proposer une commande corrigée, puis éventuellement confier la récupération à un agent ; opt-in et tentatives bornées, succès jugé par la postcondition. [verified-actions] [verified-code] |
| <a id="r08"></a>R08 | **Retries et auto-reprise encadrés** | 🟡 | Classification des erreurs, temporisation, compaction ou pause humaine ; auto-resume CLI optionnel, erreurs terminales exclues. [run-recovery] [recovery-code] |
| <a id="r09"></a>R09 | **Indice de récupération après perte d’un pod** | 🟡 | Exposer le dernier checkpoint workspace effectivement poussé et la commande de récupération ; ni restauration automatique ni preuve de livrable validé. [resume] |
| <a id="r10"></a>R10 | **Bifurquer un run depuis un échange antérieur** | 🟡 | Créer un nouveau run à reprendre, avec inputs modifiés et retour au snapshot de code si disponible ; précision du point conversationnel dépendante du backend. [cli-reference] [fork-code] |

</div>


<a id="git"></a>

## 🌿 Travailler et livrer dans un dépôt

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 10" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="g01"></a>G01 | **Worktree distinct par exécution** | 🟡 | Isoler les modifications Git du run ; le comportement dépend du mode et du dépôt, notamment pour un dépôt vide. [dsl] [repo-scope] [worktree-finalization] |
| <a id="g02"></a>G02 | **Politique de finalisation Git** | 🟡 | Conserver ou intégrer le résultat selon la politique choisie ; l’autorité de finalisation doit être accordée. [cli-reference] [worktree-finalization] |
| <a id="g03"></a>G03 | **Gate de merge avec contrôle du résultat** | 🟡 | Revue, corrections et décision de merge/squash dans le parcours prévu ; critères de validation et branche cible à configurer. [review-merge-gate] |
| <a id="g04"></a>G04 | **Vue des changements et commits** | ✅ | Inspecter les fichiers modifiés, le diff et les commits depuis le run ; métadonnées persistées pour les parcours cloud documentés. [visual-editor] [cloud-git] |
| <a id="g05"></a>G05 | **Nettoyage prudent du pool de worktrees** | 🟡 | Collecte bornée des anciens worktrees éligibles ; préserve les travaux sales, actifs ou récupérables, sans constituer un plafond disque strict. [worktree-pool] |
| <a id="g06"></a>G06 | **Environnement d’outillage Devbox** | 🟡 | Préparer l’outillage déclaré du bot et du dépôt ; dépend de l’installation et de l’environnement d’exécution. [devbox] |
| <a id="g07"></a>G07 | **Connexion et création de dépôts depuis le Studio** | 🟡 | Assistant de connexion aux forges, sélection du dépôt cible et création si autorisée ; parcours cloud et permissions de la forge requis. [repo-scope] |

</div>


<a id="declenchement"></a>

## ⚡ Déclencher, planifier et enchaîner

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 11" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="t01"></a>T01 | **Invocations déclarées dans le manifeste** | ✅ | Décrire les entrées forge, commande, schedule, board et keepalive avec lancement direct ou création de carte. [bot-invocations] |
| <a id="t02"></a>T02 | **Webhooks entrants multi-forges et JSON** | ✅ | GitHub, GitLab, Forgejo/Gitea ou payload générique ; filtres d’événements/projets/auteurs et authentification configurée. [webhooks] |
| <a id="t03"></a>T03 | **Commandes et conversations dans une forge** | 🟡 | Commandes slash et réponses à un bot dans les fils GitHub/GitLab autorisés ; envoi de réponse encore fondé sur une skill, capacité native `forge.reply` différée. [forge-conversations] [bot-invocations] |
| <a id="t04"></a>T04 | **Planification locale et cloud** | ✅ | Cron, gardes et politique de chevauchement ; exécution via cron hôte en local ou planificateur de plateforme. [scheduling] |
| <a id="t05"></a>T05 | **Keepalive et attente de disponibilité** | 🟡 | Relancer les missions récurrentes selon les gardes et les fenêtres de disponibilité ; comportement à configurer par bot et backend. [scheduling] [bot-invocations] |
| <a id="t06"></a>T06 | **Déclencheurs sur événements et résultats de runs** | ✅ | Souscriptions aux événements de board, événements explicites ou fins d’exécution pour enchaîner les missions. [trigger-spine] |
| <a id="t07"></a>T07 | **Outbox durable des effets de board cloud** | 🟡 | Matérialisation, claim, retry et échec définitif pour les effets de triggers ; exécution au moins une fois, avec fenêtres de panne résiduelles documentées. [trigger-outbox] |
| <a id="t08"></a>T08 | **Routage automatique de l’issue d’une mission** | 🟡 | Politique figée au lancement pour merge, relance ou escalade ; option de serveur et registre de décisions. [outcome-router] |
| <a id="t09"></a>T09 | **Callbacks HTTP et notifications web** | 🟡 | Callback de fin signé HMAC, alertes opérateur et Web Push ; configuration nécessaire, notifications natives desktop différées. [outbound-callbacks] [notifications] [usage-caps] |

</div>


<a id="board"></a>

## 📋 Coordonner le travail d’une équipe

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 12" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="k01"></a>K01 | **Kanban natif indépendant d’une forge** | ✅ | Cartes, colonnes, labels, priorités, affectations et commentaires, utilisables via Studio ou CLI. [native-tracker] |
| <a id="k02"></a>K02 | **Champs personnalisés typés et dépendances** | ✅ | Schéma de carte validé ; bloqueurs durs et état `waiting_deps` pour éviter un lancement prématuré. [native-tracker] |
| <a id="k03"></a>K03 | **Dispatcher autonome de tickets** | 🟡 | Sélection, claim, lancement, retries, détection de stall et limites de concurrence ; tracker natif et adaptateurs configurables. [dispatcher] |
| <a id="k04"></a>K04 | **Board de pipelines et contrôle des missions** | ✅ | Vue des tâches planifiées et runs actifs, lancement par priorité et limite de pipelines simultanés. [visual-editor] [native-tracker] |
| <a id="k05"></a>K05 | **Synchronisation GitHub Projects V2** | 🟡 | Statuts dans les deux sens ; champs d’issue synchronisés selon une table précise, pas une synchronisation arbitraire de tous les champs. [github-board-sync] |
| <a id="k06"></a>K06 | **Liens entre cartes, runs et résultats** | ✅ | Dernier run, worktree, états d’attente et historique d’événements pour suivre la provenance du travail. [native-tracker] |
| <a id="k07"></a>K07 | **Éditeur de configuration pour un non-opérateur** | 🟡 | Partager un accès révocable à certains champs d’un fichier du dépôt ; commit par la forge, contrôle de conflit et audit, sans donner accès au Studio opérateur. [config-share] |

</div>


<a id="protection"></a>

## 🔐 Permissions, secrets et données

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 13" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="p01"></a>P01 | **Permissions d’outils et mode lecture seule** | 🟡 | Gate ask/deny sur les appels des agents : claw, Claude Code et pi RPC ; Kimi/Grok deny uniquement, Codex sans ce gate. `permission:` n’est pas appliqué aux nœuds shell `tool`. [permissions] [delegation] |
| <a id="p02"></a>P02 | **Sandbox d’exécution** | 🟡 | Isolation configurable selon backend, driver et déploiement ; `auto` peut se dégrader, Codex utilise sa propre sandbox. [sandbox] |
| <a id="p03"></a>P03 | **Secrets liés aux bots et contrôle d’hôtes** | 🟡 | Bindings pour limiter les secrets fournis et `allowed_hosts` pour les parcours d’egress couverts ; ne pas étendre cette garantie à tous les appels shell. [secrets-reference] |
| <a id="p04"></a>P04 | **Secrets scellés au repos** | ✅ | AES-256-GCM avec liaison au record ; clé partagée par les composants cloud autorisés. Pas de rotation automatique de clé maîtresse aujourd’hui. [secrets-reference] |
| <a id="p05"></a>P05 | **Secrets injectés sous forme de fichiers** | 🟡 | Matérialiser un secret `as: file`, avec mécanisme de rafraîchissement en cours de run sur les parcours raccordés. [secrets-reference] [file-secret-refresh] |
| <a id="p06"></a>P06 | **Détection et masquage local de données sensibles** | 🟡 | `privacy_filter` en Go : comptes, emails, téléphones, URL et secrets. Noms de personnes, adresses postales et dates non couverts. [privacy_filter] |
| <a id="p07"></a>P07 | **Restauration des valeurs masquées** | 🟡 | `privacy_unfilter` retrouve les valeurs originales si le fichier du coffre est conservé, sans réplication Mongo/S3 du coffre. Dans les traces, masquage ciblé de l’entrée de `privacy_filter` et de la sortie de `privacy_unfilter`, pas des données circulant ailleurs. [privacy_filter] [vault-code] [runner-code] |
| <a id="p08"></a>P08 | **Contrôles des uploads et aperçus navigateur** | 🟡 | MIME, taille, chemin et protection SSRF sur les routes couvertes ; les contenus actifs sont restreints, sans garantie générale d’innocuité des fichiers. [attachments] [browser-pane] |
| <a id="p09"></a>P09 | **Coffre système pour les identifiants desktop** | 🟡 | Intégration Keychain macOS, Credential Manager Windows ou Secret Service Linux ; disponibilité du service système requise. Distinct du scellement des secrets cloud. [desktop] [keychain-code] |

</div>


<a id="administration"></a>

## 🏢 Équipes, accès et consommation

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 14" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="e01"></a>E01 | **Organisations, équipes et rôles** | ✅ | Isoler les ressources de plateforme par tenant et attribuer les droits opérateur/administrateur et capacités d’édition. [cloud-overview] [config-share] |
| <a id="e02"></a>E02 | **SSO d’organisation** | ✅ | OIDC générique et connecteurs dédiés, avec configuration et politiques d’accès propres à l’organisation. [sso] |
| <a id="e03"></a>E03 | **Clés LLM propres à l’utilisateur ou l’organisation** | ✅ | BYOK, defaults et overrides par lancement/intégration selon la chaîne de résolution ; les fournisseurs restent facturés séparément. [byok] [secrets-reference] |
| <a id="e04"></a>E04 | **Authentification par abonnement/OAuth** | 🟡 | Réutiliser les authentifications prises en charge par les backends CLI, dans les limites de ces accès et de leur fournisseur. [oauth-forfait] [backends] |
| <a id="e05"></a>E05 | **Pool volontaire d’identifiants de contributeurs** | 🟡 | Prêts de credentials avec audience, disponibilité, limites et bail par run ; ce mécanisme n’augmente pas les quotas accordés par les fournisseurs. [credential-pool] |
| <a id="e06"></a>E06 | **Budgets de run : coût, tokens, durée, itérations** | 🟡 | Limites configurables, estimation selon backend, marge de finalisation et budgets séparés des sous-bots ; pas un plafond universel de facture. [dsl] |
| <a id="e07"></a>E07 | **Quotas de plateforme et admission des lancements** | 🟡 | Concurrence, cadence, quota mensuel et coût ; les lectures de quotas peuvent laisser passer le lancement en cas d’erreur de stockage. [quotas-and-limits] |
| <a id="e08"></a>E08 | **Protection des fenêtres d’usage d’abonnement** | 🟡 | Seuils souples/durs et politiques ajustables ; mesure actuellement spécifique à Claude Code, distincte du budget monétaire. [usage-caps] |
| <a id="e09"></a>E09 | **Jetons personnels et audit d’administration** | ✅ | Accès API et actions d’administration attribués ; journal des changements selon les opérations couvertes. [cloud-rest-api] [secrets-reference] |
| <a id="e10"></a>E10 | **Affichage de l’origine des paramètres effectifs** | ✅ | Le lancement indique la valeur et la provenance des modes backend, permission, compression et auto-memory, ainsi que les nœuds ayant leurs propres réglages. [settings-precedence] |

</div>


<a id="observer"></a>

## 🔎 Observer et examiner les résultats

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 15" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="u01"></a>U01 | **Console de run en direct** | ✅ | Graphe, événements, logs et compteurs pour suivre l’exécution depuis le Studio. [visual-editor] |
| <a id="u02"></a>U02 | **Artefacts et historique par étape/itération** | ✅ | Sorties versionnées et événements persistés pour examiner une exécution terminée. [persisted-formats] |
| <a id="u03"></a>U03 | **Rapports de coûts et statistiques d’usage** | ✅ | Répartition par modèle/fournisseur, nombre de runs, échecs et durées P50/P95 ; précision liée à la comptabilisation disponible. [visual-editor] |
| <a id="u04"></a>U04 | **Liste de tâches de la session** | 🟡 | Historique déterministe des TodoWrite/todo_write reconstruit depuis les événements ; alimenté aujourd’hui par Claude Code et claw. [session-board] |
| <a id="u05"></a>U05 | **Widgets de suivi générés par IA** | 🟡 | Notes, métriques, checklists, progression et graphiques ; option locale avec budget d’évaluations, non raccordée au cloud aujourd’hui. [session-board] |
| <a id="u06"></a>U06 | **Aperçus web et captures dans le temps** | 🟡 | URLs de résultat, captures liées au run et navigateur live par attachement manuel local ; live cloud et auto-attach Playwright différés. [browser-pane] |
| <a id="u07"></a>U07 | **Livrables fichiers rendus consultables** | ✅ | Un outil peut publier un fichier produit comme pièce jointe du run par directive stdout ; les échecs de publication sont signalés sans faire échouer l’outil. [attachments] |
| <a id="u08"></a>U08 | **Terminal post-mortem du worktree** | 🟡 | Examiner un run au repos dans son environnement conservé ; local Unix uniquement, sans terminal hôte cloud. [post-mortem-shell] |
| <a id="u09"></a>U09 | **Logs structurés, erreurs et tracing** | 🟡 | JSON de processus, alertes et intégration Sentry/GlitchTip ; DSN requis, tracing activé séparément. [observability] |
| <a id="u10"></a>U10 | **Prometheus, OTLP et tableaux Grafana** | 🟡 | Endpoint de métriques et stack d’observabilité fournie ; coûts, tokens, retries et durées selon les données exposées par chaque backend. [observability-stack] |
| <a id="u11"></a>U11 | **Édition directe des fichiers d’un run** | 🟡 | Éditer un fichier texte dans le worktree local encore présent ; limite de 4 MiB, sans staging Git, verrouillage ou contrôle de conflit concurrent. Distinct de l’éditeur de bots. [run-file-editor] |

</div>


<a id="exploiter"></a>

## ⚙️ Déployer et évaluer la plateforme

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 16" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="d01"></a>D01 | **CLI, Studio, application desktop et cloud auto-hébergé** | ✅ | Plusieurs surfaces autour du même moteur ; leur parité fonctionnelle n’est pas complète. [current-state] [desktop] |
| <a id="d02"></a>D02 | **Runners distribués et stockage de plateforme** | ✅ | File NATS, runners, MongoDB et stockage S3 ; architecture Kubernetes et mise à l’échelle à opérer. [cloud-architecture] |
| <a id="d03"></a>D03 | **Sources du bot figées au lancement cloud** | ✅ | Snapshot du parent, sous-bots et ressources avec empreinte ; les runners n’utilisent pas silencieusement leur propre version du catalogue. [bot-bundle-snapshots] |
| <a id="d04"></a>D04 | **API et SDK TypeScript** | ✅ | Piloter les exécutions, reprendre et consommer les événements ; SDK pour les environnements Node, Deno et Bun documentés. [cloud-rest-api] [readme] |
| <a id="d05"></a>D05 | **Recettes et comparaison de variantes** | ✅ | Combiner workflow, paramètres, prompts, budgets et politique d’évaluation pour organiser des campagnes de benchmark. [recipes] |
| <a id="d06"></a>D06 | **Analyse de convergence sur runs enregistrés** | 🟡 | `bench asymptote` agrège les verdicts par itération sans rejouer les LLM ; ce n’est pas une garantie de convergence ni un benchmark concurrentiel réalisé. [asymptote-bench] |
| <a id="d07"></a>D07 | **Code source sous licence MIT** | ✅ | Moteur modifiable et auto-hébergeable ; les composants et services tiers conservent leurs propres conditions. [license] |
| <a id="d08"></a>D08 | **Bascule multi-projets dans l’application desktop** | ✅ | Sélectionner et retrouver plusieurs projets depuis l’application native ; parcours d’accueil pour préparer un premier projet. [desktop] [desktop-projects-code] |
| <a id="d09"></a>D09 | **Mise à jour desktop avec vérification de signature** | 🟡 | Vérifier le manifeste et l’artefact avec Ed25519, puis proposer l’application de la mise à jour selon le paquet et la plateforme. Cela ne vaut pas notarisation macOS ou signature Windows. [desktop] [updater-code] |

</div>


<a id="bots"></a>

## 🤖 Méthodes fournies sous forme de bots

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 17" tabindex="0">

| Repère | Fonctionnalité | État | Usage et portée |
|---|---|:---:|---|
| <a id="c01"></a>C01 | **Développement d’applications et de fonctionnalités** | 🧩 | Bundles `app-dev`, `feature-dev`, `feature-gap-fill`, `bmady` ; workflows adaptables au dépôt et aux critères du client. [bot-app-dev] [bot-feature-dev] [bot-feature-gap-fill] [bot-bmady] |
| <a id="c02"></a>C02 | **Revue de code et dialogue sur une revue** | 🧩 | `review-pr`, `revi-converse`, avec auxiliaires de revue ; couverture des forges et autorisations à vérifier. [bot-review-pr] [bot-revi-converse] |
| <a id="c03"></a>C03 | **Amélioration continue et modernisation** | 🧩 | `whole-improve-loop`, `branch-improve-loop`, `modernize`, `evolve` ; contrôles et étapes de validation définis par chaque bot. [bot-whole-improve-loop] [bot-branch-improve-loop] [bot-modernize] [bot-evolve] |
| <a id="c04"></a>C04 | **Tests, couverture et instrumentation** | 🧩 | `test-coverage`, `e2e-coverage`, `golden-master`, `instrument` pour construire ou examiner des preuves adaptées au projet. [bot-test-coverage] [bot-e2e-coverage] [bot-golden-master] [bot-instrument] |
| <a id="c05"></a>C05 | **Audit sécurité et maintenance des dépendances** | 🧩 | `sec-audit-source`, `sec-audit-deps`, `dep-update-guard`, `secured-renovacy`, `supply-shield` et variantes ; un audit automatisé ne certifie pas l’absence de vulnérabilités. [bot-sec-audit-source] [bot-sec-audit-deps] [bot-dep-update-guard] [bot-supply-shield] |
| <a id="c06"></a>C06 | **Veille de vulnérabilités** | 🧩 | `vuln-watch` pour détecter et aiguiller le travail de correction selon sa configuration. [bot-vuln-watch] |
| <a id="c07"></a>C07 | **Accessibilité** | 🧩 | `rgaa-audit`, `ultra11y` ; audits et corrections à qualifier sur le périmètre testé, sans promesse de conformité automatique. [bot-rgaa-audit] [bot-ultra11y] |
| <a id="c08"></a>C08 | **Documentation et architecture** | 🧩 | `docs-refresh`, `product-docs`, `wiki-gen`, `adr-cartograph`, `adr-rechallenge` ; production et mise à jour de documents à relire. [bot-docs-refresh] [bot-product-docs] [bot-wiki-gen] [bot-adr-cartograph] [bot-adr-rechallenge] |
| <a id="c09"></a>C09 | **Veille éditoriale, triage et planification** | 🧩 | `feed-watch`, `issue-triage`, `whats-next`, `campaign` ; collecte, synthèse et coordination selon les workflows fournis. [bot-feed-watch] [bot-issue-triage] [bot-whats-next] [bot-campaign] |

</div>


<a id="limites"></a>
## 🚧 Évolutions différées et limites à conserver

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 18" tabindex="0">

| Sujet | État retenu dans cet inventaire |
|---|---|
| Versionnement `dsl: 2`, imports explicites de fichiers et migration | Programme décrit par l’ADR-098 ; pas de preuve d’implémentation trouvée dans le parser/CLI au commit étudié. Ne pas annoncer ces commandes ou imports comme livrés. [dsl-roadmap] |
| Run durablement stationné sur événement externe | Différé ; distinguer ce mécanisme des triggers qui lancent un run et des attentes internes actives. [event-primitives] [async-interaction] |
| Superviseurs déclarés dans les sous-bots | Déclarations non raccordées ; ne pas étendre la capacité du parent aux enfants. [supervisors] |
| Attachement cloud d’un superviseur et interrupteur Studio | Évolutions différées ; la supervision déclarée dans le workflow existe, l’attachement CLI vise le local. [supervisors] |
| Mémoire : parité de tous les parcours | L’UI ne liste pas correctement certains espaces bot/projet ; les sous-bots cloud ne reçoivent pas le MemoryStore parent ; auto-memory limitée par le read-back du driver. [memory-and-knowledge] |
| Snapshots filesystem et coffre PII sur un autre pod | Le tracker de fichiers est local ; le coffre PII est un fichier du storeDir de l’exécuteur. Les checkpoints Git distants et le stockage Mongo/S3 n’assurent pas leur transfert. [tracker-wiring-code] [vault-code] [runner-code] |
| Browser live cloud et auto-attach Playwright | Documentés comme suite à réaliser. Les aperçus et captures existants ne prouvent pas le live distant. [browser-pane] |
| Widgets de session générés par IA dans le cloud | Stockage cloud et déclaration DSL dédiés différés ; curation actuelle locale et opt-in. [session-board] |
| Détection PII de personnes, adresses postales et dates | Prévue pour une version ultérieure ; les cinq catégories actuelles restent un détecteur à couverture limitée. [privacy_filter] |
| Rotation automatique de clé maîtresse, KMS par tenant | Non disponible ; le stockage scellé actuel ne fournit pas cette fonction. [secrets-reference] |
| Notifications natives desktop | Différées ; Web Push ne signifie pas notification native de l’application. [notifications] |
| Bot automatique de correction depuis Sentry | Intention décrite ; le bot `error-watch` n’existe pas encore. L’émission d’erreurs et l’accès MCP Sentry sont des capacités distinctes. [sentry-feedback-loop] |
| Catalogue élargi de connecteurs métier | Évolution proposée ; les futurs connecteurs ne sont pas comptés comme installables. Suivi : [catalogue de connecteurs, ticket #1072](https://github.com/SocialGouv/iterion/issues/1072). |

</div>


**Deux limites transversales.** Les budgets des sous-bots ne sont pas agrégés au parent, et les quotas de lancement ne constituent pas une barrière financière stricte en cas d’erreur de stockage. La reprise d’un run ne garantit pas que tous ses effets externes ne seront exécutés qu’une seule fois. [dsl] [quotas-and-limits] [resume]

**Méthodes d’amélioration.** Les bots peuvent conserver progressivement du travail validé et mesurer les verdicts de leurs itérations. Cela dépend des contrôles réellement configurés ; l’inventaire ne revendique ni progrès garanti à chaque itération, ni absence de régression. [improvement-ratchet]

<a id="preuves"></a>
## 🔬 Traçabilité de l’inventaire

Les critères ont des identifiants stables pour alimenter les futures comparaisons. Le total décrit des critères à des niveaux différents — primitives, interfaces, exploitation et méthodes — et ne sert pas de score contre les concurrents.

Les références de chaque ligne pointent vers la version figée du dépôt, et non vers une branche mouvante. En complément des documents, les vérifications ciblées suivantes ont permis de confirmer plusieurs mécanismes importants :

<div class="comparison-table" role="region" aria-label="Fonctionnalités d’Iterion — tableau 19" tabindex="0">

| Famille | Repère dans le code consulté |
|---|---|
| Interaction asynchrone | `StoreAsyncAskBinder`, persistance de `InteractionKindAsync`, événement `human_input_requested` : [code async][async-code]. |
| Supervision | `Coordinator`, cooldown, plafond `MaxEvals` et injection : [code supervision][supervise-code]. |
| Mémoire | Sept valeurs de `Visibility`, contrat `MemoryStore` et branchement des outils/index : [périmètres][scope-code], [contrat][knowledge-code], [outils][memory-code]. |
| Actions vérifiées | Précondition de saut, recette, postcondition et étages bornés de réparation : [implémentation][verified-code]. |
| Récupération | Dispatch par classe d’erreur, compteur de tentatives, retry/compaction/pause : [implémentation][recovery-code]. |
| Fichiers et rewind | `Capture`, `Restore` et `RestoreOnly`, chemins protégés : [contrat du tracker][tracker-code]. |
| Plugins | Manifeste et contributions typées : [implémentation][plugin-code]. |
| Éditeur partagé | Projection des champs lisibles, validation du patch et refus des conflits de SHA : [service][config-code]. |
| Pool de credentials | Acquisition, classement des contributeurs, baux et restitution : [broker][credpool-code]. |
| Triggers de board | Claim, retry et matérialisation d’effets : [worker][outbox-code]. |
| Pilotage des limites | Commandes `bump_loop`/`raise_budget`, déduplication et réponse typée : [transport][steering-code]. |
| Masquage PII | Enregistrement des deux outils, validation et coffre par run : [implémentation][privacy-code]. |

</div>


La méthode combine lecture des références et vérification ciblée de leur implémentation. Les capacités décrites restent rattachées au commit cité ; elles doivent être réexaminées lors d’un changement de version.

### 📦 Les 35 bundles réellement présents

Comptage des `bots/*/main.bot` au commit étudié. Ce nombre indique des bundles sources, pas 35 garanties de qualité, ni 35 bots tous embarqués dans chaque binaire. Certains sont des auxiliaires d’autres parcours.

[adr-cartograph](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-cartograph/main.bot) · [adr-rechallenge](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-rechallenge/main.bot) · [app-dev](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/app-dev/main.bot) · [arbitrate](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/arbitrate/main.bot) · [bmady](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/bmady/main.bot) · [branch-improve-loop](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/branch-improve-loop/main.bot) · [campaign](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/campaign/main.bot) · [dep-update-guard](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/dep-update-guard/main.bot) · [devbox-setup](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/devbox-setup/main.bot) · [docs-refresh](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/docs-refresh/main.bot) · [e2e-coverage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/e2e-coverage/main.bot) · [evolve](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/evolve/main.bot) · [feature-dev](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-dev/main.bot) · [feature-gap-fill](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-gap-fill/main.bot) · [feed-watch](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feed-watch/main.bot) · [golden-master](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/golden-master/main.bot) · [instrument](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/instrument/main.bot) · [issue-triage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/issue-triage/main.bot) · [modernize](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/modernize/main.bot) · [product-docs](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/product-docs/main.bot) · [revi-converse](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/revi-converse/main.bot) · [review-env](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-env/main.bot) · [review-pr](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-pr/main.bot) · [rgaa-audit](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/rgaa-audit/main.bot) · [sec-audit-deps](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-deps/main.bot) · [sec-audit-source](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-source/main.bot) · [secured-renovacy](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/secured-renovacy/main.bot) · [supply-shield](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield/main.bot) · [supply-shield-cve](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield-cve/main.bot) · [test-coverage](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/test-coverage/main.bot) · [ultra11y](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/ultra11y/main.bot) · [vuln-watch](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/vuln-watch/main.bot) · [whats-next](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whats-next/main.bot) · [whole-improve-loop](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whole-improve-loop/main.bot) · [wiki-gen](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/wiki-gen/main.bot)

La [version CSV de cet inventaire](/comparatifs/inventaire-fonctionnalites-iterion.csv) reprend les mêmes critères, statuts, limites et références pour préparer les prochains tableaux.

### 📎 Sources figées

Les liens courts ci-dessus renvoient aux documents et fichiers suivants.

- [asymptote-bench] : `docs/asymptote-bench.md`.
- [async-code] : `pkg/backend/model/async_ask.go`.
- [async-interaction] : `docs/async-interaction.md`.
- [attachments] : `docs/attachments.md`.
- [backends] : `docs/backends.md`.
- [bot-adr-cartograph] : `bots/adr-cartograph/main.bot`.
- [bot-adr-rechallenge] : `bots/adr-rechallenge/main.bot`.
- [bot-app-dev] : `bots/app-dev/main.bot`.
- [bot-bmady] : `bots/bmady/main.bot`.
- [bot-branch-improve-loop] : `bots/branch-improve-loop/main.bot`.
- [bot-bundle-snapshots] : `docs/bot-bundle-snapshots.md`.
- [bot-campaign] : `bots/campaign/main.bot`.
- [bot-dep-update-guard] : `bots/dep-update-guard/main.bot`.
- [bot-docs-refresh] : `bots/docs-refresh/main.bot`.
- [bot-e2e-coverage] : `bots/e2e-coverage/main.bot`.
- [bot-evolve] : `bots/evolve/main.bot`.
- [bot-feature-dev] : `bots/feature-dev/main.bot`.
- [bot-feature-gap-fill] : `bots/feature-gap-fill/main.bot`.
- [bot-feed-watch] : `bots/feed-watch/main.bot`.
- [bot-golden-master] : `bots/golden-master/main.bot`.
- [bot-instrument] : `bots/instrument/main.bot`.
- [bot-invocations] : `docs/bot-invocations.md`.
- [bot-issue-triage] : `bots/issue-triage/main.bot`.
- [bot-modernize] : `bots/modernize/main.bot`.
- [bot-product-docs] : `bots/product-docs/main.bot`.
- [bot-revi-converse] : `bots/revi-converse/main.bot`.
- [bot-review-pr] : `bots/review-pr/main.bot`.
- [bot-rgaa-audit] : `bots/rgaa-audit/main.bot`.
- [bot-sec-audit-deps] : `bots/sec-audit-deps/main.bot`.
- [bot-sec-audit-source] : `bots/sec-audit-source/main.bot`.
- [bot-supply-shield] : `bots/supply-shield/main.bot`.
- [bot-test-coverage] : `bots/test-coverage/main.bot`.
- [bot-ultra11y] : `bots/ultra11y/main.bot`.
- [bot-vuln-watch] : `bots/vuln-watch/main.bot`.
- [bot-whats-next] : `bots/whats-next/main.bot`.
- [bot-whole-improve-loop] : `bots/whole-improve-loop/main.bot`.
- [bot-wiki-gen] : `bots/wiki-gen/main.bot`.
- [browser-pane] : `docs/browser-pane.md`.
- [bundles] : `docs/bundles.md`.
- [byok] : `docs/byok.md`.
- [cli-reference] : `docs/cli-reference.md`.
- [cloud-architecture] : `docs/cloud-architecture.md`.
- [cloud-git] : `docs/adr/068-persist-run-diff-content-for-cloud-panels.md`.
- [cloud-overview] : `docs/cloud-overview.md`.
- [cloud-rest-api] : `docs/cloud-rest-api.md`.
- [config-code] : `pkg/configshare/service.go`.
- [config-share] : `docs/config-share.md`.
- [credential-pool] : `docs/credential-pool.md`.
- [credpool-code] : `pkg/credpool/broker.go`.
- [current-state] : `docs/current-state.md`.
- [cursors] : `docs/cursors.md`.
- [delegation] : `docs/delegation.md`.
- [desktop] : `docs/desktop.md`.
- [desktop-projects-code] : `cmd/iterion-desktop/bindings.go`.
- [devbox] : `docs/adr/017-devbox-first-bot-toolchain.md`.
- [dispatcher] : `docs/dispatcher.md`.
- [dsl] : `docs/dsl.md`.
- [dsl-roadmap] : `docs/adr/098-dsl-versioning-and-authoring-surface.md`.
- [effort-code] : `pkg/backend/model/effort.go`.
- [event-primitives] : `docs/adr/051-in-bot-event-driven-primitives.md`.
- [file-secret-refresh] : `docs/adr/069-mid-run-file-secret-refresh-for-sandboxed-runs.md`.
- [forge-conversations] : `docs/forge-conversations.md`.
- [fork-code] : `pkg/runview/fork.go`.
- [github-board-sync] : `docs/github-board-sync.md`.
- [groups-iteration-subbots] : `docs/groups-iteration-subbots.md`.
- [human-in-the-loop] : `docs/human-in-the-loop.md`.
- [import] : `docs/import.md`.
- [improvement-ratchet] : `docs/improvement-ratchet.md`.
- [keychain-code] : `cmd/iterion-desktop/keychain.go`.
- [knowledge-code] : `pkg/knowledge/iface.go`.
- [license] : `LICENSE`.
- [mcp-server] : `docs/mcp-server.md`.
- [memory-and-knowledge] : `docs/memory-and-knowledge.md`.
- [memory-code] : `pkg/backend/model/memory_tools.go`.
- [native-tracker] : `docs/native-tracker.md`.
- [notifications] : `docs/notifications.md`.
- [oauth-forfait] : `docs/oauth-forfait.md`.
- [observability] : `docs/observability.md`.
- [observability-stack] : `docs/observability/README.md`.
- [outbound-callbacks] : `docs/outbound-callbacks.md`.
- [outbox-code] : `pkg/trigger/effects_worker.go`.
- [outcome-router] : `docs/outcome-router.md`.
- [pause-code] : `pkg/runview/service_control.go`.
- [permissions] : `docs/permissions.md`.
- [persisted-formats] : `docs/persisted-formats.md`.
- [pi-code] : `pkg/backend/delegate/pi.go`.
- [plugin-code] : `pkg/plugin/manifest.go`.
- [plugins] : `docs/plugins.md`.
- [post-mortem-shell] : `docs/post-mortem-shell.md`.
- [privacy-code] : `pkg/backend/tool/privacy/register.go`.
- [privacy_filter] : `docs/privacy_filter.md`.
- [quotas-and-limits] : `docs/quotas-and-limits.md`.
- [readme] : `README.md`.
- [recipes] : `docs/recipes.md`.
- [recovery-code] : `pkg/runtime/recovery_dispatch.go`.
- [repo-scope] : `docs/repo-scope.md`.
- [resume] : `docs/resume.md`.
- [review-merge-gate] : `docs/review-merge-gate.md`.
- [routers] : `docs/routers.md`.
- [run-file-editor] : `docs/adr/016-in-run-worktree-file-editor.md`.
- [run-recovery] : `docs/adr/056-adaptive-run-level-recovery.md`.
- [runner-code] : `pkg/runner/loop.go`.
- [sandbox] : `docs/sandbox.md`.
- [scheduling] : `docs/scheduling.md`.
- [scope-code] : `pkg/knowledge/scope.go`.
- [secrets-reference] : `docs/secrets-reference.md`.
- [sentry-feedback-loop] : `docs/sentry-feedback-loop.md`.
- [session-board] : `docs/session-board.md`.
- [session-persist] : `docs/adr/089-session-persist.md`.
- [settings-precedence] : `docs/settings-precedence.md`.
- [skills-library] : `docs/skills-library.md`.
- [sso] : `docs/adr/035-per-org-sso-generic-oidc-plus-dedicated-connectors.md`.
- [steering-code] : `pkg/runview/steer.go`.
- [subagents-code] : `pkg/backend/model/executor_build_task.go`.
- [subbot-reattach] : `docs/adr/084-subbot-reattach-across-restarts.md`.
- [supervise-code] : `pkg/supervise/coordinator.go`.
- [supervisors] : `docs/supervisors.md`.
- [tracker-code] : `pkg/workspacetrack/tracker.go`.
- [tracker-wiring-code] : `pkg/runview/service_launch.go`.
- [trigger-outbox] : `docs/adr/094-trigger-effect-outbox.md`.
- [trigger-spine] : `docs/adr/046-event-driven-runs-trigger-spine.md`.
- [ultracode] : `docs/ultracode.md`.
- [updater-code] : `cmd/iterion-desktop/updater.go`.
- [usage-caps] : `docs/usage-caps.md`.
- [vault-code] : `pkg/backend/tool/privacy/vault.go`.
- [verified-actions] : `docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md`.
- [verified-code] : `pkg/backend/model/executor_verified_action.go`.
- [visual-editor] : `docs/visual-editor.md`.
- [web-search] : `docs/web-search.md`.
- [webhooks] : `docs/webhooks.md`.
- [workspace-versioning] : `docs/workspace-versioning.md`.
- [worktree-finalization] : `docs/adr/064-worktree-finalization-requires-delegated-authority.md`.
- [worktree-pool] : `docs/worktree-pool.md`.


[asymptote-bench]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/asymptote-bench.md
[async-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/async_ask.go
[async-interaction]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/async-interaction.md
[attachments]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/attachments.md
[backends]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/backends.md
[bot-adr-cartograph]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-cartograph/main.bot
[bot-adr-rechallenge]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/adr-rechallenge/main.bot
[bot-app-dev]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/app-dev/main.bot
[bot-bmady]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/bmady/main.bot
[bot-branch-improve-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/branch-improve-loop/main.bot
[bot-bundle-snapshots]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-bundle-snapshots.md
[bot-campaign]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/campaign/main.bot
[bot-dep-update-guard]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/dep-update-guard/main.bot
[bot-docs-refresh]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/docs-refresh/main.bot
[bot-e2e-coverage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/e2e-coverage/main.bot
[bot-evolve]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/evolve/main.bot
[bot-feature-dev]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-dev/main.bot
[bot-feature-gap-fill]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feature-gap-fill/main.bot
[bot-feed-watch]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/feed-watch/main.bot
[bot-golden-master]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/golden-master/main.bot
[bot-instrument]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/instrument/main.bot
[bot-invocations]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-invocations.md
[bot-issue-triage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/issue-triage/main.bot
[bot-modernize]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/modernize/main.bot
[bot-product-docs]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/product-docs/main.bot
[bot-revi-converse]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/revi-converse/main.bot
[bot-review-pr]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/review-pr/main.bot
[bot-rgaa-audit]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/rgaa-audit/main.bot
[bot-sec-audit-deps]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-deps/main.bot
[bot-sec-audit-source]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/sec-audit-source/main.bot
[bot-supply-shield]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/supply-shield/main.bot
[bot-test-coverage]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/test-coverage/main.bot
[bot-ultra11y]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/ultra11y/main.bot
[bot-vuln-watch]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/vuln-watch/main.bot
[bot-whats-next]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whats-next/main.bot
[bot-whole-improve-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/whole-improve-loop/main.bot
[bot-wiki-gen]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/bots/wiki-gen/main.bot
[browser-pane]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/browser-pane.md
[bundles]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bundles.md
[byok]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/byok.md
[cli-reference]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cli-reference.md
[cloud-architecture]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-architecture.md
[cloud-git]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/068-persist-run-diff-content-for-cloud-panels.md
[cloud-overview]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md
[cloud-rest-api]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-rest-api.md
[config-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/configshare/service.go
[config-share]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/config-share.md
[credential-pool]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/credential-pool.md
[credpool-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/credpool/broker.go
[current-state]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/current-state.md
[cursors]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cursors.md
[delegation]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/delegation.md
[desktop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/desktop.md
[desktop-projects-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/bindings.go
[devbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/017-devbox-first-bot-toolchain.md
[dispatcher]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dispatcher.md
[dsl]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md
[dsl-roadmap]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/098-dsl-versioning-and-authoring-surface.md
[effort-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/effort.go
[event-primitives]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/051-in-bot-event-driven-primitives.md
[file-secret-refresh]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/069-mid-run-file-secret-refresh-for-sandboxed-runs.md
[forge-conversations]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/forge-conversations.md
[fork-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/fork.go
[github-board-sync]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/github-board-sync.md
[groups-iteration-subbots]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/groups-iteration-subbots.md
[human-in-the-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/human-in-the-loop.md
[import]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/import.md
[improvement-ratchet]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/improvement-ratchet.md
[keychain-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/keychain.go
[knowledge-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/knowledge/iface.go
[license]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE
[mcp-server]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/mcp-server.md
[memory-and-knowledge]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/memory-and-knowledge.md
[memory-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/memory_tools.go
[native-tracker]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/native-tracker.md
[notifications]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/notifications.md
[oauth-forfait]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/oauth-forfait.md
[observability]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability.md
[observability-stack]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability/README.md
[outbound-callbacks]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/outbound-callbacks.md
[outbox-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/trigger/effects_worker.go
[outcome-router]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/outcome-router.md
[pause-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/service_control.go
[permissions]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/permissions.md
[persisted-formats]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/persisted-formats.md
[pi-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/delegate/pi.go
[plugin-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/plugin/manifest.go
[plugins]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/plugins.md
[post-mortem-shell]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/post-mortem-shell.md
[privacy-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/tool/privacy/register.go
[privacy_filter]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/privacy_filter.md
[quotas-and-limits]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/quotas-and-limits.md
[readme]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md
[recipes]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/recipes.md
[recovery-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runtime/recovery_dispatch.go
[repo-scope]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/repo-scope.md
[resume]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md
[review-merge-gate]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/review-merge-gate.md
[routers]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/routers.md
[run-file-editor]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/016-in-run-worktree-file-editor.md
[run-recovery]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/056-adaptive-run-level-recovery.md
[runner-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runner/loop.go
[sandbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sandbox.md
[scheduling]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/scheduling.md
[scope-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/knowledge/scope.go
[secrets-reference]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/secrets-reference.md
[sentry-feedback-loop]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sentry-feedback-loop.md
[session-board]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/session-board.md
[session-persist]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/089-session-persist.md
[settings-precedence]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/settings-precedence.md
[skills-library]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/skills-library.md
[sso]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/035-per-org-sso-generic-oidc-plus-dedicated-connectors.md
[steering-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/steer.go
[subagents-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/executor_build_task.go
[subbot-reattach]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/084-subbot-reattach-across-restarts.md
[supervise-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/supervise/coordinator.go
[supervisors]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/supervisors.md
[tracker-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/workspacetrack/tracker.go
[tracker-wiring-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/runview/service_launch.go
[trigger-outbox]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/094-trigger-effect-outbox.md
[trigger-spine]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/046-event-driven-runs-trigger-spine.md
[ultracode]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/ultracode.md
[updater-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/cmd/iterion-desktop/updater.go
[usage-caps]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/usage-caps.md
[vault-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/tool/privacy/vault.go
[verified-actions]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md
[verified-code]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/pkg/backend/model/executor_verified_action.go
[visual-editor]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/visual-editor.md
[web-search]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/web-search.md
[webhooks]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/webhooks.md
[workspace-versioning]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/workspace-versioning.md
[worktree-finalization]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md
[worktree-pool]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/worktree-pool.md

</div>
