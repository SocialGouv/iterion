---
name: gitops-repo-standard
description: The gitops repo standard: branch/merge policy, the validate job, OPS-vs-MR modes, the three-repo chart boundary — what an MR on a gitops repo must respect
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# Norme des dépôts gitops PIC (convention `*-gitops`)

Configuration **standard** appliquée à tout dépôt suivant la convention **`*-gitops`** (repo CD : wrapper Helm par env + branche `rendered` lue par ArgoCD). But : uniformiser la politique de merge et la protection des branches sur tous les repos gitops, présents et **futurs**.

> Outil : **[`scripts/gitlab/gitops-standard.sh`](../../scripts/gitlab/gitops-standard.sh)** — idempotent, **dry-run par défaut**. À lancer **à chaque création** d'un nouveau `*-gitops` (et pour aligner l'existant).

## Réglages appliqués

**Politique de merge** (API GitLab `PUT /projects/:id`) :

| Réglage | Valeur | Effet |
|---|---|---|
| `merge_method` | `ff` | **fast-forward only** (historique linéaire, pas de merge commit) |
| `squash_option` | `always` | **squash imposé** (1 commit par MR) |
| `only_allow_merge_if_pipeline_succeeds` | `true` | **pipeline vert obligatoire** avant merge |
| `only_allow_merge_if_all_discussions_are_resolved` | `true` | **tous les threads résolus** avant merge |

**Branches protégées** (`POST /projects/:id/protected_branches`) — appliquées **si la branche existe** :

| Branche | push | merge | force-push | Pourquoi |
|---|---|---|---|---|
| `main` | Maintainer (40) | Maintainer (40) | ❌ | source de vérité (apps/env + versions) |
| `rendered` | Maintainer (40) | Maintainer (40) | ❌ | **écrite par la CI** (job render) — voir CI-safe |
| `develop` | No one (0) | **Developer (30)** | ❌ | MR-only, mais les **developers peuvent merger** |

> Les repos gitops n'ont en pratique **que `main` + `rendered`** (pas de `develop`) ; la règle `develop` est là pour les repos qui en ont une (ex. repos *app*).

## ⚠ CI-safe : pourquoi push = Maintainer (et pas « No one »)

Le job **`apply`** commite sur `main` (`push_retry main -o ci.skip`) et le job **`render`** pousse `rendered` — **tous deux via un token de projet Maintainer** (`RENDER_TOKEN`/`cd-gitops`/…). Cf. `.gitlab-ci.yml` des gitops (réf. [`da-manager-gitops`](../../.repos/da-manager-gitops)).

Sur **GitLab CE**, une branche protégée **ne distingue pas** un bot CI d'un humain au niveau *push* (l'`allowed_to_push` par utilisateur est Premium). Donc :
- **push doit rester ≥ Maintainer** sur `main`/`rendered`, sinon **les pipelines cassent** (`apply`/`render` ne peuvent plus pousser).
- Conséquence assumée : un **humain Maintainer** peut encore pousser directement sur `main` — c'est d'ailleurs le **mode OPS** existant (« push direct main / édition manuelle »). La qualité reste tenue par **MR + pipeline vert + threads résolus + squash + ff**.
- `allow_force_push=false` : le render pousse en **rebase+retry** (jamais `--force`) → aucune régression.

**Prérequis à la création d'un `*-gitops`** : le token CI de render doit être **Maintainer** (déjà le cas partout sauf le doublon stale `407`).

## Usage

```bash
# Aperçu (dry-run) — n'écrit rien :
bash scripts/gitlab/gitops-standard.sh                 # les 10 *-gitops connus
bash scripts/gitlab/gitops-standard.sh 588             # un repo précis

# Appliquer :
bash scripts/gitlab/gitops-standard.sh --apply 588     # à la création d'un nouveau gitops
bash scripts/gitlab/gitops-standard.sh --apply         # aligner tous les *-gitops
```

Le script compare l'état courant à la cible et n'agit que sur les écarts (re-run → `0 écart`). Pour ajouter un nouveau repo à la liste par défaut : éditer `DEFAULT_TARGETS` en tête de script.

## Périmètre appliqué (2026-07-23)

10 dépôts `*-gitops` : `basavi-gitops` (588), `vaultwarden-gitops` (549), `egapro-gitops` (537), `srdt-gitops` (533), `cdtn-gitops` (527), `ptt-gitops` (447), `graal-gitops` (446), `da-manager-gitops` (434), `graal-gitops` **doublon stale** `studio-tech/devops` (407 — token Developer, candidat à l'archivage), `vao-gitops` (293).

- **Note gouvernance** : plusieurs de ces repos appartiennent à des **équipes produit** — norme appliquée en tant qu'Owner de groupe, à **communiquer** aux équipes.

## 🪤 La norme rend toute MR humaine impossible à merger — sauf job `validate`

`only_allow_merge_if_pipeline_succeeds=true` + **aucun job éligible à un `merge_request_event`** ⇒ `head_pipeline: null` et `detailed_merge_status: **ci_must_pass**` : la MR n'est pas « en attente », elle est **définitivement** non mergeable, et **aucun pipeline n'apparaît** dans l'UI (d'où le « je ne peux pas merger » sans message d'erreur). Le patron gitops n'a par construction aucun job MR : `apply` → `$DEPLOY_ACTION`, `render` → branche par défaut, `cleanup-review` → schedule. Invisible tant que le repo vit en **push direct sur `main`** (mode OPS) — ça mord dès qu'une équipe produit ouvre une MR.

**Correctif** : un job **`validate`** (`rules: if: $CI_PIPELINE_SOURCE == "merge_request_event"`, stage dédié) qui rend chaque `apps/<env>/` **de la branche** avec la logique de `render` — même login OCI, mêmes injections host/`s3Path` depuis `atlas-env.yaml`, mêmes skips — **sans écrire** sur `main`/`rendered`, et publie le rendu en artefact `rendered-preview/` (relecture de MR). Implémenté sur **basavi-gitops (588)** puis **graal-gitops (446)**.

- ⚠ Un pipeline de MR utilise le `.gitlab-ci.yml` de la **branche source** : après avoir ajouté `validate` sur `main`, les MR déjà ouvertes doivent être **rebasées** avant que `POST /projects/:id/merge_requests/:iid/pipelines` ne produise autre chose que *« The resulting pipeline would have been empty »*.
- ⚠ `merge_method: ff` ⇒ chaque MR doit de toute façon être rebasée (`PUT …/merge_requests/:iid/rebase`) dès qu'une autre a mergé.
- **État au 2026-08-27** : `validate` posé sur **les 9** `*-gitops` (588, 446, puis 549 · 537 · 533 · 527 · 447 · 434 · 293 en une passe). Chaque job est **dérivé du `render` du repo** (mêmes injections `host`/`s3Path` depuis `atlas-env.yaml`, mêmes skips, même login OCI) — pas un job générique : le rendu validé est bien celui que `render` produira. Commits poussés avec **`[skip ci]`** (un push sur `main` déclencherait `render`, donc un déploiement non demandé). Vérifié de bout en bout par une MR jetable sur `da-manager` (pipeline vert, chart OCI tiré, `da-manager-preprod` rendu), puis fermée.

Voir aussi : [pic-gitlab.md](pic-gitlab.md) · [20-cd-pattern/overview.md](../20-cd-pattern/overview.md).

# gitops-cd-component — composant CI/CD réutilisable (pattern app / chart / gitops)

> Source de vérité : le repo du composant lui-même —
> `$CI_SERVER_FQDN/socialgouv/produits-dnum/studio-tech/devops/components/gitops-cd-component`
> (PIC, cloné dans [`.repos/gitops-cd-component`](../../.repos/gitops-cd-component/) — brique CI
> partagée, cf. [`scripts/setup/repos.tsv`](../../scripts/setup/repos.tsv)). Son `README.md`
> documente exhaustivement les 6 templates, leurs inputs et le versionnement ; cette fiche n'en
> est qu'une distillation orientée décision. Première implémentation de référence : **poss**
> (extraction du chart en repo dédié le 2026-08-27).

## Pourquoi un composant (vs le mécanisme main-rolled de `overview.md`)

Le [pattern CD cible](overview.md) (app ⇄ gitops, trigger multi-projet, `apply`/`render`, branche
`rendered`) a d'abord été implémenté **à la main** par produit (da-manager, graal, egapro…).
`overview.md` anticipait déjà cette suite : *« au 2ᵉ-3ᵉ produit, envisager un composant CI PIC
partagé »*. **gitops-cd-component** est cette généralisation : les mêmes jobs (`validate`/
`apply`/`render`) packagés en [GitLab CI/CD Components](https://docs.gitlab.com/ci/components/)
réutilisables par `include: component:`, versionnés (`@X.Y.Z`), au lieu d'être recopiés/adaptés à
la main dans chaque nouveau repo gitops.

## Les 6 templates — deux flows découplés

Le composant sépare explicitement le cycle de vie du **chart** de celui de l'**image**, chacun
avec son propre trigger + job d'apply, déclenchés sur leur propre variable — **pas de
branchement "mode" à lire**, les deux flows coexistent sans jamais s'interférer :

| Flow | Repo trigger | Composant trigger | Variable posée | Composant apply (côté gitops) |
|---|---|---|---|---|
| Chart | `<produit>-charts` (ou `<produit>` si pas de repo dédié) | `trigger_chart` | `$CHART_VERSION` | `apply_chart` — bump la version de dépendance dans `Chart.yaml` |
| Image | `<produit>` | `trigger_app` | `$IMAGE_TAG` | `apply_app` — bump un champ `values.deploy.yaml` (typiquement `.<produit>.image.tag`) |

Plus deux composants génériques, communs aux deux flows (déjà décrits dans `overview.md`, ici
packagés en composant) :
- `validate` — dry-run `helm template` de tous les `apps/<env>/`, sur MR.
- `render` — rend la branche par défaut vers `rendered` (lue par ArgoCD), tolérant par-env.

`apply_chart`/`apply_app` peuvent coexister dans le MÊME repo gitops sans jamais interférer
(rules mutuellement exclusives sur `$CHART_VERSION` vs `$IMAGE_TAG`) ; `render` les attend tous
les deux en `needs: optional: true` — un seul tourne réellement par pipeline, selon quel repo a
déclenché.

## Trois repos — pattern cible unique

**Décision (2026-09-01) : le pattern à 2 repos ne se fait plus pour aucun produit.** Tout produit
— migration nouvelle ou déjà faite — a vocation à finir en **3 repos** : `<produit>` /
`<produit>-charts` / `<produit>-gitops`.

Motivation : les permissions GitLab sont **repo-scopées, pas path-scopées** — un dev avec accès
au repo app peut éditer n'importe quel chemin, y compris `charts/<produit>/`. Extraire le chart
dans un repo dédié est la seule façon de **cloisonner les droits d'écriture sur le chart** aux
devops, quel que soit le produit. Ce n'est donc plus une option ponctuelle mais la cible pour
tout le monde.

Répartition des rôles :
- `<produit>` (repo applicatif) — build + publish l'image, `trigger_app` ($IMAGE_TAG) à chaque
  commit.
- `<produit>-charts` (repo chart dédié) — publie le chart Helm en OCI, `trigger_chart`
  ($CHART_VERSION) uniquement quand LE CHART change (rare).
- `<produit>-gitops` — reçoit les deux triggers, `apply_chart`/`apply_app`/`validate`/`render`.

Référence validée : **poss** (2026-08-27) — premier produit passé en 3 repos, chart extrait de
`poss/charts/poss/` vers un repo `poss-charts` dédié.

**Produits déjà migrés en 2 repos (da-manager, graal, egapro, srdt, vao, cdtn, basavi…)** : le
pattern 2 repos qu'ils utilisent aujourd'hui est **désormais legacy**, à retrofiter vers 3 repos
(extraction du chart) — pas de calendrier fixé, à traiter au fil de l'eau produit par produit. Le
composant `gitops-cd-component` supporte les deux formes techniquement (le retrofit est une
opération d'extraction de chart + bascule CI, pas un changement du composant) ; ce qui change,
c'est la **cible documentée**.

## Versionnement

Tag `@X.Y.Z` (sans préfixe `v`), jamais `@main` — `main` sert uniquement au développement du
composant, un changement dessus n'impacte aucun consommateur tant qu'il n'a pas explicitement
bumpé son `@X.Y.Z`. Semver classique : patch = correctif sans changement d'interface, minor =
ajout rétrocompatible (ex. l'extraction historique de `apply`/`trigger` uniques en
`apply_chart`/`apply_app`/`trigger_chart`/`trigger_app`), major = casse un consommateur existant.

## À rétro-analyser au besoin

Retourner lire son code dans [`.repos/gitops-cd-component`](../../.repos/gitops-cd-component/) en
cas de doute sur un comportement précis (résolution des inputs `$[[ inputs.x ]]`, scripts bash des
jobs `apply_*`, tests dans `tests/`).
