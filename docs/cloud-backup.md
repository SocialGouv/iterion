# Cloud — Backup & Restore (Mongo + Blob)

This runbook covers the durable state iterion owns in cloud-mode:

| Surface | Backend | What lives there | Loss impact |
|---|---|---|---|
| Run docs + checkpoints | Mongo (`runs` collection) | Authoritative for resume | Runs un-resumable; events orphan |
| Events stream | Mongo (`events` collection, TTL) | Observability replay | Editor "run console" goes blank for affected runs |
| Interactions | Mongo (`interactions`) | Pause/resume answers | Affected runs stuck at `paused_waiting_human` |
| Identity + auth | Mongo (`users`/`teams`/`memberships`/`sessions`/`oidc_links`) | Login + RBAC | All users logged out, RBAC lost |
| Secrets (BYOK, OAuth, run secrets) | Mongo, encrypted with `ITERION_SECRETS_KEY` | Per-tenant credentials | Secrets unrecoverable if the secrets key is also lost |
| Artifact bodies | S3 / blob | Versioned `artifacts/<run-id>/<node>/<v>.json` | Artifacts lost; checkpoints reference dead keys |

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
required; the bundled `values-dev.yaml` Mongo runs single-replica
which is enough for `--oplog` but not for production failover; use
`values-prod.yaml` external Mongo with ≥3 replicas).

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
    persistentVolumeClaimName: data-iterion-mongodb-0
```

Schedule via [Velero](https://velero.io/) or a controller that issues
VolumeSnapshots on a cron. The same overlap-with-blob-snapshot
constraint applies.

---

## Blob backup

The S3/MinIO bucket holds artifact bodies addressed by version. For
production buckets, enable:

- **Versioning** (so a delete on the bucket is recoverable).
- **Lifecycle rule** that expires non-current versions after a grace
  window matching the Mongo backup retention (e.g. 35 days).
- **Cross-region replication** when the underlying durability SLA
  needs improvement.

For the bundled MinIO (`values-dev.yaml` only — not for production),
mirror to an offsite bucket on the same CronJob cadence as the Mongo
dump:

```yaml
# CronJob excerpt — same skeleton as above
image: minio/mc:RELEASE.2025-12-01T00-00-00Z
command:
  - /bin/sh
  - -c
  - |
    set -e
    mc alias set src "${MINIO_URL}" "${MINIO_KEY}" "${MINIO_SECRET}"
    mc alias set dst "${OFFSITE_URL}" "${OFFSITE_KEY}" "${OFFSITE_SECRET}"
    mc mirror --quiet src/iterion-artifacts dst/iterion-artifacts-backup
```

---

## Retention policy

Recommended baseline (tune to your compliance posture):

| Tier | Frequency | Retention | Storage |
|---|---|---|---|
| Hot | Daily dump + blob mirror | 7 days | Same region offsite |
| Warm | Weekly | 35 days | Different region |
| Cold | Monthly | 13 months | Glacier / Archive tier |

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
   the keys must exist before any run doc consults them.
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
