---
title: "Matrice de fonctionnalités"
description: "Comparez vingt fonctionnalités de dix solutions, avec les conditions, intégrations et sources."
aside: false
pageClass: "comparison-page comparison-matrix"
---

<div lang="fr">

# 🧭 Iterion et alternatives — matrice de fonctionnalités

**🏢 10 solutions · ✅ 20 fonctionnalités en synthèse · 🗂 4 thèmes détaillés.**

État documentaire au 11 septembre 2026.

📚 **Périmètre Iterion approfondi :** [139 critères dans 16 familles, avec sources, limites et 35 bots recensés](inventaire-fonctionnalites-iterion.md). La grille commune ci-dessous conserve ses 20 critères ; elle ne résume pas à elle seule toutes les capacités d’Iterion.

### Accès rapide

[✅ Qui propose quoi ?](#disponibilite) · [🧩 Création & intégrations](#creation) · [🔄 Orchestration & budgets](#orchestration) · [🛠 Environnement technique](#technique) · [☁️ Déploiement & gouvernance](#deploiement)

[🎯 Aide au choix](index.md) · [⚖️ Iterion ou n8n](iterion-vs-n8n.md) · [📚 Sources](#sources)

[👁️ Supervision](inventaire-fonctionnalites-iterion.md#supervision) · [🙋 Interaction asynchrone](inventaire-fonctionnalites-iterion.md#humain) · [📚 Mémoire](inventaire-fonctionnalites-iterion.md#memoire) · [⏪ Reprise et fichiers](inventaire-fonctionnalites-iterion.md#reprise) · [🧩 Plugins](inventaire-fonctionnalites-iterion.md#extensions) · [⚡ Déclencheurs](inventaire-fonctionnalites-iterion.md#declenchement)

<a id="disponibilite"></a>

## ✅ Qui propose quoi ?

**Une fonctionnalité par ligne, un produit par colonne.** Cette grille résume la disponibilité ; les liens sur les fonctionnalités ouvrent les explications techniques plus bas.

**✅ Oui** · **❌ Non dans le périmètre étudié** · **🟡 Sous conditions ou partiel** · **🛠 Intégration à développer**

« Oui » signifie que la fonction est fournie, avec sa configuration normale ; cela ne signifie pas qu’elle est gratuite ou équivalente dans tous les produits. Le jaune signale une condition déterminante, précisée sous le tableau. Le symbole 🛠 renvoie à un chemin d’intégration explicite : il ne signifie ni fonction native, ni solution déjà testée. Les réserves commerciales restent visibles en jaune.

<p class="comparison-scroll-hint">↔ Faites défiler la grille pour comparer les dix produits. Les intitulés restent visibles.</p>

<div class="comparison-table comparison-presence" role="region" aria-label="Matrice de fonctionnalités, vingt critères et dix produits" tabindex="0">

| Fonctionnalité | <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"><br>**Iterion** | <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"><br>**n8n** | <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"><br>**Make** | <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"><br>**Zapier** | <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"><br>**Activepieces** | <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"><br>**Dify** | <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="64"><br>**Flowise**<br>🗄 Archivé | <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="76"><br>**LangGraph** | <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"><br>**CrewAI** | <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"><br>**Windmill** |
|:---| :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| 🎨 [Construire visuellement, sans coder le graphe](#creation) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌¹ | 🟡¹ | ✅ |
| 📄 [Définition du workflow disponible en fichier](#creation) | ✅ | ✅ | ✅ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| ⌨️ [Exécuter du code dans le workflow](#creation) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🤖 [Agents IA capables d’appeler des outils](#creation) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🔌 [Intégration MCP, client ou serveur](#creation) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡¹ | 🟡¹ | ✅ |
| 🙋 [Faire intervenir un humain avant de poursuivre](#orchestration) | ✅ | ✅ | 🛠⁸ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 💾 [Persister une attente humaine et la reprendre](#orchestration) | ✅ | ✅ | 🛠⁸ | 🟡² | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 🔀 [Branches et boucles de workflow](#orchestration) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 💰 [Budget de tokens à l’échelle du run](#orchestration) | 🟡³ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ |
| 💵 [Budget de coût IA estimé à l’échelle du run](#orchestration) | 🟡³ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ | 🛠⁹ |
| 🌿 [Créer un worktree Git pour la mission](#technique) | ✅ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ |
| 🔀 [Finaliser le résultat Git avec une politique de merge](#technique) | ✅ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ |
| 🛡 [Isoler l’exécution de code dans une sandbox](#technique) | 🟡⁴ | 🟡⁴ | 🟡⁴ | 🟡⁴ | 🟡⁴ | ✅⁴ | 🟡⁴ | 🛠 | 🟡⁴ | 🟡⁴ |
| 📁 [Sandbox d’agent avec shell et fichiers de projet](#technique) | 🟡⁴ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🛠¹⁰ | 🟡⁴ | ✅⁴ |
| ⚙️ [Distribuer les exécutions sur ses propres workers](#technique) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | 🟡¹¹ | ✅ | 🟡¹ | 🟡¹¹ | ✅ |
| 🏠 [Auto-héberger le moteur de workflows](#deploiement) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| ☁️ [Hébergement managé : offre ou étude sur demande](#deploiement) | 🟡¹² | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡⁶ | 🟡¹ | 🟡¹ | ✅ |
| 👥 [SSO pour l’accès de l’équipe](#deploiement) | ✅ | 🟡² | 🟡² | 🟡² | 🟡² | 🟡² | 🟡² | 🟡¹ | 🟡² | 🟡² |
| 📖 [Code source du moteur consultable publiquement](#deploiement) | ✅ | ✅ | ❌⁵ | ❌⁵ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 📜 [Socle moteur sous MIT ou Apache 2.0 non modifiée](#deploiement) | ✅ | ❌⁷ | ❌⁵ | ❌⁵ | ✅⁷ | ❌⁷ | ✅⁷ | ✅ | ✅ | ❌⁷ |

</div>


<details class="comparison-conditions">
<summary>📋 Conditions, périmètres et limites des cases</summary>


1. **Frameworks et composants associés.** LangGraph définit le graphe en code ; LangSmith Studio sert à le visualiser et le déboguer. Le « non » concerne donc la construction native sans code, pas la visualisation. CrewAI propose bien un Studio visuel dans sa plateforme, y compris en formule Basic gratuite ; il est distinct du framework Python. Les services managés, l’authentification et les adaptateurs MCP des frameworks passent par leurs composants associés. [L2] [L3] [L4] [L7] [C2] [C9]
2. **Offres et édition.** Zapier propose le contrôle humain dès Professional, avec délai d’expiration configurable ; son export/import JSON est réservé à Team/Enterprise. Le SSO de Dify et Flowise est Enterprise, comme plusieurs contrôles des autres produits. Ces restrictions d’édition sont incluses dans les cases jaunes. [Z6] [Z8] [Z5] [N3] [M1] [A8] [D7] [F6] [C9] [W9]
3. **Budgets Iterion.** La limite porte sur le run concerné, sans agréger les sous-bots ; une marge de finalisation de 10 % est prévue par défaut. Le coût est estimé. Une limite par appel LLM ou par agent n’est pas automatiquement un budget cumulé du run. Pour les autres produits, voir le montage proposé en note 9. [I1] [W2] [C5]
4. **Périmètre de la sandbox.** n8n : runners Code ; Make : sandbox JS/Python sur offres payantes ; Zapier : scripts isolés avec limites de durée/mémoire ; Dify : nœud Code restreint ; Flowise : interpréteur E2B associé. Cela ne vaut pas, à soi seul, environnement d’agent avec shell et dépôt. Iterion dépend du backend, Activepieces du mode, CrewAI de services associés et Windmill de sa configuration. [I4] [N5] [M8] [Z7] [A5] [D3] [F7] [C6] [W5] [W6]
5. **Moteurs SaaS Make et Zapier.** Les « non » portent sur leurs moteurs commerciaux étudiés, pas sur leurs SDK ou connecteurs. L’agent on-premise de Make connecte le SaaS au réseau local ; il ne constitue pas un moteur de workflows autonome. Pour Make, l’absence d’auto-hébergement est une conclusion tirée de ce périmètre documentaire. Zapier indique explicitement ne pas proposer de version on-premise. [M1] [M5] [Z4]
6. **Flowise : fin de vie.** Les capacités décrivent le logiciel documenté. Le dépôt est archivé depuis le 13 août 2026 et la fin des opérations/support de l’équipe est annoncée au 31 août 2026. Le site affiche encore une offre Cloud : 🟡 indique cette offre historique et cette réserve majeure, sans certifier que de nouveaux contrats sont acceptés. [F3] [F9]
7. **Licences.** n8n utilise la Sustainable Use License, Dify une licence Apache modifiée, Windmill un régime mêlant AGPL et Apache selon les fichiers. Pour Activepieces et Flowise, le « oui » porte sur le socle communautaire, hors parties Enterprise. La disponibilité du code source et une licence permissive sont deux critères distincts. [N7] [D6] [W8] [A7] [F4]

8. **Attente humaine Make.** Montage proposé : enregistrer l’état dans un Data Store, demander la décision, puis déclencher un second scénario par webhook. Les briques sont documentées ; ce montage applicatif n’est pas une suspension native de la même exécution. [M9] [M10]
9. **Budget cumulé à intégrer.** 🛠 désigne ici un contrôleur de budget à développer : appels IA via une API/outils personnalisés, identifiant de run partagé, cumul des usages et refus des appels suivants. Ce parcours remplace au besoin une étape IA intégrée. Les sources prouvent les points d’extension ; la comptabilité, le blocage, les appels parallèles et les reprises restent du travail d’intégration, non testé dans cette étude. Zapier dispose par ailleurs d’une pause automatique au-delà de 75 tâches par run : c’est un garde-fou en tâches, pas en tokens/USD. [N10] [M12] [Z9] [Z10] [A9] [A10] [D9] [D10] [F8] [L5] [C10] [W1]
10. **Git et sandbox de projet à intégrer.** 🛠 renvoie à des scripts Git et/ou à un service d’exécution externe, raccordés par commande, HTTP ou outil personnalisé. Il faut y implémenter la création du worktree, l’isolation shell/fichiers, les tests et la politique de merge. La présence d’un connecteur Git ne fournit pas ce parcours complet. Les points d’extension par produit sont précisés dans les détails et dans la [qualification des cases](methode-et-sources.md). [N9] [N10] [M11] [M12] [Z10] [A9] [D9] [F8] [L5] [C1] [W1] [W5]
11. **Workers selon le parcours.** Dify documente des workers Celery pour l’exécution en streaming et les reprises ; les workflows non streaming restent dans le processus API dans la version décrite. CrewAI documente une plateforme privée Enterprise avec workers répliqués ; cette exploitation est distincte de la seule bibliothèque Python. [D8] [C11]
12. **Iterion managé.** Service managé sur demande — à étudier. L’auto-hébergement est disponible ; faisabilité, périmètre et conditions de l’exploitation managée seront définis avec le client. Les conditions de service et de support sont à définir lors de cette étude.

</details>

**Sources des cases :** les quatre tableaux détaillés ci-dessous donnent les capacités retenues et leurs références officielles pour chaque produit. Aucun « non » n’est déduit d’une simple absence de mention. Les chemins 🛠 sont des propositions d’architecture fondées sur les extensions documentées, pas des fonctions certifiées de l’éditeur. [Méthode et preuves des statuts](methode-et-sources.md).

---

### Lire les détails par produit

<div class="comparison-table" role="region" aria-label="Matrice de fonctionnalités — tableau 2" tabindex="0">

| ✅ Natif | 🔌 Extension | 🛠 Intégration | 🏷 Offre |
|---|---|---|---|
| Fourni par le produit ; configuration possible. | Composant associé. | Montage à développer sur des points d’extension documentés. | Selon l’édition ou le contrat. |

</div>


Dans les tableaux détaillés suivants, les produits sont en lignes ; chaque colonne pose la même question aux dix solutions. Les repères précisent une fonction lorsqu’ils s’appliquent ; une cellule peut cumuler plusieurs conditions.

> **À garder en tête :** une fonction documentée et une intégration proposée ne prouvent ni la même couverture, ni le même effort, ni la même fiabilité. 🛠 ne tranche pas l’existence d’une éventuelle autre solution native : il décrit le chemin que cette étude peut étayer.

<a id="creation"></a>

## 🧩 1. Création, agents et intégrations

Construire le workflow, choisir les agents et connecter les outils.

<div class="comparison-table" role="region" aria-label="Matrice de fonctionnalités — tableau 3" tabindex="0">

| Solution | 🎨 Construction visuelle | 📄 Définition du workflow / portabilité | ⌨️ Code déterministe | 🤖 Agents et modèles | 🔌 Intégrations et MCP |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | ✅ Studio natif | Sources `.bot`, bundles `.botz` | Nœuds outils et `compute`, commandes | Plusieurs backends et modèles sélectionnables | Forges, plugins, MCP client et serveur ; catalogue élargi en projet [I1] [I2] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | ✅ Éditeur natif | Export JSON natif ; versionnement Git 🏷 selon offre | JavaScript / Python | Agents, multi-agent et choix de modèles | Catalogue applicatif ; MCP entrant et sortant [N1] [N2] [N3] [N8] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | ✅ Éditeur de scénarios | Blueprints exportables en JSON | Make Code : JavaScript / Python | Make AI Agents | Catalogue d’applications ; MCP client pour AI Agents et serveur Make [M1] [M2] [M3] [M6] [M7] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | ✅ Éditeur de Zaps | Export/import JSON des Zaps 🏷 Team/Enterprise [Z6] | Étapes JS/Python | AI by Zapier avec outils ; migration du produit Agents vers les Zaps | Catalogue applicatif et offre MCP [Z1] [Z7] [Z9] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | ✅ Éditeur de flows | Flows et releases de projet | Étapes TypeScript | Agents et fournisseurs IA | Pieces, connexions et MCP [A1] [A2] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | ✅ Studio natif | DSL exportable en YAML | Nœud Code Python / JavaScript | Nœuds LLM et Agent | Outils, plugins ; publication MCP documentée [D1] [D2] [D3] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archivé | ✅ Assistant, Chatflow, Agentflow | Export/import JSON ; API/SDK d’intégration [F5] | Fonction JavaScript personnalisée | Agents, modèles et bases vectorielles | Outils et MCP dans AgentFlow V2 [F1] [F2] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | 🔌 Extension LangSmith Studio pour visualiser et déboguer ; définition en code | Graphe défini dans le code applicatif | Fonctions du langage hôte | Orchestration native ; modèles/outils via code ou LangChain | Adaptation MCP via l’écosystème LangChain [L1] [L2] [L3] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Framework en code ; Studio visuel de plateforme, y compris Basic gratuit | Python et configuration des agents/tâches | Fonctions et outils Python | Agents spécialisés, Crews et Flows | Outils, MCP via `crewai-tools` [C1] [C2] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | ✅ Éditeur de flows et d’applications | Scripts, flows et définition YAML | Plusieurs langages, dont Python, TS, Go et Bash | Étapes AI Agent et agents imbriqués | Scripts/connecteurs ; MCP HTTP streamable pour les agents [W1] [W2] |

</div>


> 📦 **Portabilité et compatibilité entre moteurs.** Exporter une définition facilite sa sauvegarde et sa revue, mais ne permet pas de l’exécuter telle quelle dans un autre produit. Un export n’embarque pas nécessairement les secrets, les données ou toutes les dépendances. [D2] [M2]

<a id="orchestration"></a>

## 🔄 2. Orchestration et contrôle de l’exécution

Diriger les étapes, faire intervenir un humain, reprendre et encadrer la consommation.

<div class="comparison-table" role="region" aria-label="Matrice de fonctionnalités — tableau 4" tabindex="0">

| Solution | 🔀 Boucles et routage | 🙋 Intervention humaine | 💾 Persistance et reprise | 📐 Entrées / sorties structurées | 💰 Encadrement du budget IA |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Boucles bornées ; routage ; fan-out et convergence | Nœuds humains, réponses et pilotage des runs | Checkpoints ; reprise des états éligibles | Schémas typés, contraintes et validation | Tokens, coût estimé, durée, itérations ; hors sous-bots et avec marge de finalisation par défaut [I1] [I3] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Contrôle de flux et composition d’agents | Approbations des appels d’outils | Wait persiste les attentes ; ceci ne garantit pas la reprise de toute panne | Entrées/sorties structurées pour les étapes IA | Max Iterations par agent ; 🛠 compteur et blocage du run dans un service IA appelé par HTTP [N1] [N2] [N4] [N10] [N11] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Routeurs, filtres et itérations | 🛠 Data Store + décision + webhook | État métier conservé entre deux scénarios ; exécutions incomplètes et retries distincts | Entrées/sorties de scénario nommées et typées | Crédits de plateforme ; 🛠 budget du run via service IA appelé par HTTP [M1] [M4] [M9] [M10] [M12] [M13] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Paths, filtres et Looping | Human in the Loop 🏷 dès Professional | Attente jusqu’à décision/expiration ; replay d’erreurs distinct | Entrées typées et sorties structurées d’AI by Zapier | Pause au-delà de 75 tâches/run ; 🛠 budget tokens/USD via API IA externe [Z1] [Z2] [Z8] [Z9] [Z10] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Branches, boucles, sous-flows | Approbations et waitpoints | Waitpoints durables ; survivent au redémarrage du worker | Propriétés des actions ; 🛠 validation du contrat global dans une étape Code | Passerelles IA prises en charge ; 🛠 comptabilité et blocage par run à intégrer [A1] [A3] [A4] [A9] [A10] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | If/Else, Iteration et Loop | Nœud Human Input | Pause/reprise humaine persistée ; portée de reprise limitée aux mécanismes documentés | Sorties Code déclarées ; sorties LLM structurées selon modèle | Paramètres par modèle ; 🛠 compteur tokens/coût et blocage via plugin/API IA [D3] [D4] [D5] [D9] [D10] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archivé | Branches, état partagé et Loop avec maximum | Nœud Human Input ; approbation d’outils | Checkpoints des attentes humaines, reprise après redémarrage | État de flow et sorties LLM JSON avec schéma ; validation globale à composer | Limites de boucles/contexte ; 🛠 budget cumulé via outil/API IA [F2] [F8] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Cycles, routage conditionnel, branches parallèles | Interruptions et modification d’état | Checkpointers ; backend persistant nécessaire | Schéma d’état du graphe ; validation métier à concevoir | 🛠 Compteur partagé dans l’état et contrôle avant les appels IA [L1] [L4] [L5] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Flows à événements, routage et tâches | Saisie humaine et workflows de feedback | `@persist` et checkpoints configurables des Crews/Flows/Agents | Modèles d’état et sorties structurées selon configuration | `max_iter`, durée/cadence par agent ; 🛠 compteur commun et contrôle via hooks LLM [C1] [C3] [C4] [C5] [C10] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Flows avec branches, erreurs et étapes d’agents | Suspend / Approval ; formulaires 🏷 Cloud ou Enterprise | Suspension et retries d’étape ; état d’agent possible en volume | JSON Schema de sortie des AI Agents | Limites d’itérations et de complétion par agent ; 🛠 budget cumulé dans un script/API IA [W1] [W2] [W3] [W4] |

</div>


> 🔎 **Trois points à vérifier sur le cas client :** une branche dessinée ne prouve pas son exécution simultanée ; une attente persistée ne couvre pas nécessairement un crash au milieu d’un appel ; un schéma de données ne garantit pas la justesse du contenu produit.

> 💰 **Portée des budgets Iterion.** Le budget est un mécanisme natif, mais les sous-bots ont leurs propres budgets et leurs totaux ne sont pas agrégés au parent. Une marge de finalisation de 10 % est documentée par défaut, désactivable. Le coût demeure lié aux informations du backend. Cette case ne signifie donc pas « plafond de facture absolu pour tous les appels et tous les enfants ». [I1]

<a id="technique"></a>

## 🛠 3. Environnement technique et travail sur les dépôts

Isoler les exécutions, gérer Git, préparer les dépendances et suivre le travail.

<div class="comparison-table" role="region" aria-label="Matrice de fonctionnalités — tableau 5" tabindex="0">

| Solution | 🛡 Isolation du code / des agents | 🌿 Worktree Git et finalisation du résultat | 📦 Dépendances d’exécution | ⚙️ Exécution distribuée | 🔭 Observation technique |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Drivers Docker/Podman/Kubernetes ; comportement selon backend et lancement | Parcours natif `worktree: auto`, branche et politique de merge | Devbox bot + dépôt ; images de sandbox | Plateforme : NATS, runners, KEDA, MongoDB/S3 | Événements, artefacts, Prometheus, OTLP [I2] [I4] [I5] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Task runners externes pour Code ; 🛠 service externe pour shell et dépôt par mission [N10] | 🛠 Scripts Git via Execute Command en auto-hébergement, ou service HTTP [N9] [N10] | Image de runners extensible ; packages autorisés explicitement | Queue mode : Redis, base partagée et workers | Historique d’exécution ; métriques workers ; log streaming 🏷 selon offre [N3] [N5] [N6] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | Make Code : sandbox JS/Python 🏷 payant ; 🛠 service externe pour shell/dépôt | 🛠 Scripts Git sur hôte distant via SSH ou HTTP [M11] [M12] | Libs standard ; libs personnalisées 🏷 Enterprise [M8] | Moteur SaaS ; agent local de connexion distinct | Historique, recherche de logs et fonctions d’audit 🏷 selon offre [M1] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | Code isolé, durée/mémoire selon offre ; 🛠 service externe pour shell/dépôt | 🛠 API d’un exécuteur Git appelé par Webhooks [Z10] | Runtime Code ; toolchain Git dans le service externe [Z7] | Moteur SaaS, sans workers de moteur auto-hébergés | Zap History ; fonctions d’observabilité 🏷 selon offre [Z1] [Z2] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Modes V8 et/ou namespaces ; 🛠 service externe pour shell/dépôt | 🛠 Exécuteur Git/sandbox via une piece utilisant le client HTTP [A9] | Versions de pieces et dépendances ; npm dépend du mode sandbox | App, file Redis, workers et stockage | Runs, analytics et audit 🏷 selon offre [A1] [A5] [A6] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Sandbox du nœud Code ; 🛠 service externe pour shell/dépôt | 🛠 Exécuteur Git/sandbox joint par HTTP ou plugin [D9] | Bibliothèques préinstallées ; hors ensemble disponible, import refusé | Celery pour streaming et reprises ; non streaming dans l’API selon la version décrite [D8] | Logs, dashboard et intégrations de traces [D1] [D3] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archivé | Interpréteur E2B associé ; 🛠 shell/dépôt via service externe [F7] | 🛠 Outil personnalisé appelant un exécuteur Git/sandbox [F8] | Librairies accessibles au runtime JS ; configuration de l’hébergement | Message queue et workers documentés | Traces, analytics et évaluations [F1] [F2] [F3] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | À fournir par l’application ou une extension ; distinct du runtime de graphe | 🛠 Fonctions/outils Git et cycle de finalisation à écrire | Environnement et dépendances de l’application | 🔌 Extension Agent Server / déploiement LangSmith, ou exploitation propre | Streaming d’état ; LangSmith Studio, tracing et évaluations [L1] [L2] [L4] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Services de sandbox associés ; ancien CodeInterpreterTool déprécié | 🛠 Outils Python pour Git et cycle de finalisation à écrire | Environnement Python et outils choisis | Framework exploité par l’équipe ; plateforme privée Enterprise et workers répliqués [C11] | Événements et intégrations de traces ; console 🏷 selon offre [C1] [C6] [C7] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | NSJAIL et namespaces ; AI Sandbox avec volumes persistants | 🛠 Scripts Git et politique de merge dans les flows ; AI Sandbox pour les fichiers | Dépendances de scripts ; résolution et lockfile Python documentés | Flotte de workers, groupes et séparation des accès | Logs de jobs, état des flows et interfaces de suivi [W1] [W5] [W6] [W7] |

</div>


> 🛡 **Sandbox : comparer le périmètre exact.** Un runner isolant un nœud Python, une sandbox d’agent disposant d’un shell et un pod de plateforme ne protègent pas les mêmes ressources. n8n documente l’isolation des nœuds Code par runners externes ; Dify restreint notamment le système de fichiers, le réseau et les commandes de son nœud Code ; Windmill peut isoler des agents de programmation avec des volumes persistants. [N5] [D3] [W5]

Pour Iterion, `auto` peut se dégrader vers une exécution sans sandbox selon l’hôte ; une demande explicite a un comportement différent. Le parcours cloud décrit dans la référence sandbox utilise actuellement le pod runner comme frontière d’isolation. La délégation Codex ne prend pas en charge la sandbox externe d’Iterion. Ces limites doivent rester visibles lors d’une comparaison de sécurité. [I2] [I4]

<a id="deploiement"></a>

## ☁️ 4. Déploiement, exploitation et gouvernance

Choisir l’hébergement, les interfaces et les contrôles d’équipe.

<div class="comparison-table" role="region" aria-label="Matrice de fonctionnalités — tableau 6" tabindex="0">

| Solution | ☁️ Auto-hébergement / service managé | ⚡ API et déclenchement | 🌿 Git pour le cycle de vie des workflows | 👥 Gouvernance d’équipe | 📜 Licence / périmètre commercial |
|---|---|---|---|---|---|
| <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | Local et plateforme auto-hébergeable ; **service managé sur demande — à étudier** | CLI/SDK, API, cron, dispatcher, webhooks | `.bot` dans Git ; bundles et versions | Organisations/équipes, SSO, secrets liés, quotas et audit | MIT ; statut expérimental [I2] [I5] [I6] |
| <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** | Auto-hébergement et n8n Cloud | API, CLI, triggers et webhooks | Git/environnements 🏷 selon offre | SSO, rôles, secrets et audit 🏷 selon offre | Sustainable Use + Enterprise distinct [N1] [N3] [N7] |
| <img class="comparison-logo" src="./assets/logos/make.ico" alt="" width="24"> **Make** | SaaS ; agent local de connexion, moteur autonome non proposé dans l’offre étudiée | API et scénarios planifiés/déclenchés | Blueprints JSON ; 🛠 pipeline Git/CI avec la CLI Make [M14] | Équipes et fonctions Enterprise 🏷 selon offre | Service commercial ; quotas de crédits [M1] [M2] [M5] |
| <img class="comparison-logo" src="./assets/logos/zapier.ico" alt="" width="24"> **Zapier** | SaaS ; pas de version on-premise | Triggers, webhooks et outils développeur 🏷 selon offre | Export JSON 🏷 Team/Enterprise ; archivage Git et remise en production à organiser [Z6] | Espaces, connexions et contrôles 🏷 selon offre | Service commercial ; allocation de tâches [Z1] [Z4] |
| <img class="comparison-logo" src="./assets/logos/activepieces.svg" alt="" width="24"> **Activepieces** | Cloud et infrastructure propre | Triggers, webhooks et outils MCP | Git et promotion de releases si Environments activé dans le plan | Projets, contrôles d’accès, gestion de secrets et audit 🏷 selon offre | Socle MIT ; parties Enterprise distinctes [A1] [A2] [A7] |
| <img class="comparison-logo" src="./assets/logos/dify.svg" alt="" width="24"> **Dify** | Cloud, VPC et auto-hébergement | API d’application ; publication MCP | Export DSL YAML ; pipeline Git à organiser | Workspaces ; SSO, RBAC et audit 🏷 Enterprise [D7] | Apache 2.0 modifiée avec conditions supplémentaires [D1] [D2] [D6] |
| <img class="comparison-logo" src="./assets/logos/flowise.svg" alt="" width="88"> **Flowise**<br>🗄 Archivé | Auto-hébergement ; Cloud encore affiché, opérations de l’équipe en fin de vie [F9] | API, SDK et chat embarqué | Exports JSON versionnables ; 🛠 pipeline Git à organiser [F5] | Teams/workspaces ; SSO OIDC 🏷 Enterprise [F6] | Socle Apache 2.0 ; parties commerciales distinctes [F1] [F3] [F4] |
| <img class="comparison-logo" src="./assets/logos/langgraph.svg" alt="" width="105"> **LangGraph** | Bibliothèque exécutée chez soi ; déploiement associé via LangSmith | Appels du runtime ; API via Agent Server | Code applicatif versionné dans le Git de l’équipe | À fournir dans l’application ou via la plateforme associée | Runtime MIT ; conditions séparées des services [L1] [L2] [L4] [L6] |
| <img class="comparison-logo" src="./assets/logos/crewai.ico" alt="" width="24"> **CrewAI** | Framework exécuté chez soi ; plateforme cloud Basic/Enterprise | Appels Python ; déploiements et triggers via la plateforme | Code et configuration versionnés dans Git | Console de plateforme ; SSO et RBAC en Enterprise | Framework MIT ; plateforme distincte [C1] [C7] [C8] [C9] |
| <img class="comparison-logo" src="./assets/logos/windmill.svg" alt="" width="24"> **Windmill** | Auto-hébergeable ; offre cloud | API, webhooks, scheduler et CLI | Intégration Git pour scripts et flows | Permissions et groupes de workers ; fonctions avancées 🏷 selon édition | AGPL/Apache selon fichiers ; Enterprise et binaires distribués à distinguer [W1] [W6] [W8] |

</div>


> 🌿 **Deux usages distincts de Git.** Le premier sert à livrer et relire l’automatisation elle-même. Le second sert à isoler les modifications qu’un agent apporte à un dépôt, protéger le résultat et décider comment l’intégrer. La présence d’une intégration GitHub ou d’un export Git ne suffit pas à établir ce second parcours.

**LangGraph et CrewAI sont comparés comme frameworks**, avec leurs plateformes associées explicitement nommées. Les fonctions d’un service managé ne sont pas attribuées automatiquement à la bibliothèque libre. De même, les fonctions Enterprise des autres produits ne sont pas attribuées à leurs éditions communautaires.

> 🗄 **Flowise : fin de vie annoncée au 31 août 2026.** Le dépôt est archivé depuis le 13 août. Les fonctions du logiciel restent documentées ; le Cloud toujours affiché sur le site ne constitue pas une garantie de service ou de support actuel. [Annonce officielle][F9].

## 🎯 La valeur du parcours intégré d’Iterion

Iterion réunit **workflow déclaratif, agents sur dépôt, worktrees, finalisation, budgets et pilotage des exécutions**. C’est cette combinaison qu’un pilote permet d’évaluer sur votre projet. Les fonctions prises séparément ont souvent des équivalents ailleurs : la comparaison utile porte sur le travail d’intégration restant, les conditions d’exécution et la qualité du résultat accepté.

Cette matrice est documentaire : pas de score global, pas de classement obtenu en comptant les cases, pas de benchmark exécuté. Chaque case décrit une capacité documentée, une condition explicite ou un chemin d’intégration argumenté. Les montages 🛠 doivent être réalisés et testés avant toute promesse client. Pour le coût, conserver le critère du comparatif : coût total d’un résultat accepté, incluant modèles, infrastructure et temps humain.

<div class="comparison-cta">

**Évaluez ce parcours sur une mission réelle.**

[Démarrer avec Iterion](../quickstart.md) · [Explorer les bots disponibles](../examples.md) · [Préparer votre pilote](index.md#🚀-evaluer-iterion-sur-votre-projet)

</div>

<a id="sources"></a>

## 📚 Sources officielles

Les logos et icônes proviennent des produits concernés : [provenance des visuels](assets/logos/README.md).

Les références Iterion sont figées sur le commit `bbc1ddc7844b81582b6dfc4d0b7f381680bb284b`, comme le comparatif prospects. Les autres pages sont celles consultées le 11 septembre 2026. La version CrewAI affichée par sa documentation est `v1.15.21` ; la page CodeInterpreterTool annonce sa dépréciation tout en conservant des exemples historiques, qui ne sont pas repris comme capacité actuelle.

- **Iterion** : [I1 — DSL][I1] ; [I2 — état courant][I2] ; [I3 — reprise][I3] ; [I4 — sandbox][I4] ; [I5 — plateforme][I5] ; [I6 — licence][I6]. Détails complémentaires : [worktrees et merge](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md), [observabilité](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/observability/README.md), [catalogue en projet](https://github.com/SocialGouv/iterion/issues/1072).
- **n8n** : [N1 — documentation][N1] ; [N2 — agents et contrôle][N2] ; [N3 — conditions relevées][N3] ; [N4 — Wait][N4] ; [N5 — task runners][N5] ; [N6 — queue mode][N6] ; [N7 — licence][N7].
- **Make** : [M1 — conditions relevées][M1] ; [M2 — blueprints][M2] ; [M3 — AI Agents][M3] ; [M4 — exécutions incomplètes][M4].
- **Zapier** : [Z1 — conditions relevées][Z1] ; [Z2 — replay][Z2] ; [Z3 — Human in the Loop][Z3].
- **Activepieces** : [A1 — documentation][A1] ; [A2 — releases et Git][A2] ; [A3 — flow control][A3] ; [A4 — outils MCP et structure des flows][A4] ; [A5 — sandboxing][A5] ; [A6 — architecture][A6] ; [A7 — licence][A7].
- **Dify** : [D1 — documentation][D1] ; [D2 — applications et DSL][D2] ; [D3 — Code][D3] ; [D4 — Human Input][D4] ; [D5 — LLM][D5] ; [D6 — licence][D6].
- **Flowise** : [F1 — introduction][F1] ; [F2 — AgentFlow V2][F2] ; [F3 — relevé Cloud][F3] ; [F4 — licence][F4].
- **LangGraph** : [L1 — runtime][L1] ; [L2 — LangSmith Studio][L2] ; [L3 — adaptation MCP][L3] ; [L4 — persistance et Agent Server][L4] ; [L5 — Graph API][L5] ; [L6 — licence][L6].
- **CrewAI** : [C1 — concepts][C1] ; [C2 — MCP][C2] ; [C3 — checkpoints][C3] ; [C4 — contrôle humain][C4] ; [C5 — agents][C5] ; [C6 — dépréciation CodeInterpreterTool][C6] ; [C7 — framework et plateforme][C7] ; [C8 — licence][C8].
- **Windmill** : [W1 — plateforme][W1] ; [W2 — AI Agents][W2] ; [W3 — approbations][W3] ; [W4 — retries][W4] ; [W5 — AI Sandbox][W5] ; [W6 — isolation][W6] ; [W7 — dépendances Python][W7] ; [W8 — licences][W8].

**Vérifications complémentaires de la grille :** [n8n — export JSON][N8] ; [Make — agent on-premise][M5] ; [Zapier — réponse du support sur l’on-premise][Z4] ; [Zapier — configuration SSO][Z5] ; [Activepieces — SSO][A8] ; [LangSmith — authentification][L7] ; [CrewAI — conditions du Studio][C9] ; [Windmill — SSO][W9].

[I1]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md
[I2]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/current-state.md
[I3]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md
[I4]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/sandbox.md
[I5]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md
[I6]: https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE
[N1]: https://docs.n8n.io/
[N2]: https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent
[N3]: methode-et-sources.md#conditions-n8n
[N4]: https://raw.githubusercontent.com/n8n-io/n8n-docs/main/docs/integrations/builtin/core-nodes/n8n-nodes-base.wait.md
[N5]: https://docs.n8n.io/deploy/host-n8n/configure-n8n/set-up-task-runners
[N6]: https://docs.n8n.io/deploy/host-n8n/configure-n8n/scaling/enable-queue-mode
[N7]: https://raw.githubusercontent.com/n8n-io/n8n/master/LICENSE.md
[M1]: methode-et-sources.md#conditions-make
[M2]: https://help.make.com/blueprints
[M3]: https://help.make.com/make-ai-agent-new
[M4]: https://help.make.com/incomplete-executions
[Z1]: methode-et-sources.md#conditions-zapier
[Z2]: https://help.zapier.com/hc/en-us/articles/19220226086797-What-is-replay
[Z3]: https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop
[A1]: https://www.activepieces.com/docs/getting-started/introduction
[A2]: https://www.activepieces.com/docs/admin-guide/guides/project-releases
[A3]: https://www.activepieces.com/docs/build-pieces/piece-reference/flow-control
[A4]: https://www.activepieces.com/docs/mcp/tools
[A5]: https://www.activepieces.com/docs/install/architecture/sandboxing
[A6]: https://www.activepieces.com/docs/install/architecture/overview
[A7]: https://raw.githubusercontent.com/activepieces/activepieces/main/LICENSE
[D1]: https://docs.dify.ai/en/home
[D2]: https://docs.dify.ai/en/cloud/use-dify/workspace/app-management
[D3]: https://docs.dify.ai/en/cloud/use-dify/nodes/code
[D4]: https://docs.dify.ai/en/cloud/use-dify/nodes/human-input
[D5]: https://docs.dify.ai/en/cloud/use-dify/nodes/llm
[D6]: https://raw.githubusercontent.com/langgenius/dify/main/LICENSE
[F1]: https://docs.flowiseai.com/
[F2]: https://docs.flowiseai.com/using-flowise/agentflowv2
[F3]: methode-et-sources.md#conditions-flowise
[F4]: https://raw.githubusercontent.com/FlowiseAI/Flowise/main/LICENSE.md
[L1]: https://docs.langchain.com/oss/python/langgraph/overview
[L2]: https://docs.langchain.com/langsmith/studio
[L3]: https://docs.langchain.com/oss/python/langchain/mcp
[L4]: https://docs.langchain.com/oss/python/langgraph/persistence
[L5]: https://docs.langchain.com/oss/python/langgraph/graph-api
[L6]: https://raw.githubusercontent.com/langchain-ai/langgraph/main/LICENSE
[C1]: https://docs.crewai.com/core-concepts/Agents
[C2]: https://docs.crewai.com/v1.15.21/en/mcp/overview
[C3]: https://docs.crewai.com/v1.15.21/en/concepts/checkpointing
[C4]: https://docs.crewai.com/v1.15.21/en/learn/human-in-the-loop
[C5]: https://docs.crewai.com/v1.15.21/en/concepts/agents
[C6]: https://docs.crewai.com/v1.15.21/en/tools/ai-ml/codeinterpretertool
[C7]: https://docs.crewai.com/
[C8]: https://raw.githubusercontent.com/crewAIInc/crewAI/main/LICENSE
[W1]: https://www.windmill.dev/docs/intro
[W2]: https://www.windmill.dev/docs/core_concepts/ai_agents
[W3]: https://www.windmill.dev/docs/flows/flow_approval
[W4]: https://www.windmill.dev/docs/flows/retries
[W5]: https://www.windmill.dev/docs/core_concepts/ai_sandbox
[W6]: https://www.windmill.dev/docs/advanced/security_isolation
[W7]: https://www.windmill.dev/docs/advanced/dependencies_in_python
[W8]: https://raw.githubusercontent.com/windmill-labs/windmill/main/LICENSE

[N8]: https://docs.n8n.io/build/manage-workflows/export-and-import
[M5]: https://help.make.com/on-premise-agent
[Z4]: https://community.zapier.com/how-do-i-3/is-zapier-only-cloud-base-or-on-premise-too-18528
[Z5]: https://help.zapier.com/hc/en-us/articles/8496279747085-Set-up-single-sign-on-with-SAML
[A8]: methode-et-sources.md#conditions-activepieces
[L7]: https://docs.langchain.com/langsmith/authentication-methods
[C9]: methode-et-sources.md#conditions-crewai
[W9]: https://www.windmill.dev/docs/enterprise/onboarding

[N9]: https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executecommand
[N10]: https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.httprequest
[N11]: https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent
[M6]: https://help.make.com/make-ai-agents-new-mcp-tools-are-now-available
[M7]: https://help.make.com/introduction-to-mcp
[M8]: https://help.make.com/the-make-code-app-is-available
[M9]: https://help.make.com/data-stores
[M10]: https://help.make.com/webhooks
[M11]: https://apps.make.com/ssh
[M12]: https://apps.make.com/http
[M13]: https://help.make.com/scenario-inputs-and-outputs/
[M14]: https://help.make.com/the-make-cli-is-now-live
[Z6]: https://help.zapier.com/hc/en-us/articles/8496308481933-Import-and-export-Zap-workflows-in-your-Team-or-Enterprise-account
[Z7]: https://help.zapier.com/hc/en-us/articles/8496310939021-Use-JavaScript-code-in-Zap-workflows
[Z8]: https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop
[Z9]: https://help.zapier.com/hc/en-us/articles/47402591569805-Migrating-from-Agents-to-AI-by-Zapier
[Z10]: https://help.zapier.com/hc/en-us/articles/8496326446989-Send-webhooks-in-Zap-workflows
[A9]: https://github.com/activepieces/activepieces/blob/main/.agents/skills/piece-builder/common-patterns.md
[A10]: https://www.activepieces.com/docs/admin-guide/guides/setup-ai-providers
[D7]: methode-et-sources.md#conditions-dify
[D8]: https://github.com/langgenius/dify/discussions/32245
[D9]: https://docs.dify.ai/en/cloud/use-dify/nodes/http-request
[D10]: https://docs.dify.ai/en/develop-plugin/features-and-specs/plugin-types/model-schema
[F5]: https://docs.flowiseai.com/migration-guide/cloud-migration
[F6]: https://docs.flowiseai.com/configuration/sso
[F7]: https://docs.flowiseai.com/integrations/langchain/tools/python-interpreter
[F8]: https://docs.flowiseai.com/integrations/langchain/tools/custom-tool
[F9]: https://github.com/FlowiseAI/Flowise/discussions/6727
[C10]: https://docs.crewai.com/v1.15.21/en/learn/llm-hooks
[C11]: https://enterprise-docs.crewai.com/reference/chart-values/worker

</div>
