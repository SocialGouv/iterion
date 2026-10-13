---
name: secrets-and-datastores
description: Secrets and datastore rules: no Secret material in charts, ExternalSecret via the local store, reloader conventions, and the managed-datastore escalation rule
---

> Curated for the gitops review bot from devops-agent-as-markdowns@a0d22bc (operator-local documentation of the platform). Trimmed to what a merge verdict needs; concrete host names are templated.

# Secrets : Vault + External Secrets (ESO) — remplace les Sealed Secrets

> Sur **Atlas v2, plus de Sealed Secrets**. Les secrets vivent dans **Vault** et sont matérialisés dans les namespaces par **External Secrets Operator** (ESO, version `2.5.0` en workload-zone — vérifié `.repos/atlas-monorepo/gitops/workload-zone/packages.yaml`).

## Principe

```
Vault (par zone: vault.<zone>.<domain>)
   │  (auth Kubernetes / OIDC)
   ▼
SecretStore / ClusterSecretStore (provider: vault)   ← fourni par la plateforme/zone
   ▲
   │ référencé par
ExternalSecret (dans le repo gitops, par env)
   │  remoteRef: chemin Vault → clés
   ▼
Secret Kubernetes (créé/maj par ESO)  → monté par le Deployment (envFrom/volume)
```

- Le **chart applicatif** ne contient **aucun secret** : il référence des `Secret` **par nom** (ex. `envFrom: [{secretRef: {name: proconnect}}]`).
- Le **repo gitops** porte les `ExternalSecret` (un par secret logique) qui pointent vers des **chemins Vault**.
- ESO synchronise Vault → `Secret` K8s ; `reloader` (présent en zone) peut redéployer en cas de changement.

### Propagation & auto-reboot sur changement de secret (convention)
Chaîne complète : **Vault → ESO (au `refreshInterval`) → `Secret` K8s → Stakater Reloader → rollout du workload**.
- **`refreshInterval: 1m` par défaut** sur tous les `ExternalSecret` (et non `1h`). C'est le délai max entre un changement de valeur dans Vault et la réécriture du `Secret` K8s ; `1h` rend « changer un secret » inutilisable en pratique. Charge Vault négligeable (quelques ES par env).
- **`reloader.stakater.com/auto: "true"` sur le Deployment** (metadata, pas le pod template) : le contrôleur **Reloader** (déployé en workload-zone, ns `reloader`) redémarre automatiquement **les workloads qui référencent** le `Secret`/`ConfigMap` modifié (envFrom, volume, valueFrom). Les charts maison l'exposent via `reloader.auto: true` (da-manager, graal, egapro) ou `extraAnnotations` (vao) ; le mettre **systématiquement** dans les values gitops.
- ⇒ avec les deux, un secret changé dans Vault propage et **reboote le pod en ≤ ~1 min** sans action manuelle. Forcer plus tôt : `kubectl annotate externalsecret <name> force-sync=$(date +%s) --overwrite` (⚠ **retirer l'annotation après** : `… force-sync-`, sinon OutOfSync permanent, cf. plus bas).
- Vérifier que Reloader a déclenché : event `Reloaded` sur le Deployment (`Changes detected in '<secret>' of type 'SECRET' … Updated '<deploy>'`).

## Deux familles de secrets

1. **Secrets de ressources Atlas** (DB, S3, cache) : créés par l'**API Atlas** lors du `link-environment` ; la connexion est écrite dans **Vault** par la plateforme.
   - ⚠ **Nommage par ID** : en v2 le secret Vault porte l'**ID** de la ressource (pas son nom). Cf. [history](../00-context/history.md) → soit la CD récupère l'ID par env, soit on l'inscrit à la main dans le manifeste `ExternalSecret`.
2. **Secrets applicatifs** (ex. da-manager : `PROCONNECT_CLIENT_ID/SECRET`, `AUTH_SECRET`) : **écrits à la main dans Vault** (accès Vault à demander), puis exposés via `ExternalSecret`.

## Le store ExternalSecrets est auto-créé par l'Environment

> ⚠️ Plus besoin de deviner : la **composition `Environment`** crée automatiquement, **dans le namespace de l'env (`envId`)**, un `SecretStore` nommé **`local-secret-store`** (kind `SecretStore`, **pas** ClusterSecretStore), provider **Vault**, `path: <wsId>/<envId>/kv` (KV v2), auth jwt. Détails complets + accès Vault dans [atlas-v2 § Onboarding/Secrets](../10-platforms/atlas-v2.md). API CRD = **`external-secrets.io/v1`**.

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: proconnect                    # PAS de namespace : ArgoCD applique dans envId
spec:
  refreshInterval: 1m                 # défaut maison (cf. § Propagation & auto-reboot)
  secretStoreRef:
    name: local-secret-store          # auto-créé par la composition Environment (in-namespace)
    kind: SecretStore
  target:
    name: proconnect                  # nom du Secret K8s consommé par le chart (envFrom)
    creationPolicy: Owner
  data:
    - secretKey: PROCONNECT_CLIENT_ID
      remoteRef: { key: proconnect, property: PROCONNECT_CLIENT_ID }   # key = chemin RELATIF au mount <wsId>/<envId>/kv
    - secretKey: PROCONNECT_CLIENT_SECRET
      remoteRef: { key: proconnect, property: PROCONNECT_CLIENT_SECRET }
```

- **Écrire les secrets** : `vault login -method=oidc` sur `https://vault.<zone>.<domain>` (l'admin du workspace a la policy d'écriture sur `<wsId>/<envId>/kv/data/*`), puis KV v2 `<wsId>/<envId>/kv/<name>`. Récupérables depuis le cluster Fabrique (`.secrets/plateform-fabrique/kubeconfig`) sans jamais afficher les valeurs.
- **DB** : avec **CNPG in-cluster**, la connexion vient du secret généré `<cluster>-app` (clé `uri`) — **pas** d'ExternalSecret/Vault pour la DB. Vault ne sert qu'aux secrets app (ProConnect, AUTH_SECRET) + dockerconfigjson registre. (Pour OVH managé, le secret de connexion serait écrit par la plateforme, nommé par ID — cf. [history](../00-context/history.md).)
- **credentials git du `configure-gitops`** : on fournit `{username, password}` **en clair** (deploy token GitLab read_repository) ; **l'API les range elle-même dans Vault** (ce n'est pas un `vaultPath` côté appelant). Cf. [atlas-v2 § configure-gitops](../10-platforms/atlas-v2.md).

## ⚠ Dérive « OutOfSync » ArgoCD sur les ExternalSecrets (réutilisable)
ESO **défaute** des champs absents du manifeste rendu → ArgoCD voit un diff permanent (l'ExternalSecret reste `Healthy`/synced mais l'app affiche `OutOfSync`). Pour l'éviter, **expliciter ces défauts** dans le manifeste (vérifié ESO en zone dev, 2026-05-28 ; **re-vérifié PTT 2026-06-10**, fix appliqué aux 4 ES db des wrappers chantier/lsp) :
- `spec.target.deletionPolicy: Retain` (en plus de `creationPolicy: Owner`).
- pour chaque `spec.data[].remoteRef` **et `spec.dataFrom[].extract`** : `conversionStrategy: Default`, `decodingStrategy: None`, `metadataPolicy: None`, `nullBytePolicy: Ignore`. *(Un `dataFrom[].sourceRef.generatorRef` ne reçoit, lui, **aucun** défaut.)*
- si `spec.target.template` : `engineVersion: v2`, `mergePolicy: Replace`.

> Diagnostic du diff exact : API ArgoCD `GET /api/v1/applications/{app}/managed-resources` (comparer `targetState` vs `normalizedLiveState`) — ou, sans token ArgoCD : extraire l'ES de la branche `rendered` et diff **JSON** (pas YAML : le quoting `'…'`/`"…"` fait des faux positifs) contre `kubectl get … -o json` (`.spec`).

Compléments PTT (2026-06-10) :
- 🪤 **Toute annotation posée à la main sur le live** (ex. `kubectl annotate externalsecret … force-sync=<val>` pour forcer un resync ESO) crée le même OutOfSync — elle n'est dans aucun manifest et l'apply 3-way la conserve. **La retirer après usage** (`kubectl annotate … force-sync-`).
- **Les ES émis par le chart SDPSN** (`<app>-secret`, `registry-secret`, db/redis/bucket) n'écrivent pas ces défauts → **OutOfSync résiduel hors de notre portée wrapper**. Deux suites ouvertes : faire évoluer **SDPSN-devops-charts** (écrire les défauts dans ses templates `ExternalSecret-*.yaml`) et/ou **signalement Atlas** — la composition Environment génère l'Application ArgoCD sans `ignoreDifferences` ni Server-Side Diff ; un fix plateforme (annotation `argocd.argoproj.io/compare-options: ServerSideDiff=true` ou `ignoreDifferences` sur `external-secrets.io`) réglerait le problème pour **tous** les produits.
