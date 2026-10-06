---
name: helm-pitfalls
description: Verified Helm traps for a merge verdict: renamed values keys dropping config silently, subchart aliases and conditions, StatefulSet immutability, frozen rendered tags
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# Helm — pièges vérifiés (conditions, sous-charts, changement de chart)

Pièges rencontrés **et prouvés** dans ce workspace, sur des charts réels — plus un point de structure (le 8) qui explique un choix que l'on est tenté de « simplifier » à tort. Chacun a coûté du temps ou serait passé en production ; aucun n'est théorique. Les commandes de contre-épreuve sont données pour pouvoir les revérifier.

> Convention de lecture : ✅ = comportement **vérifié ici** (avec la manip qui le prouve) · 🪤 = le piège lui-même.

---

## 1. 🪤 Une `condition` de sous-chart est ignorée si le sous-chart n'est pas déclaré en `dependencies`

**Symptôme** : on ajoute `condition:` sur une dépendance, on met le flag à `false`, et le sous-chart est **rendu quand même** — sans erreur, sans warning.

**Cause** : Helm n'évalue les conditions qu'en parcourant les `dependencies` **déclarées dans le `Chart.yaml` du chart racine**. Un chart simplement posé dans `charts/` est chargé et rendu, mais il ne passe jamais par cette évaluation. Or c'est un usage courant : beaucoup de charts applicatifs empilent leurs sous-charts dans `charts/` sans rien déclarer.

**Corollaire, le plus dangereux** : quand la clé de condition est **introuvable**, le défaut de Helm est **« activé »**, pas « désactivé ». Un chemin mal orthographié ne désactive donc rien — il rend le composant partout.

**Fix** : déclarer le sous-chart conditionnel dans le `Chart.yaml` racine. Les autres sous-charts peuvent rester implicites.

```yaml
# Chart.yaml du chart racine
dependencies:
  - name: valkey
    version: 0.1.0
    repository: "file://charts/valkey"   # sous-chart local
    condition: valkey.enabled
```

## 2. 🪤 La condition est résolue dans les values **racine**, préfixée par le nom du sous-chart

**Symptôme** : la condition est bien déclarée, et pourtant elle ne mord toujours pas.

**Cause** : le chemin d'une condition n'est **pas** relatif aux values du chart qui la déclare — il est résolu dans les values du chart **racine**, préfixé par le nom du sous-chart. Écrire `condition: enabled` dans `charts/valkey/Chart.yaml` fait donc chercher **`valkey.enabled`** à la racine ; y écrire `condition: valkey.enabled` fait chercher `valkey.valkey.enabled` (introuvable ⇒ activé, cf. piège 1).

**Conséquence pratique** : la clé doit exister **dans les values du chart racine**. Un `enabled: false` posé seulement dans le `values.yaml` du sous-chart ne suffit pas. Si le chart racine n'a pas de `values.yaml` — cas réel — il faut le créer.

```yaml
# values.yaml du chart RACINE (à créer s'il n'existe pas)
valkey:
  enabled: false     # défaut sûr ; les values/<env>.yaml passent à true
```

✅ **Contre-épreuve** (sirena, 2026-07-27) : rendu de 4 environnements après le fix — **7 manifests** là où le flag est à `true`, **0 et dossier de sortie absent** là où il est à `false`. Avant le fix, les 4 rendaient le sous-chart. Le test qui tranche, à faire systématiquement quand on introduit un flag :

```bash
helm template . -f values/<env>.yaml | grep -c "^# Source: <chart>/charts/<souschart>"
```

⚠️ Un flag conditionnel n'est **jamais** acquis sur la foi du seul `values.yaml` : il se vérifie par un rendu de l'environnement où le composant doit être **absent**.

## 3. 🪤 `alias:` sur une dépendance renomme le chart — et donc les labels et les noms de ressources

**Symptôme** : on ajoute un `alias` pour ranger proprement les values (`cache.server.*` plutôt que `cache.valkey.*`), et les labels `app.kubernetes.io/name` changent — donc les selectors des Services et les noms de ressources aussi.

**Cause** : l'alias ne renomme pas seulement la clé de values, il remplace le **nom du chart** dans l'arbre. Tout helper basé sur `.Chart.Name` (le `fullname`/`name` de la quasi-totalité des charts) suit.

✅ **Vérifié** (chart officiel valkey 0.11.0, même chart, seul l'alias diffère) :

| | `app.kubernetes.io/name` | Ressources | Chemin de rendu |
|---|---|---|---|
| avec `alias: server` | `server` | `t-server`, `t-server-init-scripts` | `charts/server/templates/…` |
| sans alias | `valkey` | `t-valkey`, `t-valkey-init-scripts` | `charts/valkey/templates/…` |

**À retenir** : sur un composant **déjà déployé**, ajouter ou retirer un alias change les selectors — donc casse les Services qui pointent dessus, et peut buter sur l'immutabilité (piège 5). Un `fullnameOverride` fige les *noms* mais **pas** le label `app.kubernetes.io/name`.

## 4. 🪤 Écarts de version du binaire `helm` ⇒ faux diffs

Un `helm template | diff` n'est comparable que si **la même version de Helm** produit les deux côtés. Deux occurrences ici :

- **devbox (v3) vs Homebrew (v4) en shell de login** : lancer `devbox run -- bash -lc` fait passer le PATH du login shell devant celui de devbox, donc un Helm v4 Homebrew rend là où on croyait rendre en v3 — et les golden tests `helm template | diff` cassent. **Toujours `devbox run -- bash -c`** (non-login) pour un rendu reproductible.
- **poste vs CI** : le CI SDPSN rend avec `alpine/helm` (épinglé `3.20.2` depuis la v3.3.1 du chart). Un rendu local d'une autre version fait apparaître des diffs de bruit (ordre de clés, champs par défaut) sur des composants non touchés. **En cas de doute, le dry-run de la CI fait foi** : il montre le vrai périmètre.

## 5. 🪤 Changer de chart pour un composant déjà déployé : les champs immuables

Remplacer le chart d'un composant existant (migration hors Bitnami, changement d'upstream) heurte l'immutabilité si le **nom des objets ne change pas** :

- **StatefulSet** : `serviceName`, `selector`, `volumeClaimTemplates` sont immuables → l'apply échoue, il faut **`delete` puis recréer**.
- Le **PVC** de l'ancien `volumeClaimTemplates` **survit** (aucune `ownerReference`) et, en RWO, provoque des *Multi-Attach errors* s'il reste attaché → le purger explicitement.

**Le contournement qui évite tout ça** : donner au nouveau composant un **nom différent**, et exposer l'ancien nom par un **Service alias** qui sélectionne les nouveaux labels. Le workload ne « change » pas, il est créé à côté puis l'ancien est retiré ; aucun champ immuable n'est touché.

```yaml
# Service alias : l'app continue de joindre `redis-master`, l'implémentation s'appelle `valkey`
apiVersion: v1
kind: Service
metadata:
  name: redis-master
spec:
  ports:
    - name: tcp
      port: 6379
      targetPort: tcp        # ⚠️ nom du port côté conteneur, il varie d'un chart à l'autre
  selector:
    app.kubernetes.io/name: valkey
    app.kubernetes.io/instance: {{ .Release.Name }}
```

✅ Appliqué sur sirena (redis Bitnami → Valkey) : bascule sans `delete` de workload, et **sans toucher le chart applicatif** qui codait `redis-master` en dur.

🪤 **Le nom du port du conteneur varie selon le chart** (`valkey` chez groundhog2k, `tcp` chez l'officiel). Dès que le Service est mis à jour avec un `targetPort` que l'ancien pod n'expose pas, celui-ci est **débranché immédiatement** (endpoints `<none>`), sans attendre que le nouveau soit prêt : prévoir la coupure, même si l'on comptait sur un recouvrement.

🪤 **Ce qui reste après le retrait de l'ancien chart, côté ArgoCD** — deux cas à ne pas confondre :

- les objets **déclarés** dans la source sont suivis (annotation `argocd.argoproj.io/tracking-id`, `argocd-controller` dans les `managedFields`) donc **prunés normalement** en les retirant de la source ;
- les **PVC issus d'un `volumeClaimTemplates`** ne le sont **jamais** : ils sont créés par le contrôleur StatefulSet, ArgoCD ne les a pas appliqués, ils n'ont pas de `tracking-id` (vérifiable : seul `kube-controller-manager` dans leurs managers). Ils ne partent pas davantage avec le StatefulSet, faute de `persistentVolumeClaimRetentionPolicy`. **Ce sont eux qu'on oublie**, et ils continuent d'être facturés.

⚠️ Et si le prune semble ne pas fonctionner du tout, chercher du côté de l'option **`PruneLast`** : elle diffère le prune en une vague finale, exécutée seulement après que tout le reste est *healthy* et que toutes les waves ont réussi. **Sur une Application durablement Degraded, le prune n'a donc jamais lieu.** D'où un cercle vicieux classique : un composant cassé maintient l'app en Degraded, ce qui empêche de nettoyer… ce composant cassé, et tout ce qu'on retire par ailleurs. Le remède est de réparer d'abord ce qui bloque la santé (un ReplicaSet obsolète en `ImagePullBackOff` suffit à tout figer), pas de supprimer à la main indéfiniment.

## 6. 🪤 Deployment + PVC `ReadWriteOnce` : `Recreate` obligatoire

Un chart qui rend un **Deployment** avec un PVC RWO et la stratégie `RollingUpdate` par défaut se bloque à la première mise à jour : le nouveau pod ne peut pas monter le volume que l'ancien détient encore (*Multi-Attach*). Il faut `strategy: Recreate` (souvent exposé en values : `deploymentStrategy`, `updateStrategyType`…).

✅ **Vérifié** (sirena/test, changement d'image) : ancien pod **totalement supprimé** avant création du nouveau, volume détaché/rattaché sans incident, **données conservées**, **~10 s** d'indisponibilité. C'est le prix à accepter — un cache ou une base mono-instance ne se met pas à jour sans coupure.

À noter au passage : certains charts ne savent **pas** rendre un StatefulSet à une seule instance (celui de `valkey-io` conditionne son StatefulSet à `replica.enabled`, qui impose ≥ 2 pods). Le Deployment + `Recreate` est alors la seule option raisonnable.

## 7. 🪤 Scories de rendu à nettoyer quand on fige des manifests

Quand un rendu est **figé** dans un repo gitops (plutôt que régénéré par la CI), deux scories fréquentes valent un post-traitement :

- **clés vides sérialisées `null`** — un `envFrom:` sans contenu produit `envFrom: null`. Valide pour `kubectl` et `kubeconform`, mais c'est du bruit et une source de diff parasite côté ArgoCD.
- **`automountServiceAccountToken`** — beaucoup de charts ne l'exposent pas ; un composant qui n'appelle pas l'API Kubernetes n'a aucune raison de monter un token.

⚠️ Tout post-traitement d'un rendu figé doit vivre **dans un script de génération versionné**, jamais en édition manuelle : sinon il est perdu à la prochaine régénération. Et le vrai remède reste de **ramener le composant dans un chart** rendu par la CI — un rendu figé n'est jamais mis à jour, c'est ainsi qu'un `:latest` cassé peut tourner 286 jours sans que personne ne le voie (cf. [runbook sirena](../30-migrations/sirena.md#sortie-de-bitnami--bascule-redis--valkey-2026-07-27)).

## 8. 🪤 Wrapper autour d'un chart tiers : le « double niveau » est souvent la seule option

**Symptôme inverse des autres** : à la relecture, l'empilement `chart racine → wrapper local → chart tiers` ressemble à de la complexité gratuite, et la tentation est de « simplifier » en mettant le chart tiers en dépendance directe du racine.

**Ce que ça casse** : dès qu'on ajoute **ses propres manifests** à côté d'un chart tiers (un Service alias, un ExternalSecret, un ConfigMap maison), il faut un chart local pour les héberger — on ne modifie pas un chart tiers. La seule question est *où* ce chart local se situe, et la réponse dépend de **ce que la chaîne de déploiement récupère réellement du rendu**.

Exemple mesuré (sirena) : l'action CI ne copie vers le repo gitops que les **sous-charts** —

```bash
mv helm_charts/generated_manifests/sirena/charts/* <gitops>/deployment-targets/<env>/
```

— donc un template rendu à la racine du chart (`generated_manifests/<chart>/templates/`) est **silencieusement perdu**, jamais déployé. D'où le wrapper : il est le seul endroit à la fois *local* (on peut y écrire nos templates) et *récupéré* par la CI.

**Avant de « simplifier » un empilement de sous-charts**, vérifier donc trois choses :
1. quels chemins du rendu la chaîne de déploiement copie réellement (`mv`, `cp`, `rsync`, `--output-dir`…) ;
2. si le chart racine possède un `templates/` exploitable — souvent il n'en a aucun, c'est un pur agrégateur ;
3. si le wrapper porte des templates propres, sans lesquels le composant ne fonctionne pas.

**Le coût à assumer** : le chemin des values gagne un niveau (`<wrapper>.<chart-tiers>.*`), un `helm dependency update` de plus, une arborescence rendue imbriquée — et c'est ce niveau supplémentaire qui rend les pièges 1 et 2 nettement moins lisibles.

💡 Et un signe qu'on est dans le bon moule : regarder si le dépôt utilise **déjà** ce pattern ailleurs. Chez sirena, `charts/external-secrets` combinait de longue date une dépendance externe et 3 templates propres — le wrapper valkey n'a fait que suivre.

---

## Voir aussi

- [Pattern CD — vue d'ensemble](overview.md) : chart agnostique dans le repo app ⇄ repo gitops.
- [Secrets via External Secrets](secrets-externalsecrets.md) : pièges ESO, dont `target.immutable` (le Secret n'est écrit qu'une fois — ajouter une clé n'a d'effet qu'après suppression).
- [Runbook sirena](../30-migrations/sirena.md) : le terrain d'où viennent les pièges 1, 2, 3, 5, 6 et 7.

# Documentation des charts Helm : `helm-docs`

> **Décision.** [`helm-docs`](https://github.com/norwoodj/helm-docs) génère le `README.md` d'un
> chart (badges, description, table `Values`) à partir de `values.yaml` — pour que la doc ne
> puisse pas diverger des vraies valeurs par défaut. Adopté dans
> [`devops-charts`](https://pic.sg.social.gouv.fr/socialgouv/produits-dnum/studio-tech/devops/charts)
> (dépôt des charts Helm transverses DNUM, cf. son propre README), premier chart couvert :
> **mailpit** (`dnum/mailpit/`). **Opt-in par chart** — `loadtest` n'y est pas encore passé, ce
> n'est pas un prérequis pour ajouter un chart au dépôt.

## Les trois pièces

1. **`values.yaml` annoté** — un commentaire `# --` juste au-dessus d'une clé devient sa
   description dans la table générée :
   ```yaml
   auth:
     # -- Active l'auth Basic sur la web UI (MP_UI_AUTH).
     enabled: false
   ```
   Une clé sans annotation apparaît quand même dans la table (type + défaut), juste avec une
   description vide — pas bloquant, juste moins utile.

2. **`README.md.gotmpl`** (dans le dossier du chart) — LE déclencheur de l'opt-in : sa seule
   présence dans `dnum/<chart>/` active `helm-docs` pour ce chart (cf. job CI plus bas). C'est un
   GoTemplate qui assemble la page en appelant les blocs intégrés (`chart.header`,
   `chart.description`, `chart.valuesSection`, `helm-docs.versionFooter`, ...) et, au besoin, des
   blocs **partagés** définis dans le fichier suivant.

3. **`dnum/_templates.gotmpl`** (racine `dnum/`, partagé entre charts) — contient des blocs
   réutilisables via `{{ define "nom" }}...{{ end }}` (ex. `extra.header`, `extra.install`),
   **sans aucun contenu de premier niveau**. C'est la règle qui compte : `helm-docs` exécute
   *chaque* fichier qu'on lui donne comme un document séparé et **concatène leurs sorties** — un
   fichier qui a du contenu en dehors d'un `{{ define }}` produit sa PROPRE page, dupliquée à
   côté de celle du chart. `_templates.gotmpl` doit donc rester define-only ; c'est au
   `README.md.gotmpl` du chart d'appeler explicitement `{{ template "extra.install" . }}` là où
   il le veut dans sa mise en page.

## 🪤 `--template-files` se résout **relativement au chart trouvé**, pas à la racine

Vérifié empiriquement (pas documenté clairement en amont) : avec
`--chart-search-root=dnum/<chart>`, un chemin `--template-files=dnum/_templates.gotmpl` ou
`./dnum/_templates.gotmpl` échoue silencieusement à charger le fichier (`template "extra.xxx" not
defined` à l'exécution) — malgré la doc upstream qui affirme supporter les chemins
« chart-relative et root-relative ». Le chemin qui marche, depuis `dnum/<chart>/` :
```bash
helm-docs --chart-search-root=dnum/<chart> \
  --template-files=../_templates.gotmpl \
  --template-files=README.md.gotmpl
```
Le `README.md.gotmpl` du chart doit être repassé explicitement dès qu'on ajoute au moins un
`--template-files` — l'auto-découverte par `--chart-search-root` seule ne suffit plus à le
mettre dans le même jeu de templates que le fichier partagé.

## Job CI — un seul job générique, pas un `docs-<chart>` par chart

Contrairement à `lint-<chart>`/`publish-<chart>` (dupliqués par chart dans `devops-charts`), la
vérification doc est **un seul job** (`docs`) qui se déclenche sur tout changement sous
`dnum/**/*`, détecte lui-même **quel(s) chart(s)** a(ont) réellement changé (git diff sur la base
de comparaison fiable — `CI_MERGE_REQUEST_DIFF_BASE_SHA` en MR, `CI_COMMIT_BEFORE_SHA` en push,
repli sur tous les charts si la base est introuvable), puis pour chacun :
- **skip silencieux** si `dnum/<chart>/README.md.gotmpl` n'existe pas (opt-in) ;
- sinon régénère (`helm-docs` avec les `--template-files` ci-dessus si `_templates.gotmpl`
  existe) et `git diff --exit-code` sur le `README.md` — échec si désynchronisé, avec un message
  qui dit exactement quelle commande relancer en local.

Ne **jamais** auto-commit le résultat en CI (pas de push depuis le job) : le dev régénère et
commit lui-même, cohérent avec la philosophie du dépôt (`README.md` racine : « ce dépôt ne publie
rien sans changement réel »).

## Adopter pour un nouveau chart

1. Annoter les clés utiles de `values.yaml` avec `# --` (pas besoin d'être exhaustif).
2. Ajouter un `README.md.gotmpl` dans `dnum/<chart>/` — reprendre celui de `mailpit` comme
   gabarit si on veut le socle partagé (`extra.header`/`extra.install` de `_templates.gotmpl`),
   ou un template 100% autonome sinon (aucune obligation d'utiliser le fichier partagé).
3. Régénérer en local avec la commande du job CI (cf. `--template-files` ci-dessus) et committer
   le `README.md` généré.
4. Rien à toucher côté `.gitlab-ci.yml` — le job `docs` couvre automatiquement tout chart avec un
   `README.md.gotmpl`.

## Exécuter en local (sans installer l'outil)

```bash
docker run --rm --volume "$(pwd):/helm-docs" jnorwood/helm-docs:latest \
  --chart-search-root=/helm-docs/dnum/<chart> \
  --template-files=../_templates.gotmpl --template-files=README.md.gotmpl
```
⚠ Comme `alpine/helm`, l'image a pour `ENTRYPOINT` `helm-docs` — en CI, penser à
`entrypoint: [""]` (sinon `sh -c "..."` devient `helm-docs sh -c "..."`, même piège documenté
côté [ci-common](../10-platforms/ci-common.md)/[gitops-cd-component](../20-cd-pattern/gitops-cd-component.md)).
