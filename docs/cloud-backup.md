# Cloud — Backup & Restore (Mongo + Blob)

This runbook covers the durable state iterion owns in cloud-mode:

| Surface | Backend | What lives there | Loss impact |
|---|---|---|---|
| Run docs + checkpoints | Mongo (`runs` collection) | Authoritative for resume | Runs un-resumable; events orphan |
| Events stream | Mongo (`events` collection, TTL) | Observability replay | Editor "run console" goes blank for affected runs |
| Interactions | Mongo (`interactions`) | Pause/resume answers | Affected runs stuck at `paused_waiting_human` |
| Identity + auth | Mongo (`users`/`teams`/`memberships`/`sessions`/`session_revocations`/`oidc_links`) | Login + RBAC | All users logged out, RBAC lost |
| Secrets (BYOK, OAuth, run secrets) | Mongo, encrypted with `ITERION_SECRETS_KEY` | Per-tenant credentials | Secrets unrecoverable if the secrets key is also lost |
| Artifact bodies | S3 / blob | Versioned `artifacts/<run-id>/<node>/<v>.json` | Artifacts lost; checkpoints reference dead keys |

A Mongo dump holds no plaintext session credential: `sessions` stores only
refresh-token hashes, and the desktop SSO ticket store
(`desktop_sso_tickets`) rests no credential at all — a ticket's payload is
the deferred login (account, lineage, landing team) keyed by the ticket's
SHA-256, and the session is minted at redemption
([#2635](https://github.com/SocialGouv/iterion/issues/2635);
[cloud-admin.md](cloud-admin.md) §2). The sealed BYOK/OAuth rows a dump does
hold stay unreadable without `ITERION_SECRETS_KEY`. Store, restrict and
destroy the archives accordingly.

**Critical invariant:** the Mongo backup and the blob backup must
overlap in time. A Mongo restore that references blob keys deleted
between snapshots produces dangling artifact pointers. Schedule them
in lock-step (same `cron`, same monitoring alert).

---

## Mongo backup

### Native — `mongodump` CronJob

A tightly-scoped CronJob in the same namespace as the data-plane
release. Adjust `MONGO_URI` to point at the cluster's hostname (the
chart's default Service is `<release>-mongodb`).

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: iterion-mongo-backup
  namespace: iterion
spec:
  schedule: "0 2 * * *"          # daily 02:00 UTC
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 5
  concurrencyPolicy: Forbid       # never two dumps at once
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: mongodump
              image: mongo:8.0
              command:
                - /bin/sh
                - -c
                - |
                  set -e
                  TS=$(date -u +%Y%m%dT%H%M%SZ)
                  OUT="/backup/iterion-${TS}"
                  mkdir -p "${OUT}"
                  mongodump --uri "${MONGO_URI}" \
                    --gzip \
                    --archive="${OUT}/dump.gz" \
                    --oplog
                  # Upload to your offsite store. Replace this with
                  # the appropriate CLI (aws s3 cp, gcloud storage,
                  # rclone). The PVC at /backup is a local stop-gap
                  # only — a single PVC failure loses all dumps.
                  aws s3 cp --no-progress \
                    "${OUT}/dump.gz" \
                    "s3://${BACKUP_BUCKET}/mongo/iterion-${TS}.gz"
              env:
                - name: MONGO_URI
                  valueFrom:
                    secretKeyRef:
                      name: iterion-backup
                      key: mongo-uri
                - name: BACKUP_BUCKET
                  valueFrom:
                    secretKeyRef:
                      name: iterion-backup
                      key: bucket
              volumeMounts:
                - { name: scratch, mountPath: /backup }
          volumes:
            - name: scratch
              emptyDir: {}
```

`--oplog` captures changes that happen during the dump (replica set
required; the dev Mongo of `values-dev.yaml` is a one-member replica set,
enough for `--oplog` but not for failover; production brings an external
Mongo with ≥3 replicas, as `values-prod.yaml` expects).

### Cloud-native — PVC snapshot

If the underlying storage class supports
[CSI VolumeSnapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/),
prefer snapshotting the Mongo PVCs directly: snapshot creation is O(1)
seconds rather than the linear dump time, and restore is a
PVC-from-snapshot reattach.

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: iterion-mongo-2026-05-17
  namespace: iterion
spec:
  volumeSnapshotClassName: csi-snapshotter  # cluster-specific
  source:
    persistentVolumeClaimName: <the Mongo data PVC>   # e.g. data-iterion-mongodb-0 (your Mongo chart's name)
```

Schedule via [Velero](https://velero.io/) or a controller that issues
VolumeSnapshots on a cron. The same overlap-with-blob-snapshot
constraint applies.

---

## Blob backup

The artifact bucket (`config.s3.bucket`, `iterion-artifacts`) holds the
artifact bodies, the attachments, the tool outputs, the backend sessions
and the offloaded IR — and, under `uploads/`, the uploads a launch form
staged and no run has claimed yet, transient by construction (an hour)
and nothing to keep. Inside the cluster it is replicated — SeaweedFS
`002`, three copies on three volumes — which protects a disk or a node and
nothing else: not a deletion (a human, a bug, a misplaced `deleteGone`),
not the loss of the cluster or of the region. A copy OUTSIDE the cluster
does, and the chart carries its mechanics: `backup.*` renders one CronJob
per tier running rclone — the tool and the pinned image of the production
migration ([ADR-107](adr/107-seaweedfs-object-store.md)) — against an
S3-compatible bucket in another region or another provider.

```yaml
backup:
  enabled: true
  destination:
    endpoint: https://s3.<another-region>.example   # NOT the cluster's region
    region: <another-region>
    bucket: iterion-artifacts-backup                  # copied into <bucket>/<namespace>/<release>/ (destination.path)
    provider: Other                                   # rclone's S3 provider name
    existingSecret: iterion-backup-destination        # RCLONE_CONFIG_DST_ACCESS_KEY_ID + RCLONE_CONFIG_DST_SECRET_ACCESS_KEY
```

- **Credentials by Secret only.** The destination's come from
  `existingSecret` (sealed-secrets, external-secrets, a Secret applied by
  hand); the source's are the deployment's own storage Secret. The chart
  renders no credential value, and refuses an enabled backup without its
  destination, its Secret or a pinned image.
- **Pinned image.** `backup.image.digest` (rclone 1.71.1 by default) — the
  render refuses a tag.
- **Tiers** (`backup.tiers`, the defaults are the table below): `daily`
  **syncs** the bucket into `<bucket>/current/` and moves what the sync would
  delete or overwrite under `current-deleted/<timestamp>/`, expired after 7
  days — a deletion in the source stays recoverable for a week; `weekly`
  and `monthly` **snapshot** the whole bucket under `weekly/<timestamp>/`
  (kept 35 days) and `monthly/<timestamp>/` (kept 400 days, 13 months).
- **Every run proves its copy.** It ends with `rclone check --one-way
  --size-only` (`backup.checkArgs`) against the copy it just made, over the
  objects that existed when the copy started (`--min-age`: the bucket is
  live, an object written meanwhile is next run's): the Job is red when a
  source object is missing from the destination. `--size-only` because a
  multipart ETag is not an MD5. A sync that would delete more than
  `backup.maxDelete` objects (1000) aborts red instead of mirroring an
  emptied source; only timestamp-named directories are ever expired.
- **Alerts** (with `metrics.prometheusRule.enabled`, kube-state-metrics
  series): `IterionArtifactBackupFailed` per tier (its last scheduled run is
  over and did not succeed, critical — it resolves on the next green run),
  `IterionArtifactBackupStale` per tier (no success within its
  `maxAgeHours`), `IterionArtifactBackupNeverSucceeded` per tier (the
  CronJob is older than `maxAgeHours` and no success is recorded — which is
  also what a kube-state-metrics nobody scrapes looks like, so a silent copy
  cannot pass for a working one).
- **Logs never carry the keys**: rclone prints every option it read from
  the environment at debug level, so `-v`/`-vv`/`--log-level`/`--dump` are
  refused in `backup.extraArgs` and `checkArgs`; a filter in `extraArgs`
  must be repeated in `checkArgs`, the check reads the source too.
- **NetworkPolicy.** With `networkPolicy.enabled`, add the destination
  endpoint to `networkPolicy.egress.extraAllow`: the backup pods carry the
  chart's selector labels and sit under the same deny-all egress.
- **The destination's own protections stay yours:** versioning on the
  destination bucket and a lifecycle rule on the expired prefixes complement
  the tiers; another region or another provider is what the destination
  choice buys.

`backup.enabled` is false by default: no copy exists until an operator sets
the destination and its Secret. The chart proves the manifest of the copy
(`task chart:check` renders the tiers and refuses a missing guard), never a
restore — the drill below is what proves that.

---

## Retention policy

Recommended baseline (tune to your compliance posture):

| Tier | Frequency | Retention | Storage |
|---|---|---|---|
| Hot | Daily dump + blob `sync` (`current/`, deletions kept under `current-deleted/`) | 7 days | Offsite, another region |
| Warm | Weekly blob `snapshot` (`weekly/<timestamp>/`) | 35 days | Another region |
| Cold | Monthly blob `snapshot` (`monthly/<timestamp>/`) | 13 months (400 days) | Glacier / Archive tier |

The three blob tiers are the chart's `backup.tiers` defaults; a Mongo dump
on the same cadence (above) keeps the two in lock-step.

Mongo dump size grows with `events` collection — the config's
`mongo.events_ttl_days` (env `ITERION_MONGO_EVENTS_TTL_DAYS`, default 90)
bounds it. A heavy
operator-driven backfill of long runs can spike this temporarily.

---

## Restore drill

Run this at least quarterly against a sacrificial namespace. A backup
that has never been restored is unverified.

1. **Provision a clean restore target** (separate namespace, separate
   bucket prefix). Do **not** restore on top of a live deployment.
2. **Restore the blob bucket first** — Mongo references blob keys, so
   the keys must exist before any run doc consults them. From the chart's
   off-cluster copy, run rclone the other way round, into the RESTORE
   bucket, never the live one (the two remotes as the CronJob defines them,
   `RCLONE_CONFIG_DST_*`, plus a `restore` remote for the target):
   ```sh
   # DST_BUCKET is <bucket>/<namespace>/<release> as the CronJob renders it
   rclone copy "dst:${DST_BUCKET}/current" "restore:${RESTORE_BUCKET}"              # the daily mirror
   rclone copy "dst:${DST_BUCKET}/weekly/<timestamp>" "restore:${RESTORE_BUCKET}"   # or a dated snapshot
   rclone check --one-way --size-only "dst:${DST_BUCKET}/current" "restore:${RESTORE_BUCKET}"
   ```
   An object deleted from the live bucket lives under
   `current-deleted/<timestamp>/` for the tier's `keepDays`; copy it back
   the same way.
3. **Restore Mongo from `--archive`**:
   ```sh
   aws s3 cp s3://${BACKUP_BUCKET}/mongo/iterion-${TS}.gz - \
     | mongorestore --uri "${RESTORE_MONGO_URI}" \
       --gzip \
       --archive \
       --drop \
       --oplogReplay   # only when the dump used --oplog
   ```
4. **Re-stamp the KMS key** if you rotate per-environment. Without it
   `pkg/secrets` cannot unseal stored BYOK / OAuth bundles and the
   per-tenant credentials path stays broken.
5. **Bring up an iterion server pod against the restored data plane**
   with `disableAuth=false` and verify:
   - `/readyz` returns 200.
   - `kubectl exec` into the server pod and `curl -sf
     localhost:4891/api/runs?limit=1` returns a known historical run.
   - A previously paused run can be resumed (the most stringent path:
     it exercises Mongo, blob, and the checkpoint together).
6. **Tear down** the restore namespace. Add the drill timestamp + any
   anomalies to your runbook log.

---

## Things that are **not** backed up by this runbook

- **JetStream durable state** (`pkg/queue/nats`): the runner consumes
  messages in flight. A Mongo restore re-creates the durable run docs;
  any in-flight queue messages from the lost epoch are lost. iterion's
  cooperative cancel + checkpoint design means partially-executed runs
  resume from the last checkpoint, not from the queue. Document this
  in your post-restore checklist.
- **Editor session cookies**: stateless JWTs; users re-login after
  restore. No persistence to back up.
- **Dispatcher in-flight retry windows**: same as JetStream — restore
  re-creates the tracker view; in-flight retries restart from the
  tracker's source of truth (GitHub Issues, Forgejo, native kanban).

For the JetStream gap there is no chart-provided backup value — the
bundled `nats` sub-chart only exposes `nats.enabled` +
`nats.config.jetstream.enabled`. Back the durable streams up with NATS'
own tooling (`nats stream backup <stream>` / `nats account backup`, or a
volume snapshot of the JetStream file store) if your durable subjects
exceed the loss tolerance — that's an explicit operator choice, not a
default.

See also:

- [docs/cloud-troubleshooting.md](cloud-troubleshooting.md) — symptom
  → diagnosis recipes that complement this runbook.
- [docs/cloud-admin.md](cloud-admin.md) — day-2 tenant + RBAC ops.
- [docs/cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md)
  — hardening prerequisites before any public exposure.
