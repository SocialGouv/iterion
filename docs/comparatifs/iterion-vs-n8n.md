---
title: "Iterion ou n8n"
description: "Les différences utiles pour choisir entre Iterion et n8n."
aside: false
pageClass: "comparison-page"
---

<div lang="fr">

# ⚖️ Iterion ou n8n ?

Fiche prospects · 11 septembre 2026.

Pour les détails d’exécution, d’isolation et de déploiement : [matrice des fonctionnalités techniques](matrice-fonctionnalites.md).

✅ [Voir directement quelles fonctionnalités chaque produit propose](matrice-fonctionnalites.md#disponibilite).

📚 [Inventaire Iterion : 139 critères avec sources et limites](inventaire-fonctionnalites-iterion.md).

**Avec Iterion, formalisez le cycle complet de vos missions d’agents : analyser, modifier, tester, relire et valider.** Workflows versionnés, worktrees Git, boucles de correction et pilotage des exécutions sont réunis dans le produit. n8n couvre notamment les automatisations entre applications et dispose aussi d’agents IA ; la comparaison porte sur le parcours nécessaire pour obtenir votre résultat. Cette orientation s’appuie sur les capacités documentées, sans benchmark comparatif. [Fonctionnalités d’Iterion](inventaire-fonctionnalites-iterion.md), [comparaison technique sourcée](matrice-fonctionnalites.md).

## 🔍 Les différences utiles à votre décision

<div class="comparison-table" role="region" aria-label="Iterion ou n8n — tableau 1" tabindex="0">

| Critère | <img class="comparison-logo" src="./assets/logos/iterion.png" alt="" width="24"> **Iterion** | <img class="comparison-logo" src="./assets/logos/n8n.ico" alt="" width="24"> **n8n** |
|---|---|---|
| 📄 Définition du travail | Fichier `.bot` versionnable et éditeur visuel ; agents, outils, juges, conditions et boucles. | Éditeur visuel de workflows, nœuds applicatifs, fonctions de code et agents. |
| 🔌 Connexions aux applications | Intégrations de forges, MCP, outils et extensions ; catalogue généraliste élargi encore en projet. | Large catalogue applicatif et connexions API personnalisées. |
| ⌨️ Travail sur le code | Bots de revue, correction et maintenance ; worktrees et politique de finalisation intégrés. | Peut participer au processus ; vérifier et construire le parcours précis demandé par le client. |
| 🙋 Intervention humaine | Étapes humaines et pilotage des missions depuis les surfaces Iterion. | Contrôles humains pour l’IA et mécanismes d’attente/reprise. |
| 💾 Reprise | Checkpoint des exécutions éligibles ; l’étape en échec peut être relancée. | Le nœud Wait persiste les données d’une attente puis recharge l’exécution ; les autres scénarios de panne doivent être testés séparément. |
| 💰 Encadrement du travail | Budgets configurables de durée, tokens, coût estimé et itérations ; vérifier leur portée et les règles de finalisation. | Vérifier les limites d’exécution et la politique de consommation adaptées au workflow et au plan. |
| 🌿 Versionnement et exploitation | Sources `.bot` dans Git ; exploitation locale et plateforme auto-hébergée. | Git et environnements dans certaines offres ; cloud et auto-hébergement. |
| 🤝 Maturité et achat | Projet expérimental ; engagement commercial et support à établir pour le déploiement envisagé. | Offres commerciales documentées, dont support dédié avec SLA en Enterprise. |

</div>


Sources Iterion : [DSL](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/dsl.md), [finalisation Git](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/adr/064-worktree-finalization-requires-delegated-authority.md), [reprise](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/resume.md), [catalogue en projet](https://github.com/SocialGouv/iterion/issues/1072). Sources n8n : [documentation](https://docs.n8n.io/), [Wait](https://raw.githubusercontent.com/n8n-io/n8n-docs/main/docs/integrations/builtin/core-nodes/n8n-nodes-base.wait.md), [conditions d’accès relevées](methode-et-sources.md#conditions-n8n).

## 🎯 Trois situations concrètes

Pour approfondir l’évaluation d’Iterion, inclure aussi la [supervision active](inventaire-fonctionnalites-iterion.md#supervision), les [questions asynchrones](inventaire-fonctionnalites-iterion.md#humain), la [mémoire entre missions](inventaire-fonctionnalites-iterion.md#memoire), les [skills et plugins](inventaire-fonctionnalites-iterion.md#extensions), la [restauration de fichiers](inventaire-fonctionnalites-iterion.md#reprise) et le [kanban avec dispatcher](inventaire-fonctionnalites-iterion.md#board). Ces critères complètent le premier tableau ; leur inventaire côté Iterion ne permet pas de conclure à leur absence dans n8n.

**« Quand un formulaire arrive, enrichir le contact et mettre à jour le CRM. »** Ce besoin relève d’abord des connecteurs et du routage métier, couverts par n8n. Iterion devient utile si une étape demande une mission d’agent avec analyse, contrôles et livrable à valider. Pour un simple transfert de données, son introduction peut être superflue.

**« À chaque demande de correction, analyser le dépôt, modifier le code, tester et faire relire. »** Essayez Iterion sur un dépôt pilote. Évaluez son parcours intégré : workflow versionné, travail Git distinct, boucles de correction, résultat inspectable et validation humaine. L’effort de construction d’un parcours équivalent dans n8n est à mesurer, sans présumer qu’il est impossible. [Bots Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/examples.md).

**« Nous avons déjà nos automatisations dans n8n. »** Une coexistence est envisageable : conserver les flux métier existants et faire appeler une mission Iterion lorsqu’ils en ont besoin. Le retour doit inclure un identifiant d’exécution, un état terminal et les liens vers les résultats. Cette intégration reste à réaliser et à valider pour le client. [Interfaces Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/docs/cloud-overview.md).

## 💬 Les questions fréquentes

**n8n sait déjà faire des agents : quel serait l’intérêt d’Iterion ?** Comparer la méthode complète : exécution sur le dépôt, tests, revue, itérations, gestion des résultats et reprise. La valeur à démontrer est le travail d’intégration et d’exploitation économisé sur ce parcours. Elle ne repose pas sur une exclusivité de l’IA, de MCP ou du contrôle humain. [Fonctions IA de n8n](https://docs.n8n.io/integrations/builtin/cluster-nodes/root-nodes/n8n-nodes-langchain.agent/tools-agent).

**Iterion coûte-t-il moins cher ?** Ce comparatif ne l’établit pas. Le code Iterion est sous MIT, mais les modèles, l’hébergement et le temps d’exploitation ont un coût. n8n dispose d’une édition auto-hébergée communautaire et d’offres payantes fondées sur les exécutions. Comparer le coût total pour obtenir une correction acceptée ou une revue exploitable. [Licence Iterion](https://github.com/SocialGouv/iterion/blob/bbc1ddc7844b81582b6dfc4d0b7f381680bb284b/LICENSE), [conditions de facturation relevées](methode-et-sources.md#conditions-n8n).

**Les deux sont-ils équivalents pour l’auto-hébergement ?** Ils proposent des possibilités d’auto-hébergement, mais leurs licences et leurs conditions commerciales diffèrent. Le code principal n8n est soumis à la Sustainable Use License, avec un régime distinct pour les composants Enterprise. Il ne faut pas assimiler cette licence à MIT. [Licence n8n](https://raw.githubusercontent.com/n8n-io/n8n/master/LICENSE.md).

**Quel serait un bon premier essai ?** Une mission sur un dépôt de test avec un résultat vérifiable, une limite de travail configurée, une validation humaine et une interruption volontaire. Mesurer le temps de préparation, le temps humain restant, le coût et la qualité du résultat. Répéter l’essai avant de tirer une conclusion sur la fiabilité.

<div class="comparison-cta">

**Votre prochain pas : une mission pilote avec Iterion.**

Choisissez un dépôt, un résultat attendu et vos critères de validation. Le pilote permettra de mesurer ce que le parcours intégré vous apporte, y compris si vous conservez vos automatisations métier existantes.

[Démarrer avec Iterion](../quickstart.md) · [Choisir un bot pour le pilote](../examples.md)

</div>

</div>
