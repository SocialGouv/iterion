---
title: "Choisir Iterion ou une alternative"
description: "Guide de choix entre Iterion, n8n et huit autres solutions, selon votre besoin."
aside: false
pageClass: "comparison-page"
---

<div lang="fr">

# 🧭 Iterion et ses alternatives : quel outil pour quel besoin ?

Version du 11 septembre 2026 · Comparatif destiné aux prospects et clients.

🔍 **Tableau détaillé :** [matrice de fonctionnalités — 10 solutions, 20 critères produit et techniques](matrice-fonctionnalites.md).

✅ [Voir directement quelles fonctionnalités chaque produit propose](matrice-fonctionnalites.md#disponibilite).

📚 [Inventaire approfondi d’Iterion : 139 critères, 16 familles et 35 bots recensés](inventaire-fonctionnalites-iterion.md).

**Iterion permet de transformer une méthode de travail avec des agents IA en un workflow versionné, exécutable et supervisable.** Son intérêt est particulièrement concret pour les équipes qui font intervenir des agents sur des dépôts de code : analyser, implémenter, tester, relire, corriger et soumettre un résultat à validation. Le moteur apporte des boucles bornées, des budgets configurables, des points de reprise et un suivi des exécutions. [Présentation d’Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md).

Le bon choix dépend du travail à automatiser, des outils déjà présents et de la responsabilité que l’équipe veut conserver sur l’exploitation. Ce comparatif vous aide à identifier ce qu’Iterion peut apporter à votre projet et les points à valider avant de l’adopter. Il s’appuie sur les capacités documentées, sans benchmark concurrentiel.

## 🎯 Choisir selon le résultat attendu

<div class="comparison-table comparison-choice" role="region" aria-label="Choisir Iterion ou une alternative — tableau 1" tabindex="0">

| Votre besoin principal | Positionnement des solutions |
| --- | --- |
| Répéter une méthode de développement, de revue ou de maintenance avec des agents | <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** |
| Relier un CRM, une messagerie, des formulaires et d’autres applications | **n8n**, **Make**, **Zapier**, **Activepieces** |
| Construire un assistant qui répond à partir de documents et de bases de connaissances | **Dify** ; **Flowise** pour un existant à maintenir (fin de vie) |
| Développer une application d’agents avec une logique très personnalisée | **LangGraph**, **CrewAI** |
| Exécuter des scripts, des traitements internes et des workflows techniques | <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** |

</div>


Ces repères décrivent les usages couverts par les solutions. Ils ne signifient pas qu’un même résultat est impossible avec un autre produit : l’effort d’intégration et le pilotage de la mission font aussi partie du choix.

### 💡 Quand inclure Iterion dans votre sélection

**Ajoutez Iterion à votre sélection si vous vous reconnaissez dans ces situations :**

- **Dans un processus CRM, support ou métier** : une étape devient une mission d’agent avec plusieurs phases de travail, de contrôle et de correction.
- **Au-delà d’un assistant documentaire** : vous voulez aussi faire exécuter des missions et produire des livrables, notamment dans un dépôt Git.
- **Pour une application d’agents personnalisée** : vous souhaitez décrire vos workflows et disposer des fonctions de pilotage d’Iterion, avec moins de raccordements à concevoir vous-même.
- **Dans des workflows techniques** : le travail des agents, leurs itérations et la validation de leurs résultats deviennent centraux.

Pour répéter une méthode de développement, de revue ou de maintenance, **Iterion est directement à évaluer** : ce besoin est illustré par son catalogue de bots.

## ⚖️ Les alternatives en regard

<div class="comparison-table" role="region" aria-label="Choisir Iterion ou une alternative — tableau 2" tabindex="0">

| Solution | Capacités documentées | Ce qu’Iterion apporte dans ce contexte |
|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Workflows `.bot`, éditeur visuel, agents et outils, revue/correction, budgets, reprise, worktrees Git et exploitation locale ou en plateforme auto-hébergée. | Un parcours réunissant la définition de la mission, le travail sur le dépôt, la revue et la validation du résultat. Le projet reste expérimental : à valider sur votre périmètre pilote. [État du produit](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/current-state.md). |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Automatisation visuelle, nombreux connecteurs, code, agents IA, MCP et contrôles humains ; cloud et auto-hébergement. | Iterion prend en charge le cycle de travail sur le dépôt : worktree, tests, revue, correction et finalisation. Une mission Iterion peut compléter un flux métier existant via une intégration à réaliser. |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Construction visuelle de scénarios entre applications, routage, filtres et fonctions de code. Offre hébergée avec facturation en crédits. | Iterion permet de décrire et de piloter une mission d’agents avec étapes de contrôle, boucles de correction et résultat de projet à valider. |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Connexion d’applications, déclencheurs et actions ; AI by Zapier intègre les étapes d’agents dans les Zaps. | Iterion réunit les étapes d’une mission sur dépôt dans un workflow versionné et supervisable ; son intérêt porte sur ce parcours complet. |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Flows, agents, connexions partagées et MCP, avec approbations et possibilités d’auto-hébergement. | Iterion associe les agents et les approbations au travail Git, aux budgets de mission et aux cycles de revue/correction. Ces fonctions partagées ne sont pas présentées comme des exclusivités. |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Plateforme visuelle de workflows IA et de recherche dans des bases documentaires — RAG — avec modèles et outils intégrés ; cloud, VPC ou auto-hébergement. | Iterion organise des missions qui produisent des livrables vérifiables : modifications de projet, tests et revues, avec suivi de l’exécution. |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise** | Construction visuelle d’agents, de chatflows et d’agentflows, avec traces, évaluations et interfaces d’intégration. | Iterion réunit le workflow, l’environnement de projet et les opérations Git. Pour un existant Flowise, la migration doit tenir compte des intégrations à reprendre et de sa fin de vie. [Documentation Flowise](https://docs.flowiseai.com/). |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Runtime d’orchestration d’agents avec état, persistance, streaming et intervention humaine, programmable au niveau du graphe. | Iterion fournit un langage de workflow et des interfaces de pilotage déjà réunis. Le choix dépend de la personnalisation recherchée et des composants que l’équipe souhaite assembler elle-même. [Présentation LangGraph](https://docs.langchain.com/oss/python/langgraph/overview). |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Framework Python combinant équipes d’agents spécialisés et flows explicites, avec état et mécanismes de contrôle. | Iterion réunit définition des missions, travail sur dépôt et exploitation. L’effort à comparer inclut le framework, son déploiement et le pilotage à construire autour. [Concepts CrewAI](https://docs.crewai.com/core-concepts/Agents). |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Scripts multilangages, workflows, applications internes, étapes d’agents IA et sandboxes pour agents de programmation avec fichiers persistants. | Iterion intègre worktrees, politique de finalisation Git, budgets et boucles de revue. La comparaison doit porter sur ce parcours complet, avec les capacités de sandbox déjà présentes dans Windmill. [Plateforme](https://www.windmill.dev/docs/intro), [AI Agents](https://www.windmill.dev/docs/core_concepts/ai_agents), [AI Sandbox](https://www.windmill.dev/docs/core_concepts/ai_sandbox). |

</div>


[Consulter les capacités détaillées et leurs sources](matrice-fonctionnalites.md#sources).

> 🗄 **Flowise : fin de vie annoncée au 31 août 2026, dépôt archivé depuis le 13 août.** Les fonctions décrites restent celles de sa documentation ; le Cloud encore affiché sur le site ne constitue pas une garantie de service ou de support actuel. [Annonce officielle](https://github.com/FlowiseAI/Flowise/discussions/6727).

## 🧩 Ce qu’Iterion apporte à une équipe

**Une méthode qui se partage.** Le workflow est un fichier `.bot` que l’équipe peut versionner, relire et faire évoluer. Il décrit les agents, les outils, les étapes de contrôle et les conditions de passage. L’éditeur visuel complète cette approche. Le bénéfice attendu est de rendre une méthode réutilisable au-delà de la personne qui l’a mise au point. [Langage et orchestration](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md).

**Un cycle de travail adapté aux dépôts.** Iterion fournit des bots de revue, de développement, de documentation et de maintenance. Ses worktrees permettent de travailler dans une copie Git distincte ; une politique de finalisation détermine comment récupérer ou intégrer le résultat. Ce parcours intégré est un axe de comparaison plus utile que la présence d’un simple nœud Git. [Catalogue](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/examples.md), [finalisation Git](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md).

**Des exécutions que l’on peut piloter.** L’opérateur peut suivre une mission, répondre à une demande humaine et reprendre les exécutions dont l’état le permet. La reprise conserve les étapes déjà achevées ; une étape interrompue ou en échec peut devoir être réexécutée. Les budgets et les limites d’itération encadrent le travail selon la configuration retenue. [Reprise](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md), [budgets](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md#budget-fields).

**Une collaboration pendant la mission.** L’agent peut poser une question sans arrêter son travail, puis attendre la réponse au point prévu. Un superviseur peut observer un autre agent et lui adresser des consignes. Ces mécanismes ont des limites de backend et de composition, détaillées dans l’inventaire. [Questions asynchrones](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/async-interaction.md), [superviseurs](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/supervisors.md).

**Des connaissances et des outils qui se réutilisent.** Mémoire documentaire, skills et plugins permettent de partager les connaissances et méthodes choisies entre missions. Les périmètres vont du run privé à l’organisation, selon le stockage et la configuration. La mémoire automatique reste optionnelle et dépend du backend. [Mémoire](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/memory-and-knowledge.md), [skills](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/skills-library.md), [plugins](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/plugins.md).

**Un travail coordonné dans la durée.** Déclencheurs, planification, kanban natif et dispatcher relient les événements aux missions à accomplir. Un accès de configuration restreint peut aussi permettre à un utilisateur métier de modifier certains champs, par exemple les sources d’une veille. [Invocations](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/bot-invocations.md), [kanban](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/native-tracker.md), [éditeur partagé](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/config-share.md).

**Des outils pour examiner et récupérer le travail.** Les snapshots de fichiers, le retour à une étape antérieure et les actions à postcondition vérifiable complètent la reprise du graphe. Ils facilitent l’investigation et la correction selon le parcours utilisé ; ils ne garantissent pas l’annulation de tout effet externe. [Versionnement du workspace](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/workspace-versioning.md), [actions vérifiées](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/044-adaptive-recovery-for-deterministic-action-nodes.md).

**Une exploitation locale ou partagée.** Le même moteur est accessible par CLI, studio, application desktop et plateforme auto-hébergée. La plateforme documente la gestion des organisations et équipes, des identifiants, des quotas et de l’audit. Le choix du déploiement reste déterminant pour les garanties opérationnelles. [Plateforme Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md).

## 🤝 Comparer au-delà de la présence d’agents

Les agents IA, les validations humaines, le multi-modèle et MCP ne sont pas des exclusivités d’Iterion. Ces capacités sont aussi documentées chez n8n. Dify dispose d’un nœud de saisie humaine ; Flowise AgentFlow V2 documente les boucles, les checkpoints et la reprise après une attente humaine. [Fonctions IA de n8n](https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent), [Dify Human Input](https://docs.dify.ai/en/cloud/use-dify/nodes/human-input), [Flowise AgentFlow V2](https://docs.flowiseai.com/using-flowise/agentflowv2).

LangGraph et CrewAI Flows documentent également la persistance de l’état. L’enjeu pour le client est donc de vérifier **quelle unité de travail est sauvegardée, comment elle reprend et quels effets externes peuvent être répétés**. [Persistance LangGraph](https://docs.langchain.com/oss/python/langgraph/persistence), [persistance CrewAI](https://docs.crewai.com/en/concepts/flows#flow-persistence).

## 🧭 Vérifier qu’Iterion correspond à votre projet

- **Vous avez surtout besoin de connecteurs applicatifs.** Le catalogue généraliste d’Iterion est encore en projet. Vérifiez les actions et événements nécessaires : une mission d’agent doit justifier le travail d’intégration.
- **Vous voulez surtout un assistant documentaire.** Vérifiez la couverture de vos sources, de la recherche et de l’interface de dialogue. L’intérêt d’Iterion est plus direct lorsque l’assistant doit aussi exécuter des missions et produire des livrables.
- **Vous avez déjà une application d’agents.** Évaluez ce que les fonctions intégrées d’Iterion apporteraient face au coût de migration. Un runtime déjà maîtrisé peut rester adapté à votre architecture.
- **Vous exigez un engagement de support et de disponibilité.** Le service managé Iterion peut être étudié sur demande ; son périmètre et ses engagements restent à définir. Le statut expérimental et l’absence de SLA standard doivent entrer dans votre décision. [Statut Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/README.md).

## 💰 Coût et liberté de déploiement

Le code d’Iterion est sous licence MIT. Cela ne supprime ni le coût des modèles, ni celui de l’infrastructure, de l’intégration et de la maintenance. **Service managé sur demande — à étudier** : le périmètre, les conditions et le tarif seront définis avec le client. [Licence Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE).

Les unités de facturation diffèrent : les offres payantes n8n sont présentées par exécutions de workflows ; Make utilise des crédits, avec des règles particulières pour certaines fonctions ; Zapier compte des tâches dont la consommation peut varier selon l’action. Ces unités ne se comparent pas directement. [Unités de facturation relevées dans l’étude](methode-et-sources.md#conditions-offre).

Pour comparer le coût réel, retenir une mission et compter : abonnement ou licence, modèles, infrastructure, intégration, exploitation et temps humain de validation. Le critère utile est **le coût d’un résultat accepté**, avec un niveau de qualité défini.

L’auto-hébergement ne signifie pas non plus que les licences sont identiques. n8n utilise notamment la Sustainable Use License ; Activepieces distingue un socle MIT et des répertoires Enterprise ; Dify ajoute des conditions à Apache 2.0, notamment pour le multi-tenant et sa marque. Le périmètre précis importe si vous souhaitez distribuer une solution ou l’intégrer dans une offre client. [Licence n8n](https://raw.githubusercontent.com/n8n-io/n8n/master/LICENSE.md), [licence Activepieces](https://raw.githubusercontent.com/activepieces/activepieces/main/LICENSE), [licence Dify](https://raw.githubusercontent.com/langgenius/dify/main/LICENSE).

## 🚀 Évaluer Iterion sur votre projet

Une équipe veut qu’à chaque demande de correction, un agent analyse le dépôt, propose un changement, exécute les tests, le fasse relire, corrige les remarques puis soumette le résultat à un humain.

**Essayez Iterion sur cette mission complète.** Un pilote doit montrer le résultat Git, les preuves de test, le coût, les interventions humaines et la reprise après une interruption. Il ne suffit pas que le graphe termine : le changement doit être acceptable pour l’équipe.

Si cette mission s’inscrit dans un processus CRM ou support déjà piloté par n8n, les deux outils peuvent aussi être complémentaires : n8n reçoit et distribue les événements métier ; une intégration appelle Iterion pour la mission spécialisée. C’est une architecture envisageable à intégrer et tester, pas un connecteur prêt à l’emploi établi par cette étude. [API et webhooks Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/webhooks.md).

<div class="comparison-cta">

**Passez du comparatif à une mission concrète.**

[Démarrer avec Iterion](../quickstart.md) · [Explorer les bots disponibles](../examples.md) · [Découvrir l’éditeur visuel](../visual-editor.md)

</div>

**Iterion transforme vos méthodes de travail avec des agents IA en workflows versionnés, supervisables et réutilisables, du lancement de la mission à la validation de son résultat.**

---

Périmètre : documentation officielle des alternatives consultée le 11 septembre 2026 et dépôt Iterion au commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b`. Aucun benchmark concurrentiel exécuté. Les recommandations expriment une adéquation probable aux usages, à confirmer sur le cas du client. Le catalogue élargi de connecteurs Iterion reste une proposition de développement, suivie par le [ticket #1072](https://github.com/SocialGouv/iterion/issues/1072) ; il n’est pas compté parmi les capacités disponibles.

</div>
