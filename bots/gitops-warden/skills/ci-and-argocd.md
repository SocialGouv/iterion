---
name: ci-and-argocd
description: The gitops CD layout: apps/<app>-<env>/ wrappers, main/rendered branches, CI components and ArgoCD definitions — the structural class a value-level MR must not touch
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# ci-common — templates CI mutualisés : hébergement, règle de visibilité, consommateurs

Les templates CI réutilisables (`image-build.yml`, `publish-chart.yml`, `buildkit-prune.yml`, `atlas-env-lifecycle.yml` — build DinD/BuildKit, publish chart Helm OCI, cycle de vie d'env Atlas en CI) inclus par les produits migrés.

## Emplacement (depuis 2026-07-02)

**`socialgouv/commun/studio-tech-commun/sdpsn-devops-ci-utils`** (projet **382**, visibilité **`internal`**), sous-dossier **`ci-common/`**. Inclusion :

```yaml
include:
  - project: socialgouv/commun/studio-tech-commun/sdpsn-devops-ci-utils
    ref: main
    file: ci-common/image-build.yml      # ou ci-common/publish-chart.yml, ci-common/buildkit-prune.yml
```

Puis `extends:` les jobs cachés (`.build-dind`, `.build-buildkit-operator`, `.build-buildkit-service` (legacy), `.publish-chart`, `.buildkit-prune`). Le backend **buildkit-operator** (daemon dédié par projet, OIDC — cf. [buildkit-mutualise.md](../15-build-ci/buildkit-mutualise.md)) remplace le service mutualisé depuis 2026-07-03 (MR !3).

**`atlas-env-lifecycle.yml`** (MR !4/!5/!6, 2026-07-07) est différent : pour les repos **gitops**, fragment `.atlas-env-lifecycle` à injecter par **`!reference [...:, script]`** (pas `extends`) — définit `atlas_env_ensure` (create-on-open : Environment Atlas + **OvhBucket homonyme optionnel** `ATLAS_BUCKET_PER_ENV=1` + configure-gitops + descripteur), `atlas_env_delete` et `atlas_bucket_delete` (delete-on-close : `stop-review` immédiat + `cleanup-review` schedulé). Auth = CI var `ATLAS_TOKEN` (PAT Atlas). Consommateurs validés e2e : da-manager-gitops, graal-gitops, egapro-gitops. Détail + pièges (liste buckets = `GET /ovhbuckets`, PAS la route workspace) : [atlas-pat-machine-identity](../20-cd-pattern/atlas-pat-machine-identity.md).

> Historique : les templates vivaient dans `socialgouv/produits-dnum/communs/ci-common` (projet **439**, `private`). **439 est désormais obsolète** (conservé comme backstop tant qu'aucun pipeline n'y renvoie ; à archiver après un cycle de release).

## Pourquoi cet emplacement (règle PIC imposée par la plateforme, 2026-07-02)

- **Interdit de passer un projet/groupe de `produits-dnum` en `internal`** — restriction d'**instance** (`internal has been restricted by your GitLab administrator`), réservée aux **admins**, vaut pour un transfert **comme** pour une création. Donc impossible de rendre `communs/ci-common` internal.
- Les **templates CI mutualisés** doivent vivre dans **`socialgouv/commun/studio-tech-commun`** (déjà `internal`), comme les jumeaux `sdpsn-devops-chart`, `sdpsn-devops-ci-utils`, `docker_images/*`. On a **hébergé nos templates dans `sdpsn-devops-ci-utils`** (déjà internal, sous-dossier `ci-common/`) plutôt que d'attendre un admin pour rendre un repo à nous internal.

Pourquoi `internal` est nécessaire : `include:project` d'un repo **`private`** exige que le **déclencheur** ait ≥ Reporter. Les tags de release sont créés par un **bot de token projet** (non-membre, non-ajoutable) → l'include cassait (pipeline de tag « failed »). `internal` = lisible par tout compte authentifié, **bots compris** → l'include résout. Détail : [pic-gitlab.md § include:project d'un repo privé déclenché par un bot](pic-gitlab.md#includeproject-dun-repo-privé-déclenché-par-un-bot-releasetags).

## Ce qui NE marchait PAS (prouvé, 2026-07-02)

- **Déplacer ci-common sans internal** → il reste `private` ; un bot projet non-membre le lit = **404** (jo Owner = 200). Ne débloque pas les bots.
- **Recréer un repo dans studio-tech-commun** → création avec `visibility=internal` **refusée** (même restriction admin) → repo `private` = même problème.
- **`include:project` ne suit PAS le redirect 301** : transférer ci-common casse tous les includes (ancien chemin) tant qu'ils ne sont pas mis à jour (testé → rollback).
- ✅ **Ce qui marche** (fait) : héberger les templates dans un repo **déjà internal** (`sdpsn-devops-ci-utils`) et repointer les consommateurs. Aucune fenêtre de casse (templates ajoutés là-bas **avant** de repointer). Validé : un **bot lint graal = valid** (le bot résout l'include).

## Consommateurs (tous migrés vers sdpsn, 2026-07-02)

Découverts par scan des `.gitlab-ci.yml` de tous les projets produits-dnum (recherche instance-wide indisponible). Aucun repo **gitops** n'utilise ci-common.

| Repo | id | branche |
|---|---|---|
| studio-tech/architecture/da-manager/da-manager | 433 | main |
| studio-tech/ia/graal/graal | 445 | main |
| travail/egapro/egapro | 536 | feat/ci-atlas-v2 |
| citoyens/cdtn/cdtn-admin | 526 | main |
| citoyens/cdtn/code-du-travail-numerique | 525 | main |
| citoyens/vao | 292 | feat/ci-atlas-v2 |
| citoyens/srdt/srdt | 372 | feat/migration-pic-atlasv2 |
| travail/sitere/suit/ptt/ptt-home | 37 | feat/ci-atlas-v2 |
| travail/sitere/suit/ptt/ptt-chantier | 38 | feat/ci-atlas-v2 |
| travail/sitere/suit/ptt/ptt-lsp | 39 | feat/ci-atlas-v2 |

Remplacement appliqué (commit direct `[skip ci]`) : `socialgouv/produits-dnum/communs/ci-common` + `file: templates/X.yml` → `socialgouv/commun/studio-tech-commun/sdpsn-devops-ci-utils` + `file: ci-common/X.yml`.

## Maintenance

Les templates sont désormais dans le repo d'une autre équipe (couplage assumé, faute de repo à nous internal). `sdpsn-devops-ci-utils` `main` est protégée (`push=No one`) → modifs via **MR** (dossier `ci-common/`). On garde le contenu source aussi dans ce meta-repo pour référence. Si un jour un admin rend un repo produits-dnum internal, on pourra rapatrier.

# Pattern CD cible — repo applicatif ⇄ repo chart ⇄ repo gitops

> Source de vérité : l'implémentation **validée end-to-end** da-manager — [`da-manager` (GitLab PIC, proj 433)] + [`da-manager-gitops` (434)] + runbook [`docs/30-migrations/da-manager.md`](../30-migrations/da-manager.md) — **pour la mécanique app⇄gitops** (trigger, apply, render, branche `rendered`). Pour le découpage en **3 repos** (app/charts/gitops), la référence est **poss** (2026-08-27) — da-manager reste en 2 repos (legacy, retrofit non encore fait, cf. [gitops-cd-component](gitops-cd-component.md)). Le PoC initial **vao** reste une référence **historique** (modèle push, désormais remplacé par le trigger). En cas de doute, lire le code GitLab.

## Découplage (trois dépôts)

> **Décision (2026-09-01) : 3 repos pour tout produit** (nouveau ou à retrofiter) — le chart Helm
> vit dans un repo dédié `<app>-charts`, séparé du repo applicatif, pour cloisonner les droits
> d'écriture sur le chart (permissions GitLab repo-scopées, pas path-scopées). Détail du
> mécanisme CI (composant réutilisable, deux flows chart/image découplés) :
> [gitops-cd-component](gitops-cd-component.md).

```
┌───────────────────────┐     ┌────────────────────────┐     ┌──────────────────────────────────┐
│  REPO APPLICATIF      │     │  REPO CHART DÉDIÉ       │     │   REPO GITOPS (<app>-gitops)      │
│  (<app>)              │     │  (<app>-charts)         │     │  apps/<app>-<env>/                │
│  - code               │     │  - charts/<app>/  ──────┼OCI─▶│    Chart.yaml (dépend du chart    │
│  - .gitlab-ci.yml:    │     │    (chart agnostique)   │publish│    OCI, version épinglée)        │
│    build+publish image│     │  - .gitlab-ci.yml:      │     │    values.yaml (spécifique env)   │
│    trigger_app ────────────────────────────────────────┼────▶│    templates/ ExternalSecret…     │
│    ($IMAGE_TAG, à     │     │    publish chart (OCI)  │     │  .gitlab-ci.yml: apply_app/        │
│     chaque commit)    │     │    trigger_chart ────────┼────▶│    apply_chart → render →         │
│                        │     │    ($CHART_VERSION,     │     │    manifests rendus               │
│                        │     │     rare)                │     │                                    │
└───────────────────────┘     └────────────────────────┘     └───────────────┬────────────────────┘
                                                                                │ ArgoCD (Atlas v2) sync
                                                                                ▼
                                                                      Cluster (workload-zone)
```

*(Variante 2 repos — app+chart au même endroit, `<app>-gitops` seul en face — encore en place sur
les produits migrés avant la décision du 2026-09-01, cf. § retrofit dans
[gitops-cd-component](gitops-cd-component.md).)*

## Responsabilités

### Repo applicatif → chart Helm **plateforme-agnostique**
- Vit **dans le repo du code** (`charts/<app>/`), évolue avec l'app.
- **Ne connaît pas** l'environnement, ni les secrets (référencés **par nom**), ni la plateforme.
- Publié en **artefact OCI versionné** (`oci://<registre-pic>/.../charts`). Version = SHA (snapshots `0.0.0-sha.<sha>`) ou tag sémver.
- Image consommée via un helper qui prend `global.imageRegistry` + `appVersion`/tag.

### Repo gitops → **spécifique-instance** + rendu
- Un **wrapper chart** par environnement (`apps/<app>-<env>/`) qui **dépend** du chart OCI (version épinglée) et fournit les `values` de l'env (host, ressources, ConfigMap).
- Porte les **External Secrets** (chargeurs Vault → Secret K8s) et le **set de ressources** d'infra (provisionné côté Atlas par API).
- Sa **CI rend les manifests** (`helm dependency build` + `helm template`) ; **ArgoCD** (Atlas v2) synchronise le résultat. Voir [contrat configure-gitops](../10-platforms/atlas-v2.md).

## Pourquoi ce découplage
- Le chart suit le **cycle de vie du code** (cohérence app ↔ déploiement).
- Déploiements **auditables** (diff sur manifests rendus) et **reproductibles** (versions épinglées).
- Secrets et infra **hors du code applicatif**, gérés au niveau instance.

## Simplifications adoptées vs le PoC vao

> Le découplage app/gitops, le chart publié **OCI**, les **rendered manifests** et la review-par-branche viennent du **PoC vao** (build DinD multi-services fait-main, ciblait l'infra ère-Fabrique : SealedSecrets, proxy intranet). On en garde la **structure** et on l'adapte à Atlas v2.

- **Sealed Secrets → External Secrets (ESO)** : imposé par Atlas v2. Voir [secrets-externalsecrets](secrets-externalsecrets.md).
- **Build** : DinD fait-main → **template [BuildKit mutualisé](../15-build-ci/buildkit-mutualise.md)** (maison).
- **Postgres** : **convention par environnement** (pas par produit, MàJ 2026-09-14) — **CNPG in-cluster** pour **dev / review** (gratuit, rapide, isolé par env/branche, zéro dépendance OVH), **PG managée OVH** (`OvhPostgresqlCluster` via l'API Atlas) pour **preprod ET prod**, avec des **plans différents** (**preprod sans HA**, **prod en HA** + backups OVH — la preprod préfigure la prod). Dans les deux cas le chart reste **agnostique** : la connexion arrive par un Secret référencé **par nom** (CNPG `<cluster>-app` clé `uri`, ou secret de connexion managé) ⇒ bascule possible env par env. Cf. [atlas-v2 § Base de données](../10-platforms/atlas-v2.md).
- **Garder simple** : pour un mono-service, un seul `Deployment` + `Service` + `Ingress` + `Job` de migration + `ConfigMap` + `ExternalSecret`. Pas de complexité review/multi-env tant que dev n'est pas validé.

## Automatisation CI (réutilisable)

**Raccordement app → gitops = TRIGGER (pipeline multi-projet), AUCUN token gitops stocké dans l'app.** Décisions cadrées : **2 repos** (séparation app/ops, **un sous-tenancy Atlas par projet**), **chart par-sha**, **pré-render imposé par Atlas** (Application ArgoCD en `directory.recurse`), **push déterministe** (pas de Renovate).

**Mécanique (validée end-to-end) :**
- **App CI** : `check → build → publish-chart (0.0.0-sha.<sha>)` → jobs **`trigger`** factorisés via un job caché `.deploy-trigger` (`extends`) : `deploy-preprod` (sur `$CI_DEFAULT_BRANCH`), `deploy-review` (`feat|fix|chore|migration/*`), `stop-review` (manuel, `needs: []`). Ils **déclenchent le pipeline gitops** (`trigger: project: <gitops>`, `strategy: depend`) en passant `DEPLOY_ACTION`(**preprod**|review|stop-review) / `SLUG` / `CHART_VERSION` (variable **globale**, 1 source). **Pas de `git push`, pas de `GITOPS_TOKEN` côté app.**
  - ⚠ **Nommage** : `DEPLOY_ACTION=preprod` ⇄ dossier gitops `apps/da-manager-preprod/`. (Historiquement `dev` ⇄ `da-manager-preprod` — incohérence corrigée : `dev`→`preprod` partout, app + `case` gitops, coordonné.)
- **Autorisation repo→repo** = `CI_JOB_TOKEN` + **allowlist inbound** du gitops (Settings › CI/CD › *Job token permissions* : autoriser le projet app). ⇒ un user avec droits **app seulement** déclenche le commit+render gitops, sans secret partagé. *(C'est aussi ça qui satisfait « un dev sans accès gitops peut quand même déployer ».)*
- **Gitops CI** : job **`apply`** (selon `DEPLOY_ACTION` : bump version preprod / `cp -a` template review + bump / `rm` du dossier) → **commit sur `main`** via `RENDER_TOKEN` (Maintainer, `push -o ci.skip` pour éviter la boucle) ; job **`render`** (clone `main` **frais** pour voir le commit d'apply → `helm dependency update` + `helm template` → branche `rendered`). `apply`/`cleanup` partagent un `resource_group: gitops-main` (sérialise les pushers concurrents). `configure-gitops` : `ref=rendered, path=<env>`.

**Axe OPS (ressources/secrets/scaling/host)** : édition **directe du gitops** → render → ArgoCD. Aucun pipeline app. Changer une *valeur* de secret = écrire dans **Vault** (ESO resync), même pas un commit.

**Render gitops** : `helm dependency update` (PAS `build`) → régénère `Chart.lock` ⇒ robuste au bump. Clone `main` **frais** (sinon le render d'un pipeline déclenché ne verrait pas le commit d'`apply`). Skippe un dossier sans `Chart.yaml` (dossier incomplet).

**Reloader** : annotation opt-in `reloader.stakater.com/auto: "true"` (operator stakater présent) → auto-restart du pod sur changement Secret/ConfigMap. Off par défaut dans le chart, on dans les values gitops. ⇒ rotation secret / changement config **sans rollout manuel**.

**Tokens & rotation** : côté app **aucun secret gitops** (trigger via `CI_JOB_TOKEN`). Côté gitops : `RENDER_TOKEN` (write, commit `main` + push `rendered`), `OCI_PULL_TOKEN` (read_registry, pull chart cross-projet), deploy token `read_repository` (ArgoCD lit le repo via `configure-gitops`). HTTPS, **jamais SSH**. ⚠ **rotation manuelle** (tokens à expiration). Le **create/delete d'env** en CI est désormais débloqué par les **PAT Atlas** ([atlas-pat-machine-identity](atlas-pat-machine-identity.md)) ; la **fédération OIDC GitLab** (RFC : `.plans/review-env/atlas-review-envs-synthese.md`) reste l'évolution qui éliminerait aussi ces tokens gitops persistants.

## Le pattern à deux branches (`main` sources / `rendered` manifests) — pourquoi & pièges

**Pourquoi un pré-render dans une branche `rendered`** (et non ArgoCD pointé direct sur les sources Helm) :
- **Contrainte plateforme Atlas** : l'`Application` ArgoCD créée par `configure-gitops` lit en
  **`directory.recurse`** (manifests plats), pas en mode Helm. Le repo gitops doit donc fournir du
  **YAML déjà rendu** à `path` → d'où le `helm template` → branche `rendered`.
- **Bénéfices** assumés : déploiements **auditables** (diff lisible des manifests rendus), **reproductibles**
  (chart OCI + version épinglée), et **découplage** net sources/rendu. **C'est voulu tel quel** (pas de bascule
  vers ArgoCD-Helm/OCI natif ou multi-source prévue).

**Pièges de timing à connaître** (gravés dans le CI gitops, sinon invisibles) :
- **`apply` commit en `[skip ci]` + `push -o ci.skip`** : le render tourne **dans le même pipeline** que l'apply
  (stages `apply`→`render`), donc le push d'apply ne doit PAS redéclencher un pipeline (sinon boucle).
- **`render` clone `main` FRAIS** : sur un pipeline déclenché, `apply` vient de committer sur `main` ; un
  checkout du SHA du pipeline ne verrait pas ce commit → on re-clone `main` pour rendre l'état à jour.
- **`render` exclut `$CI_PIPELINE_SOURCE == "schedule"`** : `cleanup-review` (schedule) pousse sur `main`,
  ce qui déclenche **un** render normal — on évite donc de rendre une 2ᵉ fois dans le pipeline schedule lui-même.
- **`render` tolérant par-env** : un env KO (chart OCI absent, template cassé) n'arrête pas la boucle, **conserve**
  son ancien `manifests.yaml` (prune ciblé : on ne retire de `rendered` que les envs **absents de `main`**) →
  pas de prune ArgoCD d'un workload vivant pour un échec transitoire.

**Contrat du descripteur `atlas-env.yaml`** (un par dossier review, déposé par `atlas-env.sh create`) :
```yaml
atlasEnv:
  id: env-<ulid>            # envId Atlas -> namespace + segment de host
  zoneDomain: dev.atlas-prod.public-cloud.social.gouv.fr
```
Le render lit `id`/`zoneDomain` et calcule `host = <dossier>.<id>.<zoneDomain>` (`--set-string …ingress.host`).
Descripteur absent ⇒ env Atlas pas encore créé ⇒ le dossier review est **skippé** (invariant : pas d'état cassé).
Le préprod, lui, a son host en dur dans `values.deploy.yaml` (env permanent).

## 🪤 Piège — renommer une clé de `values` : la config disparaît EN SILENCE

Le pattern sépare le **chart** (repo app, publié en OCI) des **values** (repo gitops). Les deux sont versionnés **indépendamment** : le gitops épingle une version de chart. Or **Helm ignore silencieusement toute clé de values qu'un chart ne connaît pas** — aucune erreur, aucun avertissement, le rendu reste valide.

Conséquence : dès qu'on **renomme (ou déplace) une clé** — `services.x` → `cronjobs.x`, `foo` → `foo-a`/`foo-b` — il existe une fenêtre où le chart déployé et les values ne parlent plus de la même clé. Le composant est alors rendu **sans sa configuration** : plus d'URL de base, plus de variables… et l'erreur ne ressemble pas du tout à la cause (`Could not get Connection`, `Failed to determine DatabaseDriver`).

**Constaté sur daccord (2026-07-31)** : `batch-transfert` déplacé de `services:` vers `cronjobs:`, puis scindé en `batch-transfert-dila`/`-urssaf`. Le gitops (values) a été poussé **avant** que le chart correspondant ne soit déployé ⇒ le CronJob déployé s'est retrouvé avec **0 variable d'environnement** et a échoué à se connecter à la base.

**Règle** : lors d'un renommage de clé, pousser **le chart d'abord**, déployer, **puis** les values — ou vérifier après coup que la clé est bien consommée :
```bash
kubectl -n <envId> get cronjob <nom> \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].env[*].name}'
# vide alors qu'on attend des variables => la clé de values ne correspond plus au chart
```

## Environnements de review par branche

- **Provisioning (humain, 1 commande)** : `scripts/atlas/atlas-env.sh create <slug>` (générique tout-produit, config en tête) → crée l'**Atlas Environment** (zone dev) + `configure-gitops(path=<app>-review-<slug>)` + dépose le **descripteur** `apps/<app>-review-<slug>/atlas-env.yaml` (`{envId, zoneDomain}`) + **seed** des secrets bootstrap (vault→vault depuis un env de référence). C'est le **seul acte privilégié/humain** (token PKCE) ; le reste est automatique.
- **Host calculé par le render** depuis le descripteur : `host = <dossier>.<envId>.<zone-domain>` — jamais écrit en dur ⇒ **pas besoin de débloquer Kyverno** (reste sous le domaine autorisé de l'env). Le render skippe un dossier review **sans descripteur** (env pas encore créé) → pas d'état cassé.
- **Déploiement continu** : `push` branche → `deploy-review` (trigger) → gitops copie le template + bump → render → ArgoCD. Le **descripteur n'est jamais écrasé** (`cp -a`, pas de `rm -rf`).
- **Teardown** : `atlas-env.sh delete <slug>` (env Atlas + dossier — teardown complet). **Auto-cleanup** : pipeline schedule gitops **`cleanup-review`** (quotidien) qui purge les dossiers review dont la **branche a disparu** OU **inactifs > 7j** (fenêtre glissante, reset à chaque deploy) → prune le workload ; il **liste** les `atlas-env.sh delete` restants. ⚠ le `delete` Atlas peut laisser une **Application ArgoCD orpheline** (finalizer bloqué) — à nettoyer côté ArgoCD.
- **Secrets review (Vault)** : un token **ws-admin** couvre les **nouveaux** envs via ses `identity_policies` (**pas de re-login**) ; le write peut échouer ~1-3 min post-create (mount en provisioning) → le script **retry**.

> **Fait** : ce câblage (trigger + allowlist, descripteur, `atlas-env.sh`, cron cleanup) est désormais packagé en **composant CI PIC partagé** — voir [gitops-cd-component](gitops-cd-component.md) (validate/apply_chart/apply_app/render/trigger_chart/trigger_app, réutilisables par `include: component:`). Il documente aussi le découpage cible à **trois repos** (app/charts/gitops, cf. § Découplage plus haut) pour cloisonner les droits d'écriture sur le chart.

## À détailler / rétro-analyser au besoin
- Gestion des **IDs de ressources** dans les noms de secrets Vault (cf. [history](../00-context/history.md)) — pour les ressources OVH managées (CNPG in-cluster les évite).
