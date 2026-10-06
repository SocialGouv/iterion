---
name: platform-conformance
description: Atlas v2 platform facts for a merge verdict: managed resources (OvhValkey, OvhPostgresqlCluster, OvhBucket), LimitRange bounds and ratio, ingress/Kyverno domain rules, cert-manager, naming norms
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# Plateforme Atlas v2

- **API** : `https://api.atlas-prod.public-cloud.social.gouv.fr/`
- **Code** : `.repos/atlas-monorepo` (« contient uniquement le code pour *builder* les artefacts d'un déploiement, aucune référence à des déploiements réels » — `README.md`).
- **Rôle** : plateforme de déploiement Kubernetes **pilotée par API**. Les utilisateurs ne manipulent **pas** kubectl/Crossplane directement : tout passe par l'**API Atlas**.

## Architecture (vérifiée via `gitops/*/packages.yaml`)

Atlas est découpé en quatre parties (`atlas-monorepo/README.md`) :
- **`tf-modules/`** — modules OpenTofu pour l'infra initiale (réseaux, routeurs) puis la config one-time (init Vault, Keycloak…).
- **`gitops/`** — génère (via une lib TypeScript CDK8S, publiée sur le registre npm GitLab) les manifests du **control-plane** et des **workload-zones**, consommés par le **GitOps controller (Flux)**.
- **`services/api/`** — l'**API Atlas** (déployée sur le control-plane). Voir § API.
- **`compositions/`** — les **compositions Crossplane** (jamais écrites par les users, **générées par l'API**) ; fonctions TypeScript (`crossplane-function-js`).

### Control-plane (composants vérifiés)
**Crossplane 2.2.1**, MongoDB (Percona psmdb-operator), **NATS** (+ nui), **Mimir** (métriques), kube-prometheus-stack, Kyverno, cert-manager (+ webhook OVH), Cilium, CAPI, reloader, velero, goldilocks. + **Keycloak** (IdP OIDC de l'API) et **ArgoCD** (cf. quick links de l'OpenAPI).

### Workload-zone (où tournent les produits — composants vérifiés `gitops/workload-zone/packages.yaml`)
- **CNI** : Cilium 1.19.4 · **Ingress** : **Traefik** 40.2.0
- **Secrets** : **External Secrets** 2.5.0 (ESO) + **Vault** (HashiCorp 0.32.0 ; **OpenBao** présent)
- **GitOps** : flux-operator 0.50.0 / fluxcd v2.8.8
- **DB** : **CloudNativePG** 0.28.2 (operator dispo *in-cluster* — alternative au Postgres OVH managé)
- **Stockage** : openstack-cinder-csi · **Backup** : velero · **Observabilité** : Alloy + kube-prometheus-stack
- **Réseau/DNS/TLS** : external-dns + cert-manager + webhook OVH · **Policy** : Kyverno · **Reload** : reloader (stakater)
- **Cluster** : provisionné via CAPI + provider OpenStack ; autoscaler + metrics-server + goldilocks

> **Pas de sealed secrets** sur Atlas v2 : les secrets vivent dans **Vault** et sont matérialisés dans les namespaces par **ESO**. Voir [secrets-externalsecrets](../20-cd-pattern/secrets-externalsecrets.md).

## Modèle de ressources (XRD Crossplane)

Compositions exposées (`.repos/atlas-monorepo/compositions/functions/*/xrd.yaml`) :
`workspaces`, `environments`, `zones`, `ovhbuckets`, `ovhpostgresqlclusters`, `ovhpostgresqldatabases`, `ovhvalkeys`, `grafanaorgmappers` (groupe `atlas.social.gouv.fr/v1alpha1`).

Hiérarchie : **Organization → Workspace → Environment** ; une **Zone** = un cluster K8s de charge.

- **Workspace** (`workspaces.atlas.social.gouv.fr`) : conteneur d'un projet, avec contrôle d'accès et `domains`. Spec : `name`, `organizationId`, `domains[]`. Outputs : `domains`, `grafana.orgId`.
- **Environment** (`environments.atlas.social.gouv.fr`) : conteneur d'un projet logiciel avec ses ressources par env. **Champs clés** :
  - `spec.name`, `spec.workspaceId`, `spec.zoneRef.name`
  - `spec.gitOps` = **{ `repoUrl`, `ref`, `path`, `credentials.{vaultPath, vaultVersion}` }** — c'est **le contrat de rattachement gitops** (le repo/branche/chemin que la plateforme synchronise ; credentials = chemin Vault d'un secret git : `username`+`password` ou `bearerToken` en HTTPS, `identity`+`known_hosts` en SSH).
  - `status.outputs.namespace` = namespace créé dans le control-plane.
- **Zone** : un cluster (domaine, endpoint, CA, région/réseau OVH), expose un accessor Vault OIDC.
- **OVH PG / buckets / valkey** : ressources d'infra, liables à un environnement (`link-environment`).

## Nommage (norme)

À appliquer à **toute nouvelle création**. Porte sur le champ `name` — celui qu'on passe à la création et qu'affichent les UI.

**Toute ressource nommable suit une formule unique :**

```
<produit>-<type>[-<usage>]-<environnement>
```

- **`<produit>`** — le slug du produit (= le nom du workspace de zone `dev`).
- **`<type>`** — le genre de ressource, **vocabulaire fermé** (une CRD = un token, cf. table).
- **`<usage>`** — présent **uniquement** quand un même environnement peut contenir **plusieurs** ressources de ce `<type>` (buckets : toujours ; databases : dès qu'il y en a plus d'une). Décrit **le rôle / le consommateur** de la ressource, pas ce qu'elle contient. **Cadré** (cf. ci-dessous), pas un mot libre. Omis sinon.
- **`<environnement>`** — **vocabulaire fermé**.
- Chaque segment dans `^[a-z0-9]+$`, nom total `≤ 63` caractères (contrainte `valid_name()` de [`scripts/atlas/atlas-env.sh`](../../scripts/atlas/atlas-env.sh) — la marge est plus courte pour un nom à 4 segments).

**`<type>` — genre de ressource (fermé, une CRD = un token) :**

| Ressource (CRD Atlas) | `<type>` |
|---|---|
| PostgreSQL managé OVH (`OvhPostgresqlCluster`) | `pg` |
| PostgreSQL CNPG via l'API (`InClusterPostgresqlCluster`) | `cnpg` |
| Bucket S3 (`OvhBucket`) | `s3` |
| Cache (`OvhValkey`) | `valkey` |
| MySQL / MariaDB managé | `mysql` |

`pg` et `cnpg` sont **deux types distincts** (backends, cycles de vie et secrets différents), à ne pas confondre.

**`<usage>` — cadré (pas un mot libre), à choisir dans _l'une_ de ces deux familles :**

1. **Usage fonctionnel transverse** — token d'une liste courte, indépendant du produit : `app` (la ressource applicative **unique** du produit), `migration`, `backup`, `metabase`. *(Liste extensible par MR.)*
2. **Service applicatif consommateur** — quand un produit a **plusieurs** ressources du même `<type>` pour des services distincts, `<usage>` = le **slug du service applicatif** (`chantier`, `lsp`, `home`…) : le nom d'un service **réel** du produit, pas un token arbitraire.

Règle de choix, sans ambiguïté : **1 seule** ressource applicative de ce type → `app` ; **plusieurs** (une par service) → **le slug du service** (ex. ptt a deux bases backend → `ptt-chantier-<env>` et `ptt-lsp-<env>`).

**`<environnement>` (fermé) :** `dev`, `test`, `valid` (validation), `form` (formation), `preprod`, `prod` ; un environnement de review = `review-<slug>`.

**Application par catégorie d'objet :**

| Objet | Motif | Exemple (produit `domifa`) |
|---|---|---|
| **Workspace** zone `dev` | `<produit>` | `domifa` |
| **Workspace** prod (dédié) | `<produit>-prod` | `domifa-prod` |
| **Environment** | `<produit>-<environnement>` | `domifa-dev` |
| **Cluster** DB / cache (1 par env) | `<produit>-<type>-<environnement>` | `domifa-cnpg-dev`, `domifa-pg-preprod` |
| **Database** (enfant d'un cluster, ≥ 1) | `<produit>-<usage>-<environnement>` | 1 base applicative : `domifa-app-dev` · **plusieurs services** : `ptt-chantier-dev`, `ptt-lsp-dev` · autre usage : `domifa-metabase-dev` |
| **Bucket S3** | `<produit>-s3-<usage>-<environnement>` | `domifa-s3-app-dev`, `domifa-s3-backup-preprod` |

Une **database** est enfant d'un **cluster** (pas d'un environnement). Comme le cluster porte déjà le `<type>` (`pg`/`cnpg`), la database l'**omet** et se distingue par son `<usage>` — obligatoire, puisqu'un cluster peut héberger plusieurs bases.

> ⚠️ **Les identifiants techniques ne suivent pas cette norme et ne sont pas choisis** : `ws-01…`, `env-01…`, `obckt-01…`, `cpgc-01…` sont générés par la plateforme. Plusieurs mécanismes indexent d'ailleurs par **ID**, jamais par nom — creds S3 en Vault sous `buckets/<obckt-id>`, Secrets DB `<role>-<opgId>` (cf. *Stockage objet* et *CNPG via l'API Atlas* plus bas). Renommer un objet ne déplace donc **pas** ses secrets.

### Points que la norme ne tranche pas

- **Environnements de review par branche** : `<environnement>` vaudrait `review-<slug>`, mais l'outillage produit `<ENV_PREFIX><slug>` avec un `ENV_PREFIX` par produit (`graal-review-`, `da-manager-review-`). Les défauts de `atlas-env.sh` n'ont **pas** été alignés : changer le nom d'un env de review change le dossier gitops et l'Application ArgoCD des produits en place.

### Écarts existants — non renommés

Un renommage casse le chemin gitops, l'Application ArgoCD et les deploy tokens : le passage `domifa-preprod` → `domifa-dev` (2026-08-10) a demandé une reprise complète (`configure-gitops` rejoué, nouveau deploy token, bucket recréé). L'existant reste donc tel quel ; la norme vaut pour la suite.

| Objet | Nom actuel | Selon la norme |
|---|---|---|
| Env da-manager | `da-manager-preprod` | conforme |
| Env domifa | `domifa-dev` | conforme |
| Env basavi prod | `basavi-prod` | conforme |
| Workspace basavi prod | `BASAVI-PROD` | `basavi-prod` (minuscules) |
| Envs de review egapro | `alpha`, `staging` | `egapro-alpha`, `egapro-staging` |
| Cluster CNPG cdtn | `cluster-cdtn` | `cdtn-cnpg-<env>` |
| Cluster CNPG domifa | `domifa-dev` | `domifa-cnpg-dev` |
| Database métier domifa | `domifa-dev` | `domifa-app-dev` |
| Database Metabase domifa | `domifa-metabase-pg-dev` | `domifa-metabase-dev` |
| Bucket domifa dev | `domifa-dev` | `domifa-s3-app-dev` |

> **ptt-dev** : les ressources DB (cluster CNPG, bucket backup, 2 databases) ont été **recréées aux noms normés** le 2026-09-09 (`ptt-cnpg-dev`, `ptt-s3-backup-dev`, `ptt-chantier-dev`, `ptt-lsp-dev`) et les anciennes non normées supprimées → **plus d'écart ptt**.

## API Atlas (source : `services/api/openapi/openapi.yaml`, OpenAPI 3.1)

Architecture **CQRS + Event Sourcing** (`services/api/README.md`) : **POST = commandes** (mutations, ne renvoient rien), **GET = queries**. Délibérément **non-RESTful** (pas de PUT/DELETE ; les suppressions sont des POST `…/delete`). Auth **OIDC Keycloak** (bearer JWT) ; les credentials transmis sont **sanitizés** (stockés dans Vault, remplacés par un chemin).

### Surface (extrait des paths)
- **Org/Workspace** : `GET/POST /organizations`, `/organizations/{orgId}/workspaces`, `/organizations/{orgId}/members/...`, `POST /workspaces`, `GET /workspaces/{wsId}`, `/workspaces/{wsId}/members/...`, `POST /workspaces/{wsId}/delete`.
- **Environments** : `GET/POST /workspaces/{wsId}/environments`, `GET /environments`, `GET /environments/{envId}`, `POST /environments/{envId}/rename`, **`POST /environments/{envId}/configure-gitops`**, `POST /environments/{envId}/delete`.
- **Ressources** : `…/ovhpostgresqlclusters` (+ `/{id}/databases`), `…/ovhbuckets` (+ `enable-versioning`, `link-environment`, `unlink-environment`), `…/ovhvalkeys`, `…/ovhpostgresqldatabases` (+ `link-environment`).
- **Divers** : `GET /zones`, `GET /kubeconfig`, `/admin/...`.
- **Quick links** (par instance/zone) : ArgoCD `https://argocd.<domain>`, Grafana `https://grafana.<domain>`, Vault `https://vault.<zone>.<domain>`.

### Auth & endpoints (prod — vérifié via l'OpenAPI servi)
- **Base API** : `https://api.atlas-prod.public-cloud.social.gouv.fr/api/v1` (préfixe `/api/v1`).
- **Keycloak** : `https://keycloak.atlas-prod.public-cloud.social.gouv.fr`, **realm `atlas-prod`**.
- **Flow** : OAuth2 **authorization code + PKCE** (login interactif), client public `control-plane-atlas-portal`. Deuxième medium livré : **PAT Atlas** (`atlas-pat-…`, `Authorization: Bearer`) = **identité machine headless** pour la CI — cf. [atlas-pat-machine-identity](../20-cd-pattern/atlas-pat-machine-identity.md).
- **Deux media d'auth** ([`authenticate.ts`](../../.repos/atlas-monorepo/services/api/src/http/middlewares/authenticate.ts), `detectAuthMedium`) : un bearer préfixé **`atlas-pat`** → branche PAT (résout le hash → principal `user`, hérite des rôles FGA) ; sinon **JWT** → l'API **ne valide que la signature** (JWKS realm `atlas-prod`, RS256, **sans contrôler l'audience ni le client** — `FIXME: allow audience for downstream services`) ⇒ **tout token signé par ce realm est accepté**, y compris l'`id_token` kubelogin `dev-kubernetes` (claims `groups`/`email`). C'est ce qui permet de **dériver `ATLAS_TOKEN` automatiquement** (cf. ci-dessous).
- **Quick links** : ArgoCD/Grafana `https://{argocd,grafana}.atlas-prod.public-cloud.social.gouv.fr`, Vault par zone `https://vault.<zone>.atlas-prod.public-cloud.social.gouv.fr`.

```bash
# Plus de copier-coller : atlas-token.sh dérive un id_token frais (client dev-kubernetes),
# refresh silencieux depuis le cache kubelogin (= celui de kubectl).
ATLAS_TOKEN="$(scripts/atlas/atlas-token.sh)" \
  curl -sS -H "Authorization: Bearer $ATLAS_TOKEN" \
  "https://api.atlas-prod.public-cloud.social.gouv.fr/api/v1/zones"
```

> 🪤 **L'`id_token` ne vit que 300 s (5 min)** — mesuré 2026-07-30 (`exp - iat`, client `dev-kubernetes`,
> `typ: ID`). Conséquence : un token **collé** quelque part (UI, variable d'un shell resté ouvert, onglet
> Scalar) est **périmé quasi immédiatement** → l'API répond **`400 — jwt expired`** alors que le token
> « vient d'être généré ». **Ne jamais stocker le token** : le dériver **à chaque appel** par substitution
> (`ATLAS_TOKEN="$(scripts/atlas/atlas-token.sh)" curl …`), ce que font `atlas-env.sh`/`atlas-members.sh`.
> Le refresh est silencieux tant que le **refresh token** du cache kubelogin est vivant ; quand il meurt,
> `atlas-token.sh` rouvre un **login navigateur** (échoue en `context deadline exceeded` si personne ne
> le complète — c'est un login manquant, pas une panne d'API).

> ⚠ Pas de flow `client_credentials` exposé en prod (≠ realm `atlas-sandbox` de basavi). Pour le poste de travail, le **client public `dev-kubernetes` + refresh token** (via `atlas-token.sh`) suffit et évite tout copier-coller. Pour une **CI 100 % headless**, la plateforme a livré les **PAT Atlas** (`atlas-pat-…`) : identité machine bearer, `atlas-env.sh`/`atlas-members.sh` l'acceptent tel quel — cf. [atlas-pat-machine-identity](../20-cd-pattern/atlas-pat-machine-identity.md). ⚠ le PAT couvre l'**API** (create/configure-gitops/delete) mais **pas le seed Vault** des secrets applicatifs (pas d'endpoint API secrets ; accès **Vault** zone toujours requis pour ce seed + le credential de lecture du repo gitops).

### Découvertes prod (session)
- **Zones** (`GET /api/v1/zones`) : `dev` → `dev.atlas-prod.public-cloud.social.gouv.fr` ; `prod` → `prod.atlas-prod.public-cloud.social.gouv.fr`. Un host produit = `<app>-<slug>.dev.atlas-prod…` (review en zone dev).
- **Création d'Organization = admin-only** : `POST /admin/organizations` → `403 User must be admin` pour un user non-admin. Un **admin Atlas** doit créer l'Org + Workspace et rattacher l'utilisateur (sinon `/organizations` et `/workspaces` renvoient `[]`).
- **Schémas de création** : org `{name, domains[], initialAdmin{userEmail|userId}}` ; workspace `{name, domains[] (⊆ org), initialAdmin}` ; environment `{name, zone(ZoneRef), gitops?}` ; `configure-gitops {gitops}`.
- **CI ⇒ PAT Atlas** : l'auth prod étant PKCE interactif, automatiser les appels Atlas en CI (créer/supprimer env par branche) exigeait une identité machine. Livrée sous forme de **PAT** (`atlas-pat-…`, medium bearer géré par `authenticate.ts`) — cf. [atlas-pat-machine-identity](../20-cd-pattern/atlas-pat-machine-identity.md). Le PAT hérite des rôles FGA de l'utilisateur qui l'a minté ⇒ identité **dédiée** grantée **workspace `admin`** par workspace migré.

## Flux de déploiement d'un produit (cible)

1. (Une fois) **Workspace** sous une **Organization**.
2. Provisionner les **ressources** via l'API (`OvhPostgresqlCluster` + `database`, `OvhBuckets`…), `link-environment` → secrets de connexion dans **Vault**.
3. Écrire les **secrets applicatifs** dans Vault.
4. Créer l'**Environment** (`zoneRef` = zone cible) puis **`configure-gitops`** vers le **repo gitops** (manifests rendus).
5. **ArgoCD** synchronise les manifests ; **ESO** matérialise les secrets ; l'app démarre.

## Dev local (référence)
`devbox` + `direnv` (`README.md`). `task e2e:setup` (clusters kind), `task control-plane:bootstrap`, `task wokrload-zone:bootstrap`, `tilt up`. Outils dans `.repos/atlas-monorepo/devbox.json` (crossplane-cli, fluxcd, kcl, kubectl, helm, opentofu, cdk8s-cli, openfga-cli…).

---

# Onboarding & déploiement d'un produit (mécanique réutilisable)

> **Confirmé par le code** (`services/api`, `compositions/functions/environments.atlas.social.gouv.fr`) et exercé en réel (da-manager). Référence pour migrer le prochain produit. Source de vérité = le code dans `.repos/atlas-monorepo` ; y retourner au besoin.

## Autorisation & rôles (qui peut créer quoi)
- **Admin Atlas = membre du groupe Keycloak `/admin`** (realm `atlas-prod`), exposé via le claim JWT `groups` ([`authenticate.ts`](../../.repos/atlas-monorepo/services/api/src/http/middlewares/authenticate.ts) : `isAdmin = payload.groups?.includes("/admin")`). Le scope `groups` est demandé par défaut ; si le claim est absent → pas membre.
- **Création d'Organization = admin-only** (`POST /admin/organizations`, middleware `requireAdmin`, aucun contournement OpenFGA). Seul acte super-admin de toute une migration.
- **Modèle OpenFGA** (`services/api/atlas.fga`) : `organization.admin` → `can_create_workspaces` ; `workspace.admin` (`= [user] or admin from organization`) → `can_create_environments / _buckets / _ovh_postgresql_databases / _ovh_valkeys`. L'**`initialAdmin`** fourni à la création d'une org/workspace **devient `admin`** (tuple émis par le domaine).
- ⇒ **Pattern d'onboarding** : un admin Atlas crée **une fois** l'Org avec `initialAdmin: {userEmail: <toi>}`. Ensuite **tu** crées Workspace → Environment → ressources → `configure-gitops` **avec ton propre token PKCE** (pas besoin d'être `/admin`).
- **UserId interne** = `user-<ulid>` (≠ `sub` Keycloak). Pas d'endpoint self/`me` ; lisible seulement via `/admin/users` (admin, **et sans filtre : `?search=` → `400 Unknown query parameter`**). ⛔ **Ne pas tâtonner pour trouver un userId** : pour `initialAdmin`/membres, **fournir `userEmail`** (résolu côté serveur) — inutile de grep les members d'autres ws ou d'appeler `/admin/users`. Besoin d'un userId a posteriori (pour `set-roles`/`remove`, id-keyed) ⇒ il est dans `GET /workspaces/{wsId}`.`members[].{id,email}` après l'ajout. Un user doit avoir été **pré-créé** (reconcilié par `sub` puis `email` ; sinon `404 User not found` même pour un GET — donc un `GET /organizations` qui répond `200 []` prouve que le compte existe déjà).

## Membres, rôles & cloisonnement par produit (multi-tenant)
- **1 workspace = 1 produit/tenant.** Un org-admin crée N workspaces sous l'org et gère les membres de chacun **indépendamment** → cloisonnement par produit natif (ex. des users sur `da-manager` uniquement, d'autres sur `basavi` uniquement).
- **Endpoints membres** (`WorkspaceController`/`OrganizationController`) : `POST /workspaces/{wsId}/members` (ajout), `…/members/{userId}/remove`, `…/members/{userId}/set-roles` (idem `…/organizations/{orgId}/members/...`). Autorisés par les relations **`can_manage_members`** / **`can_manage_roles`** (= `admin`). ⇒ un **workspace-admin** suffit (pas besoin du groupe `/admin`).
- **Ajout par email** : `addMember` accepte `userEmail` (résolu serveur) **ou** `userId`. ⚠ le user doit **pré-exister** (réconcilié par `sub` puis `email`, typiquement s'être connecté ≥ 1×), sinon `404 User not found`.
- **Rôles assignables (grossiers)** — `WorkspaceRole` (`services/api/src/features/workspaces/domain/types.ts`) = **`viewer | editor | admin | secrets_viewer | executor | grafana_is_editor`** (6 valeurs ; `executor` ajouté le 2026-06-05 par `35731dbe`) :
  - `viewer` / `editor` / `admin` : lecture / écriture / contrôle total, **sur tous les services** (ArgoCD + k8s + Vault) et **tous les envs** du workspace (les rôles d'env héritent `… from workspace` dans `atlas.fga`).
  - `secrets_viewer` : lecture des secrets ; `executor` : droit d'**`exec`/`port-forward` kubernetes** (relation FGA `kubernetes_is_executor` → groupe `<envId>:exec` émis par `JwtEnricherService`) ; `grafana_is_editor` : éditeur Grafana (seule granularité par-service exposée par l'API). `admin` ajoute la gestion membres/rôles + création d'envs/ressources.
  - ⚠ `OrganizationRole` (`services/api/src/features/organization/domain/types.ts`) = **`admin` uniquement** — pas de `viewer`/`editor` au niveau organisation.
- **Dérivation par-service / par-env** : `JwtEnricherService` mappe (rôle FGA × claim `service` du token : `argo-cd`/`vault`/`kubernetes`/`grafana`) → groupes `<envId>:<role>` (ou `<wsId>:<role>` pour Grafana). ⇒ un membre de `ws-da-manager` ne reçoit que les groupes des envs da-manager → **ne voit que les Applications da-manager dans ArgoCD** (idem Vault/k8s/Grafana). **Cloisonnement inter-produits garanti par construction.**
- **Limites (assumées, non bloquantes pour nous)** :
  - Pas de scoping **par-service seul** via l'API (rôles grossiers ; le modèle FGA *sait* l'exprimer — `argocd_is_*` — mais l'API ne l'expose pas, sauf `grafana_is_editor`). *On scope par tenant, pas par service → non nécessaire.*
  - Pas d'attribution de rôle **par-environnement** : `EnvController` n'expose aucun endpoint membre → un rôle s'applique au **workspace entier** (tous ses envs : dev/prod/review). (Le modèle FGA autorise un tuple d'env direct, mais il n'est pas exposé.)

### Droits : organisation → workspace ne cascade pas

> **Vérifié le 2026-09-14, et ça coûte un aller-retour humain.** Être **admin de l'organisation**
> ne donne **aucun droit** sur un workspace dont on n'est pas membre direct — pas même celui de
> s'y ajouter soi-même. Tenté sur le workspace `domifa` par un admin de l'org SDPC :
> `POST /workspaces/{wsId}/members` → **403 `can_manage_members`**, et `GET /workspaces/{wsId}` →
> **403 `can_view`** (le workspace n'apparaît même pas dans `GET /workspaces`).

Le modèle FGA ([`services/api/src/gen/atlas.fga.ts`](../../.repos/atlas-monorepo/services/api/src/gen/atlas.fga.ts), type `workspace`) l'explique :

| Relation | Définition | Cascade depuis l'org ? |
|---|---|---|
| `admin` | `{ this: {} }` (l.118) | **non** — affectation directe uniquement |
| `can_manage_members` / `can_manage_roles` | `computedUserset(admin)` (l.164-165) | **non** |
| `can_delete` | `tupleToUserset` → `organization.can_delete_workspaces` (l.127) | **oui** |
| `can_change_domains` | `tupleToUserset` → `organization.can_replace_workspace_domains` (l.121) | **oui** |

⚠️ **Asymétrie à connaître (et à remonter)** : un admin d'organisation peut **supprimer** un
workspace et **changer ses domaines**, mais **pas y ajouter un membre**. Il n'existe pas de porte
de service : les routes `admin/` de l'API ne couvrent que les **organisations** et les
**utilisateurs** (`paths/admin/` = `organizations`, `organizations_{orgId}_domains`,
`organizations_{orgId}_rename`, `users`, `users_{userId}_delete`), jamais les appartenances aux
workspaces.

⇒ **Conséquence pratique** : pour entrer sur le workspace d'un produit qu'on n'a pas créé, il faut
qu'un **admin direct de ce workspace** nous ajoute. Identifier les candidats via
`GET /organizations/{orgId}` (qui, lui, liste les membres de l'org et leurs rôles) puis demander.
Et **après l'ajout, `kubectl`/ArgoCD/Vault peuvent exiger un nouveau login** — les groupes OIDC
sont figés au login (cf. mémoire `atlas-role-change-requires-relogin`). *(Observé sur ce cas :
l'API Atlas a pris le nouveau rôle immédiatement — le contrôle est serveur, sur l'`userId` —
tandis que `kubectl` a suivi sans re-login, le RBAC se résolvant aussi côté serveur.)*

### Recette — ajouter / gérer un membre (exercé en réel)
Acte d'**admin du workspace** (pas besoin du groupe `/admin`). Token chargé sans l'afficher ; **`curl` pur, pas de `jq` dans la même commande** (cf. [credentials-and-access § hook secrets](../00-context/credentials-and-access.md)).

> **Outil dédié (recommandé)** : [`scripts/atlas/atlas-members.sh`](../../scripts/atlas/atlas-members.sh) provisionne les membres **par équipe**, depuis deux listes locales (gitignorées, jamais commitées, **transmises hors-bande**, jamais lues par l'agent — sortie *redacted*) :
> - `.secrets/atlas-teams.tsv` — `team  email  roles[,…]  [userId]` : une **équipe** = des membres+rôles, définie une fois et réutilisée (template [`atlas-teams.tsv.example`](../../scripts/setup/atlas-teams.tsv.example)).
> - `.secrets/atlas-bindings.tsv` — `workspace_id|*  team[,team…]` : quelles équipes sur quel workspace. **`*` = tous les workspaces de l'org SDPC** (`org-01ks7sjgcxkez4ebz81x6t7frc`, anciens+nouveaux, énumérés via `GET /organizations/{orgId}/workspaces`) → l'équipe devops transverse ; chaque produit reçoit en plus sa propre équipe (template [`atlas-bindings.tsv.example`](../../scripts/setup/atlas-bindings.tsv.example)). Les workspaces **prod** (ex. basavi-prod) vivent dans une **autre org** dont on n'est pas admin ⇒ **jamais énumérés par `*`**, protection par construction. Un id de workspace explicite dans le fichier reste possible hors `*` (non filtré par org — l'API refuse de toute façon si non-admin).
>
> Commandes : `teams` / `ls` (sans API), `sync [--prune]` (idempotent — rejouer n'ajoute que les manquants, `*` couvre tout nouveau ws), `diff`, `set-roles`/`remove` (bas niveau, par user-id).
> ⚠️ **Gap email→user-id** : la liste membres renvoie `{"id":"user-…","roles":[…]}` (**clé `id`, pas d'email**) et l'API ne résout pas email→id hors `/admin`. Donc `sync` se fait **par email** (additif), mais `--prune` est **id-keyed** (4e col `userId` du fichier teams ; id réels via `diff`) — un ws dont un membre désiré n'a pas d'id est **sauté au prune** (garde-fou), sauf `--force`.

Curls bruts équivalents (référence / dépannage) :
```bash
set -a; source .secrets/.env; set +a
API="https://api.atlas-prod.public-cloud.social.gouv.fr/api/v1"
WS="<wsId>"            # ex. workspace du produit

# 1) Lister les membres actuels (champ "members" : [{id:"user-…", roles}], clé `id`, PAS d'email)
curl -sS -H "Authorization: Bearer $ATLAS_TOKEN" "$API/workspaces/$WS"

# 2) Ajouter par email → 204 (corps vide). roles ∈ {viewer,editor,secrets_viewer,admin}
curl -sS -X POST -w '\nstatus=%{http_code}\n' \
  -H "Authorization: Bearer $ATLAS_TOKEN" -H "Content-Type: application/json" \
  -d '{"userEmail":"<email>","roles":["<role>"]}' \
  "$API/workspaces/$WS/members"

# 3) Changer les rôles / retirer (userId lu à l'étape 1)
curl -sS -X POST -d '{"roles":["<role>"]}' -H "Content-Type: application/json" \
  -H "Authorization: Bearer $ATLAS_TOKEN" "$API/workspaces/$WS/members/<userId>/set-roles"
curl -sS -X POST -H "Authorization: Bearer $ATLAS_TOKEN" \
  "$API/workspaces/$WS/members/<userId>/remove"
```
- **Rôles validés par l'API** (`WorkspaceRole` côté OpenAPI/zod du body) = **`viewer | editor | secrets_viewer | admin`** uniquement (le `grafana_is_editor` listé plus haut vient des types domaine mais **n'est pas accepté** par `addMember`).
- ⚠ **`404 User not found`** si l'email n'a jamais été réconcilié → la personne doit s'être **connectée ≥ 1×** à Keycloak `atlas-prod` au préalable. Un `204` (ou son `userId` qui apparaît à l'étape 1) confirme la résolution.
- ⚠ **Ré-ajout d'un membre existant = `400`** (corps `{"title":"User … is already a member of workspace …"}`), **pas** un `409`. `atlas-members.sh` le traite comme idempotent (déjà-membre) ; en manuel, filtrer `already…member` du `400`.
- ⚠ **…mais parfois `500` au lieu du `400`** (observé 2026-08-27 sur le ws `srdt` = `ws-01kvg7s3v0g8snhbvwfxcqmvg7`, 3 membres déjà en place, corps `{"title":"Internal Server Error","status":500}`). **Aucun effet de bord** : relecture `GET /workspaces/{wsId}` ⇒ les 7 membres intacts, rôles inchangés (`secrets_viewer` conservé). ⇒ **un `500` sur `POST …/members` n'est pas un échec d'écriture** : re-lire les membres avant de conclure ou de rejouer. `atlas-members.sh` le compte en « en attente/erreur », pas en ajout — le résumé de `sync` peut donc sur-signaler. **À remonter à la plateforme.**
- Idem org : `POST /organizations/{orgId}/members[ /{userId}/{set-roles,remove} ]`.
- Token PKCE **court** : si `400 {"detail":"jwt expired"}`, rafraîchir `ATLAS_TOKEN` puis rejouer.

## API — gotchas
- **CQRS** : POST = commandes (souvent corps vide en réponse, `202`/`201 {id}`), GET = queries. Pas de PUT/DELETE (suppressions = POST `…/delete`). IDs **générés serveur** : `org-`, `ws-`, `env-`, `opgc-`, `obckt-`, `ovk-`, `user-` + ulid lowercase.
- **Pas de `GET /workspaces/{ws}/environments`** (→ `405`). Lister via `GET /environments` ou `GET /environments/{id}`.
- Domaines : `workspace.domains ⊆ org.domains` (égalité ou suffixe `.`). **Fixer les domaines d'org = `/admin/...` (super-admin)** ; les domaines d'un workspace = `POST /organizations/{orgId}/workspaces/{wsId}/domains` (org-admin). ⚠ **Ce N'EST PAS que de la métadonnée** : c'est un **gate runtime** (cf. Ingress/Kyverno ci-dessous).

## Ce que provisionne la composition `Environment` (auto, par env)
Source : `compositions/functions/environments.atlas.social.gouv.fr/composition.fn.ts`. La création d'un Environment crée **~45 ressources**, dont :
- **Namespace** = `envId` (`env-<ulid>`) sur la zone cible. NetworkPolicy, RBAC, ServiceAccounts, ResourceQuota, et une **`LimitRange` `default`** contraignante (composition `…:195`) :
  - 🚧 **`maxLimitRequestRatio.memory = 2`** → tout conteneur doit avoir `limits.memory / requests.memory ≤ 2`, sinon le pod est **forbidden** (`pods "…" is forbidden: memory max limit to request ratio per Container`) → Deployment Degraded, jamais de pod. (CPU non contraint.)
  - `defaultRequest`: `memory 500Mi`, `cpu 250m` ; `default` limit `memory 500Mi` ; `min memory 50Mi` ; `max memory 8Gi`. ⇒ dans le chart : viser `requests.memory ≈ limits.memory/2` (ex. req `512Mi` / limit `1Gi`).
- **Vault** : mount **KV v2 `<wsId>/<envId>/kv`** + mount **database `<wsId>/<envId>/db`** ; 4 **policies** `<envId>_{admin,editor,secrets-viewer,viewer}` + 4 **groupes OIDC externes** (alias `<envId>:<role>` sur l'accessor OIDC de la zone). La policy `admin` donne `create/update/patch/delete` sur `<wsId>/<envId>/kv/data/*` + metadata.
- **ESO SecretStore `local-secret-store`** (kind `SecretStore`, **dans le ns `envId`**, provider Vault, `path: <wsId>/<envId>/kv`, `version: v2`, auth jwt `path: k8s-<zone>` role `external-secrets-env` SA `default`). → c'est **le store que les ExternalSecrets d'un produit référencent**. (+ un SecretStore control-plane nommé `<envId>` dans `crossplane-system` pour les ressources gérées par la plateforme.)
- Si `spec.gitOps` fourni : **AppProject** + **Application ArgoCD** (namespace `argo-cd`) — `source.directory.recurse: true`, `repoURL/targetRevision=ref/path`, `destination.namespace = envId`, `syncPolicy: automated {prune, selfHeal}` — + un **ExternalSecret** (store `control-plane-vault`) qui matérialise le secret repo ArgoCD depuis les credentials.

## Secrets applicatifs : qui écrit, où, comment
- **Pont OpenFGA → groupes → accès** : `JwtEnricherService` (`services/api`) ajoute, selon le claim `service` du token (`vault`/`argo-cd`/`grafana`/`kubernetes`) et les relations FGA, des groupes `<envId>:<role>`. Vault OIDC / ArgoCD / k8s / Grafana mappent ensuite ces groupes → permissions. **Admin du workspace → admin de l'env → `vault_is_admin` → groupe `<envId>:admin` → écriture KV.**
- **Écrire un secret** (humain) : `vault login -method=oidc` sur `https://vault.<zone>.<domain>` (auth method `oidc/`), puis écrire dans **`<wsId>/<envId>/kv/<name>`** (KV v2 → API `…/kv/data/<name>`). On peut récupérer des creds existantes depuis le cluster **Fabrique** (`.secrets/plateform-fabrique/kubeconfig`) sans les afficher. ⚠ Login OIDC : préférer l'**UI Vault** (`/ui`) — le callback CLI `localhost:8250` **ne passe pas en devcontainer** ; `vault` CLI = devbox `vault-bin`.
- ⚠ **Seeder un env fraîchement créé exige un login Vault POSTÉRIEUR à sa création** : la policy `<envId>_admin` est mappée via les **groupes du token OIDC** (`<envId>:admin`, ajoutés par le JwtEnricher **au login**). Un `VAULT_TOKEN` émis **avant** la création n'a **pas** cette policy, **même quand l'env est `Ready`** (`identity_policies` ne liste que les envs présents à l'émission du token) ⇒ un simple retry ne suffit pas. Après `create` : **re-login Vault** puis `atlas-env.sh seed`.
- **Consommer** (gitops) : `ExternalSecret` (`external-secrets.io/v1`) avec `secretStoreRef: {name: local-secret-store, kind: SecretStore}` et `remoteRef.key: <name>` (**relatif au mount**, ex. `proconnect`), `property: <clé>`. Pour un dockerconfigjson : `target.template.type: kubernetes.io/dockerconfigjson`.

## `configure-gitops` — contrat réel (⚠ corrige la doc d'API)
- `POST /environments/{envId}/configure-gitops` body = **`{gitops: {repoUrl, ref, path, credentials: {username, password}}}`**. Les `credentials` sont en **clair** (username + token) ; **l'API les range dans Vault elle-même** (« stored securely, never returned ») — ce n'est **PAS** un `vaultPath` côté appelant. → fournir un **deploy token GitLab `read_repository`** sur le repo gitops.
- ArgoCD lit `repoUrl@ref:path` en **directory recurse** et applique dans `ns = envId`. ⇒ le repo gitops doit fournir des **manifests plats déjà rendus** à `path`, **sans `namespace:` codé en dur** (ArgoCD place tout dans `envId`).
- 🧹 **Teardown** : `POST /environments/{envId}/delete` déprovisionne l'env (ns, Vault, workload — `GET` renvoie ensuite 403, host → 404). ℹ️ **latence (observé 2026-05-29)** : l'**Application ArgoCD** (créée par `configure-gitops`) reste **visible un moment** après le delete puis **se nettoie seule** (le `resources-finalizer` se résout une fois le prune/ns finalisé) — **ne pas la supprimer à la main** (et un env-admin n'a de toute façon pas `applications, delete` en RBAC ArgoCD).

## Ingress / DNS / TLS (workload-zone) — DEUX gotchas majeurs
- **`ingressClassName: "public"`** (la classe Traefik de la workload-zone s'appelle **`public`**, généré par `gitops/lib/src/constructs/traefik.ts` → `ingressClass.name`; **PAS `traefik`**). Une mauvaise classe ⇒ ingress non pris en charge ⇒ pas d'adresse LB ⇒ **external-dns ne crée pas l'enregistrement** ⇒ host en **NXDOMAIN**.
- Annotation TLS : **`cert-manager.io/cluster-issuer: "letsencrypt"`** (ClusterIssuer `letsencrypt`, ACME **DNS-01** via webhook OVH — `gitops/lib/src/constructs/cert_manager_issuers.ts`). ⚠ **La section `ingress.tls` DOIT avoir un `secretName`** : sans lui, cert-manager (ingress-shim) **n'émet aucun Certificate** et Traefik sert un cert **auto-signé** → `curl: SSL certificate problem: self-signed certificate`, host injoignable en HTTPS public.
- 🚧 **Kyverno `disallow-unknown-domains` (Enforce)** — *le gros piège*. Toute `Ingress` (+ annotations external-dns) dans un ns `atlas.social.gouv.fr/type: environment` doit avoir un host **sous un domaine autorisé**, sinon le sync ArgoCD échoue : `admission webhook "validate.kyverno.svc-fail" denied … Domain is prohibited in this namespace`. Policy : `gitops/workload-zone/src/platform-components/assets/policies/disallow-unknown-domains.yaml` (host accepté si `== ad` ou `endsWith('.'+ad)`).
  - **Domaines autorisés** = annotation namespace `atlas.social.gouv.fr/domains` = **`workspace.domains` + `<envId>.<zone-domain>`** (`compositions/functions/environments…/composition.fn.ts:105-106`).
  - ⇒ **workspace `domains:[]` ⟹ seul host utilisable = `<...>.<envId>.<zone-domain>`** (ex. `app.env-01k….dev.atlas-prod.public-cloud.social.gouv.fr`). Un host « joli » (`<app>.<zone-domain>`) impose d'ajouter `<zone-domain>` à l'**org** (super-admin) puis au **workspace**.
- **DNS/TLS** : une fois l'ingress admis + doté d'une adresse, external-dns (`domainFilters:[<zone-domain>]`) crée l'enregistrement dans la zone publique et cert-manager émet le cert ⇒ le host résout depuis Internet.

## Base de données : CNPG (dev/review) vs OVH managé (preprod + prod)
> **Convention (par environnement, pas par produit) — MàJ 2026-09-14** : **CNPG in-cluster** pour **dev / review** ; **PG managée OVH** (`OvhPostgresqlCluster`) pour **preprod ET prod**, avec des **plans différents** (**preprod sans HA**, **prod en HA** + backups OVH). Motivation : la preprod doit préfigurer la prod (topologie DB managée), tout en restant moins coûteuse (pas de HA). Le contrat applicatif est identique (connexion via un Secret nommé) ⇒ bascule possible env par env.
> *Écart historique assumé* : da-manager (preprod+review) et graal (preprod+review) ont leur preprod en **CNPG** (auto-déclaré) et srdt (2026-06-25) en CNPG **via l'API** — antérieurs à cette convention, non re-basculés. Premier respect de la nouvelle règle : **domifa-preprod** en PG managé OVH (2026-09-01).
>
> **Exception** : Cluster OVH managé en test PTT (pour vérifier le fonctionnement de la rotation des creds qu'on ne peut pas mettre en place avec CNPG)
>
> ⚠️ **CORRECTIF (2026-08-06)** : l'affirmation ci-dessous « aucun endpoint CNPG côté API Atlas » était vraie au **2026-06-03/08** mais est **devenue fausse** — l'API a gagné depuis une ressource `inclusterpostgresqlclusters`/`inclusterpostgresqldatabases` (présente dans `openapi.yaml` `v2.4.4`, utilisée par **srdt** dès le 2026-06-25). **Deux façons de faire du CNPG coexistent désormais** — laquelle a été utilisée pour tel produit doit se lire dans son propre gitops (présence ou non d'un `cnpg-cluster.yaml` maison), pas se déduire de cette doc.

### CNPG auto-déclaré (CRD maison dans le gitops)
- Operator **CloudNativePG** installé (`cnpg-system`, `gitops/lib/src/constructs/cnpg.ts`), CRD **`postgresql.cnpg.io/v1`** (`Cluster`, `Database`, `Pooler`…). Un produit crée un `Cluster` dans son ns (`spec.instances`, `storage.size`, `bootstrap.initdb.{database,owner}`) — CRD écrit à la main dans le repo gitops (rendu Helm → appliqué par ArgoCD), comme da-manager/domifa.
- CNPG génère le secret **`<cluster>-app`** (clés `username/password/dbname/host/port/uri/jdbc-uri`). Mapper l'app : `DATABASE_URL` ← clé **`uri`** (service `<cluster>-rw`). Gratuit, rapide, pas de dépendance OVH.
- **Un seul secret, rôle owner unique** (pas de séparation de privilèges, pas de rotation) — le plus simple à câbler, le moins bien isolé.
- **Extensions non-core (ex. PostGIS)** : deux leviers combinables du CRD, à la main du produit — `spec.imageName` (choisir une image qui embarque déjà l'extension, ex. `ghcr.io/cloudnative-pg/postgis:16-3.5`, cf. domifa) **et/ou** `spec.bootstrap.initdb.postInitSQL` (SQL exécuté par l'opérateur **en superuser, une seule fois au bootstrap**, avant que le cluster ne soit livré — ex. `CREATE EXTENSION IF NOT EXISTS postgis;`). Permet d'activer une extension sans jamais exposer de rôle superuser en fonctionnement (`enableSuperuserAccess: false` reste vrai ensuite). **Vérifié live sur le cluster domifa** (`domifa-db`, 2026-08-07).

### CNPG via l'API Atlas (`inclusterpostgresqlclusters`)
> Ressource **`In-cluster PostgreSQL Database management`** de l'API (`POST /workspaces/{wsId}/inclusterpostgresqlclusters`, IDs `cpgc-…`/`cpg-…`), **au contrat quasi identique à l'OVH managé** ci-dessous (même `backups.bucketId` requis, mêmes routes `.../databases` + `link-environment`/`unlink-environment`/`delete`) — mais orchestrant un cluster **CloudNativePG in-cluster**, pas un service OVH externe.
- **Création** : `POST /workspaces/{wsId}/inclusterpostgresqlclusters` `{name, version:"15"|"16"|"17"|"18", diskSize (déf. 40), backups:{bucketId}, zone}` → `201 {id: cpgc-…}` (nécessite un `OvhBucket` dédié pour les backups, comme l'OVH managé). Puis `POST /inclusterpostgresqlclusters/{cpgcId}/databases` `{name, linkedEnvironments:[envId]}` → `201 {id: cpg-…}`.
- **Secrets livrés directement dans le namespace de l'env** (Secrets Kubernetes natifs, **pas de Vault/ExternalSecret impliqué** pour la DB elle-même) : **`schema-editor-<cpgId>`**, **`editor-<cpgId>`**, **`reader-<cpgId>`**, **8 clés chacun** (vérifié `kubectl get secret -o json | jq '.data|keys'`, sans lire les valeurs) : **`PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`, `PGSSLMODE`, `PGURI`, `PGURI_JDBC`** — **même contrat de clés que les creds Vault dynamiques de l'OVH managé** (§ ci-dessous), livré en **Secret k8s natif** (pas d'`ExternalSecret` côté produit) plutôt qu'en `VaultDynamicSecret`. Donc **séparation de privilèges** dès la sortie de boîte (migrations via `schema-editor`, runtime via `editor`) — contrairement au CNPG auto-déclaré ci-dessus (un seul secret owner). ⚠️ **CORRECTIF (2026-08-14, vérifié live)** : le **contenu** de ces Secrets natifs n'est **PAS statique** — `PGUSER`/`PGPASSWORD` sont des **credentials Vault dynamiques** (user éphémère `v-k8s-dev--<rôle>-…-<expUnix>`, **rotation ~12 h**, membre d'un **rôle parent stable** `editor/schema-editor/reader-<hash>`) ; la plateforme **rafraîchit le Secret natif** ⇒ il y a **bien rotation/TTL**, comme l'OVH managé (cf. § *Mécanisme vérifié* plus bas).
- **Câblage app** : soit référencer le Secret **par nom directement** (`secretKeyRef: {name: editor-<cpgId>, key: PGHOST}` etc., une entrée par variable d'env attendue par l'app) — pas de reshape ESO nécessaire puisque c'est déjà un Secret k8s natif ; soit consommer `PGURI` tel quel si l'app sait parser une URL de connexion.
- 🪤 **Le reshape via ESO est IMPOSSIBLE (RBAC) — utiliser `secretKeyRef` clé-par-clé.** Si l'app attend d'autres noms que `PG*` (ex. `DB_USERNAME`, `FLYWAY_DB_USERNAME`), on est tenté de renommer avec un `ExternalSecret` provider `kubernetes` qui **lit** le Secret natif → il faut le droit `get` sur `secrets`, **que le RBAC env-admin d'Atlas v2 n'accorde pas**, et K8s **interdit de créer un `Role` qui l'accorde** (anti-escalade : « attempting to grant RBAC permissions not currently held »). ⇒ **remap uniquement par `env[].valueFrom.secretKeyRef`** dans la spec du pod (c'est le **kubelet** qui lit le Secret, avec ses droits — jamais bloqué). Corollaire : **`envFrom` en bloc du Secret natif est inadapté** dès qu'on veut 2 rôles (editor **et** schema-editor) dans le même conteneur — leurs 8 clés `PG*` **entrent en collision** (une seule `PGUSER` survivrait) ; il **faut** le mappage par-clé pour distinguer `DB_*` (editor) de `FLYWAY_DB_*` (schema-editor).
- Références vécues :
  - **srdt** (`cpgc-01kvqqppb9qhmfcjx987mb3gjb`/`cpg-01kvqtdy4yjj3x2hzc3b16cae2`, `srdt-gitops` `values.deploy.yaml`) — le `web` (migrations DDL au boot) consomme `schema-editor-<cpgId>`. *(Vérifié live 2026-08-06 via `openapi.yaml` v2.4.4 + lecture directe de `srdt-gitops`.)*
  - **ptt-chantier/lsp** (2026-08-12, `cpgc-01kzp25bmjqh6d2a5fty6eekep`) — **migration OVH managé → CNPG-via-API PG 18** : chart maison + remap `secretKeyRef` par-clé (`editor→DB_*`, `schema-editor→FLYWAY_DB_*`), `application.yml` inchangé. Flyway 65 migrations + HikariPool OK, séparation des rôles conservée, **connexion JDBC sans `sslmode` OK**. *(cf. [runbook chantier §5](../30-migrations/chantier.md#5-db--cnpg-via-api-pg-18-remap-par-extraenvsecretkeyref).)*
- 🔬 **Mécanisme vérifié live (2026-08-14)** — sondes `psql` via Jobs in-cluster (`pods/exec` interdit) sur **ptt-chantier/lsp + domifa**, connexion avec les Secrets natifs `editor-`/`schema-editor-<cpgId>` :
  - **Creds = Vault dynamiques, PAS statiques** : `PGUSER` = user éphémère `v-k8s-dev--<rôle>-c-<rand>-<expUnix>`, **rotation ~12 h** (jusqu'à 4 users vivants par rôle, expirations à 08:21/20:21). La plateforme **réécrit le Secret natif** → invalide l'ancienne mention « statique / pas de rotation ».
  - **Rôles parents STABLES** `editor-<hash>`/`schema-editor-<hash>`/`reader-<hash>` : les users éphémères en sont **membres** ; les **objets sont owned par le parent `schema-editor`** (persiste à travers la rotation).
  - **`editor` a bien tout le DML sur les objets de `schema-editor`** (⇒ résout la « question ouverte » plus bas) : `has_table_privilege` = SELECT/INSERT/UPDATE/DELETE **100 % des tables** (ptt 27/27, domifa 32/32) + USAGE **100 % des séquences** + lecture réelle OK. `editor` n'a **pas** `CREATE` sur `public` (réservé à `schema-editor`) et **n'est pas membre** de `schema-editor` → moindre privilège propre.
  - **PAS via `ALTER DEFAULT PRIVILEGES`** (`pg_default_acl` **vide**) : l'accès `editor` vient de **grants au niveau objet** portés par le rôle parent (`pays.relacl` = `editor-…=arwdDxtm`, `reader-…=r`), donc **posés par la plateforme** (probable `GRANT ON ALL TABLES` rejoué à la réconciliation/rotation). ⚠️ Corollaire : une table créée par une **migration future** n'obtient l'accès `editor` qu'au **prochain re-grant plateforme** (pas instantané) — non bloquant (migrations en `schema-editor`), mais à connaître.
- 🪤 **Provisioning long + `Ready=False` durable** possible (même mécanique que l'OVH managé ci-dessous — non revérifié spécifiquement sur cette ressource mais le contrat backup est identique).
- 🚫 **BLOQUANT — pas de PostGIS (ni de contrôle sur l'image/le bootstrap)** : le body de création (`InClusterPostgreSQLClusterCreateBody`, toujours vrai en **v2.5.1**) **n'expose aucun champ image/extension/postInitSQL** — contrairement au CNPG auto-déclaré ci-dessus, qui a les deux (`imageName` + `postInitSQL`).
  - **Testé 2026-08-06** (`openapi.yaml` v2.4.4, versions `16` et `18`, domifa, Job `psql` avec creds `schema-editor-<cpgId>`) : `pg_available_extensions` → **0 ligne** `postgis%` ; `CREATE EXTENSION postgis` → `extension "postgis" is not available` (`.control` absent de l'image).
  - **Retesté 2026-08-07 après upgrade plateforme** (`openapi.yaml` **v2.5.1**, PG 18, testé sur le workspace domifa) : **l'image par défaut a changé** — `pg_available_extensions` renvoie désormais **10 lignes `postgis*` (v3.6.4)**. Mais `CREATE EXTENSION postgis` échoue toujours : `permission denied … Must be superuser` — **le blocage a glissé de « image sans l'extension » à « aucun rôle superuser exposé pour l'activer »**. Le CNPG sous-jacent sait pourtant le faire sans exposer de superuser en fonctionnement (`postInitSQL` au bootstrap, cf. ci-dessus) — c'est un **manque côté wrapper API Atlas**, pas une limite de CNPG lui-même. Cluster/database/bucket de test nettoyés après vérification.
  - **Un produit qui a besoin de PostGIS (ou de toute extension non-core) doit rester sur le CNPG auto-déclaré** — cette ressource API reste réservée aux besoins PG "vanilla" (comme srdt) jusqu'à ce que l'API expose un équivalent de `postInitSQL`/`extensions` (piste de feature request à remonter aux SRE — la capacité existe déjà côté opérateur).

### PG managée OVH (prod)
- **OVH managé** (`OvhPostgreSQLCluster`) **exige `backups.bucketId`** → créer un `OvhBucket` d'abord ; billable, provisioning async. La `database` se crée avec `{name, linkedEnvironments:[envId]}`. À réserver à la prod si besoin.
- **Contrat de création** (openapi v2.5.x, `OVHPostgreSQLClusterCreateBody`, discriminé par `plan`) : versions `15|16|17|18` ; gamme legacy **`essential`** (1 node)/**`business`** (2)/**`enterprise`** (3) avec flavors `db1-4/7/15/30` (`essential` : disk 80–1920, déf. 80) ; gamme actuelle **`discovery`** (1 node, sans SLA)/**`production`** (2 nodes, backups 14 j + PITR)/**`advanced`** (3 nodes) avec flavors `b3-8/16/32/…/256` (disk 160–25600, déf. 160). Défauts API : `production`/`b3-8`/160. Specs : `db1-15`=4c/15GB, `b3-8`=2c/8GB, `b3-16`=4c/16GB, `b3-32`=8c/32GB. **`max_connections` ≈ 100 par 4 GB de RAM** (cap 1000 — [doc OVH](https://docs.ovhcloud.com/en/guides/public-cloud/databases/postgresql-capabilities)). Prix catalogue public (`api.ovh.com/1.0/order/catalog/public/cloud`, **par node**) : `essential/db1-15` ≈ 217 €/mois, `production/b3-8` ≈ 163 €/node, `production/b3-16` ≈ 326 €/node ; stockage additionnel 0,37 €/Go/mois (`production`).
- 🪤 **Pas de route d'update** dans l'API Atlas (create/list/get/delete seulement) : changer plan/flavor/disk après coup = patch du XR `OVHPostgreSQLCluster` par un **SRE** (OVH sait faire le resize à chaud, mais Crossplane réconcilierait un changement fait console) → dimensionner juste dès la création.
- ❓ **Extensions (PostGIS & co) sur le managé** : `avnadmin` est détenu par la plateforme et le body de création n'expose rien → valider avec les SRE **avant** de créer un cluster pour un produit qui en a besoin (même classe de manque que le CNPG-via-API ci-dessus ; relevé domifa 2026-08-26).
  - 🪤 **Provisioning long + `Ready=False` durable** : le cluster met ~30 min ; il reste `Ready=False` (reason `Creating`, ressources `backup-manager-promote-{ancestor,father,grandfather}` `Unavailable`) **alors que `Responsive=True`** et que la **création de database fonctionne déjà** (PG joignable). Les `.status.errors` (cronjobs backup) oscillent — ne pas les confondre avec un échec. (Vécu 2026-06-05/08, `cluster-ptt`.)

### Creds DB **dynamiques** (Vault Database Secrets Engine) — OVH managé *uniquement*
> La PG managée OVH apporte un **sur-ensemble** du contrat CNPG : des **creds dynamiques** (rôles à mot de passe **éphémère, rotation auto** par Vault) — ce que **CNPG ne sait pas faire** (CNPG = un seul Secret statique `<cluster>-app`). C'est le mécanisme cible pour répliquer en test le comportement des DB managées de prod.

- À la création de la `database`, la composition `ovhpostgresqldatabases` **configure le mount Vault `database` `<wsId>/<envId>/db`** via l'admin OVH **`avnadmin` détenu par la PLATEFORME** ⇒ **l'opérateur n'a jamais besoin d'un rôle admin DB** : le « saut fonctionnel » no-admin (vs Atlas v1) est **résolu côté plateforme**. Elle crée **3 rôles** Vault-DB : **`schema-editor`** (DDL/migrations), **`editor`** (CRUD app), **`reader`** (lecture seule) — TTL défaut **2 j**, max **7 j**.
- Pour **chaque** rôle, dans le **namespace de l'env** : un `VaultDynamicSecret` (générateur) + un `ExternalSecret` nommés **`<role>-<opgId>`** (ex. `editor-opg-01ktc2…`). Le Secret produit (`creationPolicy: Owner`, même nom) porte les **clés `PG*`** :
  - depuis le KV `databases/<opgId>` : `PGHOST`, `PGPORT`, `PGDATABASE`, `PGSSLMODE`, `PGSSLROOTCERT_PEM` ;
  - creds dynamiques (rewrite `username→PGUSER`, `password→PGPASSWORD`) : **`PGUSER`**, **`PGPASSWORD`** ;
  - template : `PGURI` = `postgres://{user}:{pass}@{host}:{port}/{db}?sslmode=…` et `PGURI_JDBC` = `jdbc:postgres://{user}:{pass}@…` ⚠️ **non standard** (le driver attend `jdbc:postgresql://host:port/db` **sans** creds inline) → **inexploitable tel quel**, reconstruire l'URL JDBC côté app.
- **Naming par ID** (comme S3 `buckets/<id>`) : Secrets `<role>-<opgId>`, clé KV `databases/<opgId>`. **Aucun seed Vault manuel.** Vérif (sans lire de valeur) : `kubectl --context <zone> -n <envId> get externalsecrets` → les 3 `…-<opgId>` `READY=True`. *(RBAC env-admin : `list secrets` **interdit**, mais lister les ExternalSecrets et lire leur **spec** `-o yaml/json` est **autorisé** et **suffit** pour connaître les clés produites.)*
- **Câblage app — remap `PG*` → contrat applicatif** (validé **chantier 2026-06-09**) : le sous-chart SDPSN ne fait que de l'`envFrom` (clés brutes) **et le render gitops est 100 % Helm** (pas de kustomize) ⇒ le remap se fait par un **ExternalSecret dédié** (dans `templates/` du wrapper) qui **produit un Secret aux clés `DB_*`/`FLYWAY_*`**, monté via **`deployment.additionalsecretRef`** (→ `envFrom`). Sa `spec.dataFrom` lit **les sources** (pas le Secret `<role>-<opgId>`) : `extract` du KV `databases/<opgId>` (→ PGHOST/PGPORT/PGDATABASE) **+** `generatorRef` du `VaultDynamicSecret` `<role>-<opgId>` (→ `username`/`password`). Ex. : `chantier-db-app` (editor → `DB_HOST/PORT/NAME/USERNAME/PASSWORD`), `chantier-db-flyway` (schema-editor → `FLYWAY_DB_USERNAME/PASSWORD`).
  - 🪤 **`spec.target.template.engineVersion: v2`** (et **PAS** `engine`) — sinon le strict-decoding (ArgoCD/server-side) **rejette** l'objet (`unknown field …template.engine`) → ExternalSecret jamais créé (Secret DB absent, `NotFound`). Valider via `kubectl apply --dry-run=server`.
  - ✅ **MàJ 2026-09-14 (chart maison, ptt-preprod) : consommer DIRECTEMENT le Secret natif `<role>-<opgId>`** (à clés `PG*`) via `extraEnv`/`secretKeyRef` clé-par-clé — le remap `PG*→DB_*/FLYWAY_*` se fait dans les values du wrapper, **sans ExternalSecret custom**. C'est l'approche à privilégier dès que le chart expose `valueFrom` (le sous-chart SDPSN ne le permettait pas, d'où l'ES-remap historique ci-dessus ; les charts maison ptt/da-manager le permettent). 🪤 **NE PAS recréer un ExternalSecret custom sur le générateur** avec `refreshInterval:1m` : combiné à `reloader.stakater.com/auto` (ci-dessous), chaque refresh régénère des creds → **spirale de restart** (boot applicatif > 60 s). Le Secret natif `<role>-<opgId>` est déjà rafraîchi ~12 h par la plateforme.
- **Modèle de rôles app (validé)** : **`app=editor`**, **Flyway=`schema-editor`** (seul à avoir `CREATE` sur `public` → propriétaire des tables). 🪤 **Migrations « sans GRANT »** : pas d'accès admin ni de `CREATEROLE` sur Atlas v2 → des migrations legacy faisant `GRANT … TO <rôle legacy pré-créé>` (`db_owner`…) échouent (`role "…" does not exist`) → **retirer les GRANT** (droits portés par les rôles dynamiques). ❓ **Question ouverte Atlas** : `editor` accède aux tables créées **ensuite** par `schema-editor`, alors que la composition **ne pose AUCUN `ALTER DEFAULT PRIVILEGES`** et que son `GRANT ALL ON ALL TABLES TO editor` ne joue qu'**à la création du rôle** (cred editor observé **antérieur** aux tables) → mécanisme **non visible dans le code de composition**. **MAJ 2026-08-14 (vérifié sur CNPG-via-API, § *Mécanisme vérifié* ci-dessus)** : ce n'est **PAS** un default-privilege (`pg_default_acl` vide) mais des **grants au niveau objet** portés par le **rôle parent stable** (users Vault éphémères membres du parent) — même logique attendue côté OVH.
- **Rotation** : l'annotation `reloader.stakater.com/auto:"true"` (sous-chart) → **restart du pod** quand le Secret change. ⚠️ avec un pool (Hikari…), les connexions ouvertes sous l'ancien user sont **révoquées** à l'expiration du lease ⇒ courte coupure au rolling — acceptable en test, à régler (TTL/handling) en prod.
- **TLS — ⚠️ MàJ 2026-09-14 (ptt-preprod, OVH managé PG18/plan discovery)** : **SSL est bien imposé** — `PGSSLMODE=verify-full` dans le Secret, host `*.database.cloud.ovh.net`, et la connexion JDBC **sans `sslmode` ÉCHOUE**. *(L'ancienne observation « sans sslmode a fonctionné, PG 16.14 » portait sur un cluster OVH antérieur/autre version — **ne plus s'y fier** sur les nouveaux clusters.)* **Fix appliqué** : override de `spring.datasource.url` via env **`SPRING_DATASOURCE_URL`** = `jdbc:postgresql://$(DB_HOST):$(DB_PORT)/$(DB_NAME)?currentSchema=public&sslmode=require`, l'URL étant **composée par l'expansion k8s `$(DB_*)`** des `env:` précédents (Flyway hérite de cette URL, pas de `flyway.url` séparée). `require` (chiffré, sans vérif cert) en preprod ; **`verify-full` + `sslrootcert` depuis `PGSSLROOTCERT_PEM` pour la prod**.
- Réf : `.repos/atlas-monorepo/compositions/functions/ovhpostgresqldatabases.atlas.social.gouv.fr/composition.fn.ts`. *(Vérifié live 2026-06-08, env **test PTT** : `editor/schema-editor/reader-opg-01ktc2wjadfr5wtaca9v9v3qe1` `READY=True`.)*

## Stockage objet : OvhBucket → creds auto en Vault
- **API** : `POST /workspaces/{wsId}/ovhbuckets` body `{name, zone:{name}, versioned, linkedEnvironments:[envId]}` → `201 {id}` (lien possible dès la création) ; sinon `POST /ovhbuckets/{id}/link-environment {envId}`. Suppression : `POST /ovhbuckets/{id}/delete`. Composition `compositions/functions/ovhbuckets.atlas.social.gouv.fr` : user OVH `objectstore_operator` + bucket (SSE-AES256) + `S3Policy` RW.
- **Creds livrés AUTOMATIQUEMENT dans le Vault de l'env** (au `link-environment`) sous la clé **`buckets/<obckt-id>`** — ⚠️ **l'ID du bucket (`obckt-…`), PAS le nom affiché (`spec.name`)** : la composition prend `metadata.name` du XR (= l'id) à la fois comme clé KV **et** comme nom réel du bucket OVH (donc `AWS_BUCKET_NAME` = l'id). Mount `<wsId>/<envId>/kv` ; propriétés `AWS_BUCKET_NAME`, `AWS_ENDPOINT_URL_S3`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (⚠ la **région** n'est PAS poussée par-env → la figer côté app). ⇒ l'`ExternalSecret` doit lire **`remoteRef.key: buckets/<obckt-id>`** (récupérer l'id au `POST …/ovhbuckets` → `201 {id}`) — **pas de seed Vault manuel**. Vérif : `vault kv list <ws>/<env>/kv/buckets`.
  - 🪤 **Piège vécu (2026-06-03, graal)** : un `ExternalSecret` lisant `buckets/<nom-affiché>` → **404** → ESO `SecretSyncError` → pod bloqué, alors que les creds étaient bien présents sous `buckets/<obckt-id>`. **Ce n'était PAS un bug de la composition** (confirmé par l'équipe Atlas sur sandbox) — juste la mauvaise clé côté produit.
- Outillage : [`scripts/atlas/atlas-env.sh`](../../scripts/atlas/atlas-env.sh) avec **`BUCKET_PER_ENV=1`** = 1 bucket/env (créé+lié+supprimé avec l'env — utilisé par [graal](../30-migrations/graal.md)).

## Accès cluster (kubectl) pour debug
- `GET /api/v1/kubeconfig` → kubeconfig **OIDC** (pas de token embarqué). **Structure** (à reconstruire sans relire le fichier) :
  - `clusters`: `dev` (`server: https://<ip>:6443`, `certificate-authority-data`) et `prod`.
  - `users`: `oidc@<zone>` avec `exec` (`apiVersion: client.authentication.k8s.io/v1beta1`, `command: kubectl`, `args: [oidc-login, get-token, --oidc-issuer-url=https://keycloak.atlas-prod…/realms/atlas-prod, --oidc-client-id=<zone>-kubernetes, --oidc-extra-scope=profile|email|groups]`).
  - `contexts`: `dev`→(cluster dev,user oidc@dev), `prod`→idem.
- Nécessite **kubelogin** (`devbox add kubelogin-oidc`). Login interactif **`authcode` par défaut** : le client `<zone>-kubernetes` enregistre `http://localhost:8000` / `:18000` (`tf-modules/services/kubernetes-oidc.tf`). ⚠ **Ne PAS** ajouter `--grant-type=authcode-keyboard` : ce mode utilise un redirect OOB non autorisé → `invalid redirect_uri`. Le token + un **refresh_token** sont cachés (`~/.kube/cache/oidc-login`) → appels suivants non interactifs. **RBAC** dérivé des groupes `<envId>:<role>` (admin workspace → admin du ns env). Helpers : **[`scripts/atlas/build-kubeconfig.sh`](../../scripts/atlas/build-kubeconfig.sh)** (kubeconfig TLS-vérifié via token rafraîchi).
- **kubectl zone dev — `email_verified` obligatoire** : l'apiserver utilise `username-claim = email` → il **rejette tout token dont l'email n'est pas vérifié** (401 « propre » malgré aud/iss/kid/signature corrects). L'email du user doit être **vérifié dans Keycloak** (realm atlas-prod) ; avec un token frais, `kubectl --kubeconfig <kubeconfig API> --context dev get ns` fonctionne. `build-kubeconfig.sh` pour le non-interactif. *(Diag d'un 401 apiserver : si `iss`==well-known, `kid`∈JWKS, `/version`→200, alors c'est le token/claims, pas la connexion.)*
- Quick links : ArgoCD `https://argocd.<domain>`, Grafana `https://grafana.<domain>`, Vault `https://vault.<zone>.<domain>`.
- Outils & méthodes d'autonomie réutilisables : **[`scripts/atlas/`](../../scripts/atlas/README.md)** (vault-put, build-kubeconfig, + récap API GitLab/Atlas/Vault/Fabrique, publish chart OCI manuel, récup secrets Fabrique).

## Ordre d'onboarding récapitulatif (produit → Atlas v2)
1. (admin, 1×) Org + `initialAdmin=toi`.
2. `POST /organizations/{org}/workspaces` → Workspace.
3. `POST /workspaces/{ws}/environments {name, zone}` → Environment (provisionne ns + Vault + `local-secret-store` + policies/groupes). Attendre `Ready`.
4. Écrire les secrets app dans Vault `<wsId>/<envId>/kv/*` (login OIDC).
5. Pousser le gitops (chart wrapper : chart OCI app + CNPG + ExternalSecrets via `local-secret-store`, `ingressClassName: public`) → render plat sur la branche `rendered`.
6. `POST /environments/{envId}/configure-gitops {gitops:{repoUrl, ref:rendered, path, credentials}}` (deploy token read_repository).
7. ArgoCD sync (auto) → CNPG + ESO + pod ; vérifier via host `https://<app>.<zone-domain>/…` ou kubectl OIDC.
