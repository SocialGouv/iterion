---
name: context-and-access
description: Platform context: what product teams are given self-service, the vocabulary (PIC, ESO, CNPG, XRD), and a known-good gitops shape to diff against
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# Programme de migration — vue d'ensemble

## Objectif

Migrer des produits (provenant de sources diverses) vers deux plateformes mutualisées :

- **CI sur la PIC** (GitLab) — tests, build d'images, qualité/sécurité, via des composants CI partagés.
- **CD sur Atlas v2** — déploiement Kubernetes piloté par API (Crossplane + ArgoCD), avec secrets via Vault + External Secrets.

La migration remplace l'ancienne plateforme **Fabrique** (kontinuous + sealed secrets) et **Atlas v1** (gitops sur CRDs Fabrique + Vault/ExternalSecrets). Voir [history.md](history.md) et la fiche [atlas-v1.md](../10-platforms/atlas-v1.md).

## Principe de découplage (architecture cible)

Deux dépôts par produit, avec des responsabilités nettes :

1. **Repo applicatif** (le code) — contient un **chart Helm plateforme-agnostique** aligné avec le code de l'app, **publié en artefact OCI** versionné. Le chart ne connaît ni l'environnement, ni les secrets, ni la plateforme : il expose des *values* et référence les secrets **par nom**.

2. **Repo gitops** (le déploiement) — pour chaque environnement, un **wrapper chart** qui dépend de la version OCI épinglée du chart applicatif et apporte tout le **spécifique-instance** :
   - valeurs par environnement (host, ressources, ConfigMap…),
   - **External Secrets** (chargeurs de secrets depuis Vault),
   - le **set de ressources** d'infra (DB, buckets, cache…) provisionnées côté Atlas,
   - la CI du repo gitops **rend les manifests** (`helm template`) que la plateforme (ArgoCD) synchronise.

Bénéfices : le chart suit le cycle de vie du code ; les déploiements sont auditables (manifests rendus) ; les secrets et l'infra restent hors du code applicatif.

## Répartition CI / CD

| Cas | CI | CD |
|---|---|---|
| Produit standard | PIC GitLab (build + tests, briques `studio-tech-commun/sdpsn-devops-ci-utils`) | Atlas v2 (gitops + ArgoCD) |
| Outillage (ex. **da-manager**) | **Tout-GitLab PIC** (check + build + publish chart + release ; plus de GitHub) | Atlas v2 (gitops + ArgoCD), trigger côté PIC |

## Produit pilote

- **da-manager** — outillage (form builder « Document d'Architecture », Next.js + Postgres). Premier produit migré, en mode « dev d'abord ». Runbook : [30-migrations/da-manager.md](../30-migrations/da-manager.md).
- **basavi** — déjà sur l'approche build PIC (composant docker-image + flows Kestra). Référence pour la CI PIC.

## Règle secrets (transversale)

Les secrets locaux sont dans `.secrets/` et **ne doivent jamais être lus en clair**. On les **charge** uniquement (`set -a; source .secrets/.env; set +a`). Détails dans le `CLAUDE.md` racine et [10-platforms/pic-gitlab.md](../10-platforms/pic-gitlab.md).

# Glossaire

| Terme | Définition |
|---|---|
| **PIC** | Plateforme d'Intégration Continue — instance GitLab partagée (`<forge>`) : composants CI réutilisables + registre d'images. |
| **Atlas v2** | Plateforme de déploiement Kubernetes pilotée par API (Crossplane + ArgoCD + Vault + ESO). Cible de la CD. |
| **Atlas v1** | Génération précédente d'Atlas : provisionnement des ressources via **CRDs Fabrique** (groupes `org/workspace.fabrique.social.gouv.fr`) en gitops, secrets via **Vault + ExternalSecrets** (pas Sealed Secrets — c'est en *Fabrique legacy* qu'on en trouvait). Remplacée par v2 (provisionnement par **API**, clés Vault par ID au lieu de nom). Voir [atlas-v1.md](../10-platforms/atlas-v1.md). |
| **Fabrique** | Ancienne plateforme (« plateforme Fabrique »), déploiements via **kontinuous** + **sealed secrets**. Kubeconfig legacy : `.secrets/plateform-fabrique/kubeconfig` (contexts `ovh-dev`, `ovh-prod`). |
| **kontinuous** | Outil de déploiement Helm de l'ère Fabrique (dossier `.kontinuous/` dans les repos). Remplacé par le pattern gitops + ArgoCD. |
| **Crossplane** | Operator Kubernetes qui expose des ressources d'infra comme des objets K8s (XRD/Compositions). Cœur d'Atlas v2 (control-plane). |
| **XRD** | *Composite Resource Definition* Crossplane — définit le schéma d'une ressource composite (ex. `Environment`, `Workspace`, `Zone`). |
| **Composition** | Implémentation d'une XRD : ce qui est réellement créé quand on instancie la ressource. Sur Atlas, générées par des fonctions TypeScript. |
| **Workspace / Environment / Zone** | Modèle de ressources Atlas v2 : `Organization → Workspace → Environment` ; `Zone` = cluster Kubernetes de charge. Voir [atlas-v2.md](../10-platforms/atlas-v2.md). |
| **ArgoCD** | GitOps continuous delivery — synchronise les manifests d'un repo gitops vers le cluster. Utilisé pour les **apps produit** sur Atlas v2. |
| **Flux** | GitOps controller utilisé pour les **composants plateforme** (control-plane / workload-zone) d'Atlas v2. |
| **Vault** | Coffre-fort de secrets (HashiCorp Vault ; OpenBao présent comme alternative). Backend des secrets sur Atlas v2 ; accessible par zone (`vault.<zone>.<domain>`). |
| **ESO / External Secrets** | External Secrets Operator — synchronise des secrets depuis Vault vers des `Secret` Kubernetes via `ExternalSecret` / `SecretStore`. **Remplace les sealed secrets** sur Atlas v2. |
| **Sealed Secrets** | (Legacy) secrets chiffrés committables (Bitnami). Utilisés sur **Fabrique legacy** (avant Atlas) ; en Atlas v1 **et** v2, on est passé à Vault + ExternalSecrets. |
| **CNPG** | CloudNativePG — operator PostgreSQL in-cluster. Disponible en workload-zone ; alternative au Postgres OVH managé. |
| **OCI** | Format d'artefact (registre). Les charts Helm sont publiés/consommés en `oci://…`. |
| **Composant CI (GitLab)** | Brique de pipeline réutilisable, incluse via `include: component: $CI_SERVER_FQDN/<projet>/<composant>@<version>`. |
| **Dependency Proxy** | Cache d'images Docker Hub fourni par GitLab, utilisé par les composants build PIC. |
| **ProConnect** | Fournisseur d'identité OIDC de l'État (auth des utilisateurs de da-manager). À ne pas confondre avec le Keycloak d'Atlas (auth de l'**API** plateforme). |
| **Keycloak (Atlas)** | IdP OIDC du control-plane Atlas : émet les jetons d'accès à l'**API Atlas**. |
| **Kestra** | Orchestrateur de workflows (ETL) déployé sur Atlas ; utilisé par basavi. |

# Accès self-service Atlas v2 pour une équipe produit (kubectl + Vault)

Fiche **destinée aux équipes produit** (pas seulement aux devops) : accéder en lecture/debug au cluster de son environnement, et lire/écrire ses secrets Vault — **sans manipuler de token**, sans ce repo.

> Tout repose sur l'**OIDC** : `kubelogin` et `vault` obtiennent eux-mêmes le jeton (login navigateur, puis refresh silencieux). On ne copie/colle jamais de secret. Cloisonnement : chaque membre ne voit que **les environnements de son workspace** (cf. [atlas-v2 § membres](../10-platforms/atlas-v2.md)).

## Prérequis (une fois)
- Outils : **`kubelogin`** (plugin `kubectl-oidc_login`), **`jq`**, **`curl`**, et **`vault`** (CLI) pour les secrets.
  - macOS : `brew install int128/kubelogin/kubelogin jq vault` · krew : `kubectl krew install oidc-login`.
- **Compte Keycloak `atlas-prod`** : s'être connecté ≥ 1× et avoir l'**email vérifié** (sinon l'apiserver renvoie `401` — il utilise `username-claim = email`).
- **Rôles sur le workspace** (à demander au devops) : `editor` (agir + écrire les secrets) **+** `secrets_viewer` (lire les valeurs de secrets). ⚠ `secrets_viewer` **seul** ne donne **ni** kubectl/ArgoCD/Grafana **ni** l'écriture — il se cumule. Détail du modèle : [atlas-v2 § membres](../10-platforms/atlas-v2.md).
- ⚠ **Après toute attribution/changement de rôle, se reconnecter** (ArgoCD/Grafana/Vault/kubectl) : les droits viennent de **groupes OIDC figés au login** — un jeton émis avant le changement ne les porte pas.

Les exemples ci-dessous ciblent la **zone `dev`**. Pour la prod : remplacer `dev` par `prod` (client **du kubeconfig** `prod-kubernetes`, `vault.prod.…`). La commande de récupération du kubeconfig, elle, garde **`control-plane-atlas-portal`** dans les deux cas (client control-plane, commun aux zones).

## kubectl — 2 commandes

**1. Récupérer le kubeconfig OIDC** (il ne contient **aucun secret** : URL apiserver + CA public + config OIDC). Commande autonome — `kubelogin` mint le jeton, `curl` télécharge le kubeconfig :

```bash
curl -sS -H "Authorization: Bearer $(kubectl oidc-login get-token \
  --oidc-issuer-url=https://<atlas-host>/realms/atlas-prod \
  --oidc-client-id=control-plane-atlas-portal \
  --oidc-extra-scope=profile --oidc-extra-scope=email --oidc-extra-scope=groups \
  | jq -r .status.token)" \
  https://<atlas-host>/api/v1/kubeconfig > ~/.kube/atlas.yaml
```

*(Le 1ᵉʳ `oidc-login` ouvre le login Keycloak dans le navigateur — `http://localhost:8000` — puis met en cache.)*
> ⚠️ **Deux clients OIDC distincts, ne pas confondre** :
> - **`control-plane-atlas-portal`** = client du **portail / API Atlas** (control-plane). C'est celui à utiliser pour appeler l'API (ici `/api/v1/kubeconfig`).
> - **`dev-kubernetes`** (resp. `prod-kubernetes`) = client de l'**apiserver de la zone**, présent dans le bloc `exec` **à l'intérieur** du kubeconfig téléchargé — utilisé par `kubectl` (étape 2), pas ici.
Alternative sans token du tout : ce fichier étant secret-free et **identique pour toute la zone**, un collègue qui l'a déjà peut simplement te l'envoyer.

**2. Utiliser kubectl** (le login OIDC est déclenché automatiquement par le plugin, puis silencieux) :

```bash
export KUBECONFIG=~/.kube/atlas.yaml
kubectl --context dev -n <envId> get pods           # <envId> = nom de ton environnement (env-…)
kubectl --context dev -n <envId> logs deploy/<app> -f
```

- Droits = ton rôle, **sur le namespace de ton env uniquement**.
- ❌ `get/list secrets` est **interdit**. Pour voir les clés produites par ESO : `get externalsecrets` puis `-o yaml` (la **spec** est lisible, pas les valeurs).
- Réinitialiser la session : `kubectl oidc-login clean`.
- Rendre permanent (sans `export`) : fusionner une fois — `KUBECONFIG=~/.kube/config:~/.kube/atlas.yaml kubectl config view --flatten > ~/.kube/merged && mv ~/.kube/merged ~/.kube/config`.

## Vault — lire / écrire ses secrets

Les secrets applicatifs vivent dans **Vault** (pas dans le cluster en clair) et sont matérialisés dans le namespace par ESO. Pour les **modifier**, on écrit dans Vault.

```bash
export VAULT_ADDR=https://<atlas-host>
vault login -method=oidc            # login navigateur ; sinon UI /ui (méthode OIDC)

M=<wsId>/<envId>/kv                  # mount KV v2 de ton env (wsId = workspace, envId = environnement)
vault kv get   $M/<nom-du-secret>                       # lecture        (rôle secrets_viewer)
vault kv put   $M/<nom-du-secret> CLE=valeur AUTRE=...   # écriture/maj   (rôle editor)
vault kv patch $M/<nom-du-secret> CLE=nouvelle           # maj partielle  (rôle editor)
```

- `editor` ⇒ écriture KV ; `secrets_viewer` ⇒ lecture. Les deux se cumulent (lire **et** écrire).
- 🪤 **`403 invalid token` juste APRÈS un login réussi = une `VAULT_TOKEN` périmée traîne dans le shell.** La CLI Vault donne **priorité à la variable d'environnement** sur `~/.vault-token` : un `source .secrets/.env` fait plus tôt dans la session fige l'ancienne valeur, que le login ne remplace pas (il n'écrit que le fichier). Le login le signale d'ailleurs — `WARNING! The VAULT_TOKEN environment variable is set!` — et le `vault token lookup` du script échoue au passage (`lookup indisponible`). ⇒ `unset VAULT_TOKEN` (ou re-`source .secrets/.env`, mis à jour par le login), puis rejouer. Symptôme trompeur : l'erreur pointe `sys/internal/ui/mounts/…`, ce qui fait croire à un chemin de mount erroné alors que c'est le jeton.
- Login OIDC : préférer l'**UI** (`$VAULT_ADDR/ui`) si le callback CLI `localhost:8250` ne passe pas (devcontainer).
- Vérifier les policies du jeton : `vault token lookup -format=json | jq .data.policies` → doit lister `<envId>_editor` **et** `<envId>_secrets-viewer`. Si seul `secrets-viewer` apparaît après un changement de rôle → **se reconnecter**.

## Changer un secret → reboot automatique du pod

Chaîne : **Vault → ESO (au `refreshInterval`, ~1 min) → `Secret` k8s → Stakater Reloader → rollout**. Avec `reloader.stakater.com/auto: "true"` sur le Deployment (convention du repo), un secret modifié dans Vault **reboote le pod qui le référence en ≤ ~1 min**, sans action manuelle. Détail : [secrets via ESO § Propagation & auto-reboot](../20-cd-pattern/secrets-externalsecrets.md).

Pour forcer sans attendre :
```bash
kubectl --context dev -n <envId> annotate externalsecret <name> force-sync=$(date +%s) --overwrite
# ⚠ retirer l'annotation après usage (sinon OutOfSync ArgoCD permanent) :
kubectl --context dev -n <envId> annotate externalsecret <name> force-sync-
```
Vérifier que Reloader a déclenché : `kubectl --context dev -n <envId> get events | grep Reloaded`.

# Runbook — migration da-manager

> 1er produit migré (outillage « Document d'Architecture »), **tout-GitLab PIC**, **déployé et validé sur Atlas v2 (zone dev)**. Runbook actionnable de l'état cible. Chronologie / itérations : [historique](../90-historique/da-manager-journal.md). Source : [`.repos/da-manager`](../../.repos/da-manager/) + [`.repos/da-manager-gitops`](../../.repos/da-manager-gitops/).

## Produit
- **App** : Next.js 16 / React 19, **pnpm**, **Node 22**, Dockerfile multi-stage `output: standalone` (port 3000), probe `/api/healthz`.
- **DB** : PostgreSQL via **Drizzle** ; migrations **au boot** (`instrumentation.ts`, pas de Job). En cluster via **CNPG** (`Cluster da-manager-db`, connexion = secret `da-manager-db-app/uri`).
- **Auth** : **ProConnect** (OIDC) + **dev-login** (`ENABLE_DEV_LOGIN`). Secrets : `PROCONNECT_CLIENT_ID/SECRET`, `AUTH_SECRET`.
- **Config** : `ALLOWED_EMAIL_DOMAINS`, `PROCONNECT_ISSUER`, `AUTH_URL` + `NEXT_PUBLIC_ENABLE_DEV_LOGIN` (passés en **runtime**/ConfigMap → **une seule image** pour tous les env).

## Architecture cible (validée)
**Chart Helm agnostique** dans le repo app (publié **OCI**) ⇄ **repo gitops** (wrapper par env + rendered manifests + ressources) ⇄ **ArgoCD** sur Atlas v2. Pattern générique : [20-cd-pattern/overview.md](../20-cd-pattern/overview.md). Build : [BuildKit mutualisé](../15-build-ci/buildkit-mutualise.md). Secrets : [Vault + ESO](../20-cd-pattern/secrets-externalsecrets.md).

## Chart Helm — `charts/da-manager` (repo app)
Mono-service (simplifié du chart du PoC vao) :
- `Deployment` (port 3000, probes `/api/healthz`, `envFrom` ConfigMap + Secrets par **nom**), `Service`, `Ingress` (host depuis values), `ConfigMap`, `ServiceAccount`, HPA/PDB optionnels.
- `extraEnv` (avec `valueFrom`) + `envFrom` proconnect/database **conditionnels** (`secrets.database: ""` ⇒ pas d'envFrom DB).
- **Pas de Postgres dans le chart** : DB = CNPG porté par le gitops. Secrets référencés par nom : `proconnect`, `auth`, `pg-app`.

## CI build + publication (repo app, GitLab PIC primaire)
- `.gitlab-ci.yml` : `check` (lint/format + **tests** vitest+Postgres ; en CI `PGHOST=postgres`) → `build` + `publish-chart` (briques partagées [`ci-common`](../15-build-ci/buildkit-mutualise.md)) → `deploy-*` (trigger gitops) → `release` (semantic-release).
- **Build** : `extends: .build-buildkit-service` (include `…/studio-tech-commun/sdpsn-devops-ci-utils, file: ci-common/image-build.yml`). da-manager = outil interne ⇒ backend **service BuildKit mutualisé** (vars `BUILD_*` + `BUILDKIT_*` du service). L'autre backend `.build-dind` est le défaut produit (les runners PIC refusent le buildkit *rootless* ; cf. doc).
- **Publish chart** : `extends: .publish-chart` (`file: ci-common/publish-chart.yml`), `CHART_DIR=charts/da-manager`. Pousse `oci://$CI_REGISTRY/$CI_PROJECT_PATH/charts` (nom du chart dérivé de Chart.yaml).
- **Purge cache** : job manuel `buildkit-prune` (`file: ci-common/buildkit-prune.yml`) — utilisé une fois post-transfert pour casser le lineage `tooling/` sur le réplica buildkit (cf. [piège lineage](../15-build-ci/buildkit-mutualise.md#pièges-rencontrés)).
- Variables CI (mode **service**) : `BUILDKIT_DAEMON_ADDRESS` (`:443`), `BUILDKIT_SVC_COUNT`, `BUILDKIT_CERT_*`, `BUILDKIT_HTTP_PROXY`, `BUILDKIT_TUNNEL=1`. Toujours requis : `OCI_PULL_*`, `RENDER_TOKEN`, `SEMANTIC_RELEASE_TOKEN`. ⚠ `RENDER_TOKEN`/`SEMANTIC_RELEASE_TOKEN` ont dû être **recréés** (le transfert révoque les project access tokens). Convention secrets = **`masked_and_hidden`** (`BUILDKIT_CERT_*` en **base64** décodés au job) — cf. [credentials-and-access § variables CI](../00-context/credentials-and-access.md).

## CD — par **trigger** (app → gitops)
Aucun token gitops stocké dans l'app : les jobs `deploy-preprod` / `deploy-review` / `stop-review` (factorisés via `.deploy-trigger`, `extends`) **déclenchent** (`trigger`, `strategy: depend`) le pipeline du repo gitops via `CI_JOB_TOKEN` (+ **allowlist inbound** 434←433), en passant `DEPLOY_ACTION`(**preprod**|review|stop-review) / `SLUG` / `CHART_VERSION`.
- Côté gitops : job **`apply`** (commit sur `main` via `RENDER_TOKEN`, `[skip ci]`) + **`render`** (clone main frais → `helm dependency build` + `helm template` → branche `rendered` ; **host calculé depuis le descripteur** `atlas-env.yaml`).
- ArgoCD (path/branche pointés par `configure-gitops`) synchronise les manifests rendus.

## Environnement statique — `da-manager-preprod`
Env permanent de l'API Atlas, `configure-gitops(path=da-manager-preprod, ref=rendered)`, dossier gitops `apps/da-manager-preprod/` (host = `<envId>`-scoped, `ingressClassName: public`, CNPG `Cluster`, ExternalSecrets `proconnect`+`registry`). Le job CI `deploy-preprod` (`DEPLOY_ACTION=preprod`) cible ce dossier.

## Environnements de review par branche — `scripts/atlas/atlas-env.sh`
Script générique (`create`/`seed`/`delete`/`ls`, prefix `da-manager-review-`, `--dry-run`) :
- `create <slug>` = **seul acte privilégié/humain** (token PKCE) : Atlas Environment + `configure-gitops(path=da-manager-review-<slug>)` + descripteur `apps/da-manager-review-<slug>/atlas-env.yaml` (`{envId, zoneDomain}`) + **seed** Vault (`registry`, `proconnect`) vault→vault depuis preprod.
- `deploy-review` copie le template + bump (descripteur préservé, `cp -a`) ; le **render calcule le host** (`<dossier>.<envId>.<zone-domain>`) ⇒ pas besoin de débloquer Kyverno. DB = **CNPG par branche**.
- **Purge** : schedule gitops `cleanup-review` (quotidien, `REVIEW_TTL_DAYS=7`) — supprime les dossiers dont la branche a disparu OU inactifs >7j (prune workload + **liste** les `atlas-env.sh delete` restants).

> ⚠ **Seed d'un env neuf** : il faut un **login Vault POSTÉRIEUR à la création** de l'env (la policy `<envId>_admin` est ajoutée **au login** depuis les groupes OIDC). Après `create` : re-login Vault puis `atlas-env.sh seed <slug>`. Cf. [atlas-v2 § Vault](../10-platforms/atlas-v2.md).

## Secrets (Vault + ESO)
Par env, mount `<wsId>/<envId>/kv` : `proconnect` (`PROCONNECT_CLIENT_ID/SECRET` + `AUTH_SECRET`) ; `registry` (`.dockerconfigjson`, deploy token `read_registry` projet 433). DB CNPG ⇒ secret `da-manager-db-app` in-cluster (pas Vault). Le gitops porte les `ExternalSecret` (défauts ESO explicités, cf. [secrets-externalsecrets.md](../20-cd-pattern/secrets-externalsecrets.md)).

## Vérification
- `helm template charts/da-manager -f <values>` valide (`kubeconform`).
- Atlas : Environment `Ready`, ArgoCD `Synced/Healthy`, `ExternalSecret` `SecretSynced`, pod `Ready`, **migrations Drizzle OK**, login dev-login/ProConnect sur le host.
- `kubectl --context dev` opérationnel (kubeconfig API + kubelogin ; `scripts/atlas/build-kubeconfig.sh` pour le non-interactif).

## Pièges réutilisables (versions génériques : [atlas-v2.md](../10-platforms/atlas-v2.md))
- **Ingress** : `ingressClassName: public` (pas `traefik`) — sinon pas de LB ⇒ DNS absent (NXDOMAIN).
- **Kyverno `disallow-unknown-domains`** : le host doit être ⊆ annotation ns `atlas.social.gouv.fr/domains` (= `workspace.domains` + `<envId>.<zone-domain>`). Par défaut : host sous le domaine de l'env.
- **LimitRange** : `requests.memory / limits.memory ≤ 2` (ex. `512Mi`/`1Gi`).
- **ExternalSecret OutOfSync** (diff cosmétique) : expliciter les défauts ESO (`deletionPolicy`, `conversionStrategy`, `decodingStrategy`, …).
- **TLS** : renseigner `ingress.tls[].secretName` sinon cert-manager n'émet pas de certificat (cert auto-signé).
- **kubectl dev** : l'email du user Keycloak doit être **vérifié** (`username-claim=email` ⇒ 401 si `email_verified:false`).

## Coordonnées
- **PIC** : `socialgouv/produits-dnum/studio-tech/architecture/da-manager/da-manager` (433, primaire, défaut `main`) + `…/architecture/da-manager/da-manager-gitops` (434). *(Réorganisés 2026-06-02 sous le sous-groupe `architecture/da-manager` ; anciens chemins `…/tooling/…` redirigés 301. Build CI = brique partagée [`studio-tech-commun/sdpsn-devops-ci-utils`](../15-build-ci/buildkit-mutualise.md), projet 439.)*
- **Atlas** : Org SDPC `org-01ks7sjgcxkez4ebz81x6t7frc` · Workspace `ws-01ksq7185sfavebnnc8637mg43` · Env preprod `env-01kssbv4csvqg6xcvg63nhm9wy`.
- **Host preprod** : `https://<atlas-host>` *(suffixe `-preprod` ajouté 2026-06-02 ; même envId)*.

## Reste à faire
- ✅ **create/delete env 100 % CI — EN PLACE (2026-07-07)** via PAT Atlas + brique ci-common `atlas-env-lifecycle` ([atlas-pat-machine-identity](../20-cd-pattern/atlas-pat-machine-identity.md)) : `apply` crée l'env à la 1re review (`atlas_env_ensure`), `cleanup-review` supprime aussi l'Environment Atlas (`atlas_env_delete`) — validé e2e pipelines 52688/52690. CI vars 434 : `ATLAS_TOKEN` (PAT), `ATLAS_GITOPS_DEPLOY_USER/_TOKEN`. Le seed Vault (`registry`,`proconnect`) reste opérateur post-create (`atlas-env.sh seed`).
- **Identité durable** : PAT interim (compte opérateur, exp. 2026-07-14) à remplacer — compte bot + PAT, ou SA `client_credentials` (MR draftée, branche `feat/ci-service-account` du clone atlas-monorepo, ressort SRE). La **fédération OIDC GitLab** (RFC [atlas-review-envs-synthese.md](../../.plans/review-env/atlas-review-envs-synthese.md)) reste l'évolution qui supprimerait *aussi* les tokens gitops à rotation manuelle.
- **Parkés** : ProConnect réel (whitelister le callback côté AgentConnect) ; host « joli » (domaine ajouté à l'org/workspace par un super-admin).

> ℹ️ **Comportement attendu (pas un reste-à-faire)** : l'**App ArgoCD orpheline** après un `delete` d'env **se nettoie seule** avec latence — ne pas la supprimer à la main (cf. [historique](../90-historique/da-manager-journal.md)).
