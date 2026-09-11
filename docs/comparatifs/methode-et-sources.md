---
title: "Méthode et sources"
description: "Périmètre des capacités documentées et des intégrations proposées dans le comparatif."
aside: false
pageClass: "comparison-page"
---

<div lang="fr">

# 🔎 Méthode et sources du comparatif

11 septembre 2026 — accompagne la [matrice de fonctionnalités](matrice-fonctionnalites.md).

La matrice compare les capacités documentées des produits, leurs conditions d’accès et les chemins d’intégration que leurs interfaces permettent de concevoir. Les sources officielles sont datées du 11 septembre 2026. Aucun essai concurrent n’a été exécuté.

Cette distinction est essentielle : une case 🛠 décrit une façon de réaliser la fonction à partir d’extensions documentées. Elle **ne prouve pas l’absence d’une autre solution native**, et ne signifie pas qu’un montage complet a été développé ou validé. Les conditions de déploiement et d’offre restent explicites.

## Exemples de capacités confirmées

<div class="comparison-table" role="region" aria-label="Méthode et sources — tableau 1" tabindex="0">

| Produit et critère | Statut | Preuve et périmètre |
|---|---|---|
| Zapier — fichier de workflow | 🟡 | Import/export JSON en Team et Enterprise. Les comptes Free/Professional ne disposent pas de cet outil. [Guide officiel](https://help.zapier.com/hc/en-us/articles/8496308481933-Import-and-export-Zap-workflows-in-your-Team-or-Enterprise-account). |
| Flowise — fichier de workflow | ✅ | Le guide de migration décrit un export JSON puis son import ; les credentials sont traités séparément. [Migration Cloud](https://docs.flowiseai.com/migration-guide/cloud-migration). |
| Make — MCP | ✅ | AI Agents accepte des serveurs MCP ; Make fournit aussi son propre serveur. [Client](https://help.make.com/make-ai-agents-new-mcp-tools-are-now-available), [serveur](https://help.make.com/introduction-to-mcp). |
| Zapier — attente humaine persistée | 🟡 | Request Approval met le run en attente d’une décision, avec expiration et historique ; dès Professional. Cela n’établit pas une reprise universelle après toute panne. [Human in the Loop](https://help.zapier.com/hc/en-us/articles/38731463206029-Request-approval-to-keep-your-workflow-running-with-Human-in-the-Loop). |
| Make — sandbox de code | 🟡 | Make Code isole JS/Python ; offres payantes, ressources limitées ; bibliothèques personnalisées en Enterprise. [Make Code](https://help.make.com/the-make-code-app-is-available). |
| Zapier — sandbox de code | 🟡 | Le runtime JavaScript est isolé et soumis à des limites de durée/mémoire dépendant de l’offre. [Code by Zapier](https://help.zapier.com/hc/en-us/articles/8496310939021-Use-JavaScript-code-in-Zap-workflows). |
| Flowise — sandbox de code | 🟡 | Code Interpreter appelle une sandbox E2B associée. Ce n’est pas la preuve d’un shell de projet intégré dans Flowise. [Outil E2B](https://docs.flowiseai.com/integrations/langchain/tools/python-interpreter). |
| Dify — workers propres | 🟡 | La version 1.13 décrit Celery pour les workflows en streaming et les reprises. Le non streaming demeure dans l’API dans ce périmètre. [Annonce des mainteneurs](https://github.com/langgenius/dify/discussions/32245). |
| CrewAI — workers propres | 🟡 | Le chart de plateforme privée configure les réplicas des workers et leur placement ; périmètre Enterprise, distinct du framework seul. [Configuration des workers](https://enterprise-docs.crewai.com/reference/chart-values/worker). |
| Dify — SSO | 🟡 | SSO, RBAC et audit présentés dans l’offre Enterprise. [Conditions d’accès relevées](#conditions-dify). |
| Flowise — SSO | 🟡 | OIDC documenté en Enterprise ; la réserve de fin de vie du produit s’applique aussi à cette fonction. [SSO](https://docs.flowiseai.com/configuration/sso). |

</div>


## Chemins d’intégration proposés

Le raisonnement est architectural : les sources ci-dessous établissent la capacité à appeler du code, une API ou un outil. Le travail nécessaire pour obtenir la propriété métier reste décrit comme **à développer**.

<div class="comparison-table" role="region" aria-label="Méthode et sources — tableau 2" tabindex="0">

| Critère | Produits concernés | Montage proposé et limite |
|---|---|---|
| Intervention humaine / conservation de l’attente | Make | Data Store pour l’état métier, notification et décision, webhook pour démarrer la continuation. Deux scénarios peuvent porter une même mission ; cela ne suspend pas nativement le même run. Authentification de la réponse, expiration et déduplication sont à concevoir. [Data Stores](https://help.make.com/data-stores), [Webhooks](https://help.make.com/webhooks). |
| Budget cumulé en tokens / en coût IA estimé | n8n, Make, Zapier, Activepieces, Dify, Flowise, CrewAI, Windmill | Faire passer les appels IA concernés par un contrôleur commun, propager un identifiant de run, cumuler les usages et refuser les appels suivants. Les tarifs, les usages retournés, les appels simultanés et la reprise doivent être pris en compte. Une étape IA intégrée peut devoir être remplacée. Un contrôle placé uniquement après une étape agent ne contrôle pas sa boucle interne. |
| Worktree / politique de merge du résultat | n8n, Make, Zapier, Activepieces, Dify, Flowise, Windmill | Exécuteur Git à développer : création du worktree, branche, commandes/tests, collecte du résultat et intégration selon la politique choisie. Les scripts ou APIs ci-dessous sont les points de raccordement ; un simple export de workflow dans Git ne réalise pas ce parcours. |
| Sandbox d’agent avec shell et fichiers | n8n, Make, Zapier, Activepieces, Dify, Flowise | Un service externe crée la sandbox, expose les commandes et les fichiers de la mission, puis gère la fin de vie et la récupération. L’isolation reste une responsabilité de ce service ; HTTP, SSH ou un nœud Code ne la procurent pas à eux seuls. |

</div>


### Points de raccordement par produit

<div class="comparison-table" role="region" aria-label="Méthode et sources — tableau 3" tabindex="0">

| Produit | Briques documentées | Responsabilité de l’intégration |
|---|---|---|
| n8n | [Execute Command](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executecommand) et [HTTP Request](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.httprequest), également utilisable comme outil d’agent | Commandes Git en auto-hébergement, ou exécuteur distant. Execute Command est désactivé par défaut depuis n8n 2.0 et absent de n8n Cloud. Pour les budgets, le service IA appelé par HTTP porte le compteur et le refus. |
| Make | [SSH](https://apps.make.com/ssh), [HTTP](https://apps.make.com/http), scénarios et [Data Stores](https://help.make.com/data-stores) | Exécuteur Git/sandbox par SSH ou HTTP ; contrôleur de budget côté service IA ; corrélation des scénarios pour l’humain. |
| Zapier | [Webhooks sortants](https://help.zapier.com/hc/en-us/articles/8496326446989-Send-webhooks-in-Zap-workflows) | API d’un service Git/sandbox ou IA contrôlé, à intégrer dans les Zaps. La durée d’une mission peut imposer un lancement asynchrone puis un retour de résultat. |
| Activepieces | [Client HTTP des pieces](https://github.com/activepieces/activepieces/blob/main/.agents/skills/piece-builder/common-patterns.md), [fournisseurs IA et passerelles](https://www.activepieces.com/docs/admin-guide/guides/setup-ai-providers) | API d’exécution et gestion des fichiers ; passerelle IA ou service dédié avec corrélation par run pour le budget. Le guide de passerelles ne promet pas, à lui seul, ce périmètre par run. |
| Dify | [HTTP Request](https://docs.dify.ai/en/cloud/use-dify/nodes/http-request), [interface de modèle et LLMUsage](https://docs.dify.ai/en/develop-plugin/features-and-specs/plugin-types/model-schema) | Outil/API d’exécution Git et shell ; plugin/service IA cumulant usages et coûts. Compter à la fin d’un workflow ne suffit pas à bloquer les appels pendant celui-ci. |
| Flowise | [Custom Tool](https://docs.flowiseai.com/integrations/langchain/tools/custom-tool) et [AgentFlow V2](https://docs.flowiseai.com/using-flowise/agentflowv2) | Outil qui appelle un exécuteur externe ; contrôle IA dans ce service. L’hébergement, la maintenance et les extensions doivent tenir compte de la fin de vie de Flowise. |
| CrewAI | [Hooks LLM](https://docs.crewai.com/v1.15.21/en/learn/llm-hooks) et outils Python | Hooks/adapter avec compteur commun ; traitement explicite du dépassement et de l’arrêt. L’existence du hook ne suffit pas à garantir un budget complet couvrant tous les chemins d’appel. Les outils Git sont également à développer. |
| Windmill | [Scripts et flows](https://www.windmill.dev/docs/intro), [AI Sandbox](https://www.windmill.dev/docs/core_concepts/ai_sandbox) | Scripts Git et contrôleur IA à écrire. La sandbox documentée fournit déjà les outils de programmation et les fichiers, mais ne définit pas la politique d’intégration Git propre au client. |
| LangGraph | [Graph API](https://docs.langchain.com/oss/python/langgraph/graph-api) et état partagé | Fonctions Git, compteur et contrôle des appels à développer dans le graphe. Les primitives du framework ne sont pas un produit livré pour ces usages. |

</div>


Les limites d’itérations, les plafonds de sortie par appel et les quotas de compte restent des fonctions utiles, mais ne deviennent pas des budgets cumulés de run. **Zapier documente désormais une pause automatique au-delà de 75 tâches par run** : elle est conservée dans les détails comme garde-fou distinct. [Migration vers AI by Zapier](https://help.zapier.com/hc/en-us/articles/47402591569805-Migrating-from-Agents-to-AI-by-Zapier).

## Hébergement — 2 situations précisées

- **Iterion : service managé sur demande — à étudier.** L’auto-hébergement est actuellement disponible. Le périmètre et les conditions d’un service managé peuvent être étudiés avec le client ; aucune offre standard ni aucun SLA ne sont revendiqués ici.
- **Flowise : offre Cloud encore affichée, fin de vie de l’équipe annoncée.** Le [relevé du site produit](#conditions-flowise) conserve la mention d’une offre Cloud ; l’[annonce des mainteneurs](https://github.com/FlowiseAI/Flowise/discussions/6727) annonce l’archivage le 13 août et la fin des opérations le 31 août 2026. La case conditionnelle conserve les deux faits. La possibilité de souscrire un nouveau contrat et la continuité du service ne sont pas établies.

## Conditions d’accès relevées {#conditions-offre}

Les restrictions d’édition, d’hébergement et de facturation ci-dessous proviennent des pages officielles consultées le **11 septembre 2026**. Les titres des sources commerciales sont conservés pour identifier le relevé ; les liens techniques de cette page et de la matrice permettent d’examiner les mécanismes concernés. Ces conditions peuvent évoluer et ne constituent pas un engagement contractuel.

### n8n {#conditions-n8n}

Source : page officielle « Plans and pricing ». Offres payantes exprimées en exécutions de workflows ; Git, environnements, SSO et fonctions de gouvernance dépendent de l’édition. Support dédié et SLA annoncés en Enterprise. Le relevé ne généralise pas ces fonctions à l’édition communautaire.

### Make {#conditions-make}

Source : page officielle « Pricing ». Service hébergé, unités de consommation en crédits et contrôles d’équipe selon l’offre. Make Code est soumis aux conditions de l’édition. L’agent on-premise documenté connecte le service à un réseau local ; il ne fournit pas le moteur de workflows à auto-héberger.

### Zapier {#conditions-zapier}

Source : page officielle « Pricing ». Service hébergé, consommation exprimée en tâches, avec règles propres à certaines actions. Export/import JSON réservé à Team/Enterprise ; fonctions de gouvernance et de SSO selon l’offre. Les restrictions précises sont également citées depuis le centre d’aide dans la matrice.

### Activepieces {#conditions-activepieces}

Source : présentation officielle des fonctions IT et de gouvernance. SSO et contrôles d’équipe soumis à l’offre retenue. Les fonctions des parties Enterprise ne sont pas attribuées automatiquement au socle communautaire MIT.

### Dify {#conditions-dify}

Source : page officielle « Dify Enterprise ». SSO, RBAC et audit présentés dans l’édition Enterprise. Cette condition est conservée dans la grille ; elle ne décrit pas une fonction accessible dans toute installation communautaire.

### CrewAI {#conditions-crewai}

Source : page officielle « Pricing ». Studio visuel présenté dans la plateforme, y compris en formule Basic gratuite ; SSO, RBAC et certains modes d’exploitation relèvent de l’offre Enterprise. La plateforme et le framework Python restent deux périmètres distincts.

### Flowise {#conditions-flowise}

Source : site officiel Flowise, qui affichait encore des offres Cloud lors du relevé. Cette mention est conservée avec l’[annonce de fin de vie des mainteneurs](https://github.com/FlowiseAI/Flowise/discussions/6727) : archivage le 13 août 2026, fin des opérations et du support annoncée au 31 août. La continuité effective du service et l’ouverture de nouveaux contrats ne sont pas établies.

## Périmètre des critères

Les critères décrivent le résultat recherché ; la légende distingue ce qui est fourni du travail d’intégration. Les fonctions Git portent sur le dépôt où l’agent intervient, et non sur le seul versionnement des workflows. L’hébergement managé distingue une offre disponible d’une étude sur demande. La grille couvre **20 fonctionnalités et 10 produits** ; elle n’est pas un classement par nombre de coches.

Les paramètres d’un modèle, la politique de coût d’une plateforme et un budget cumulé de run ont des portées différentes. Les sources et limites sont conservées dans les [tableaux détaillés](matrice-fonctionnalites.md#orchestration).

</div>
