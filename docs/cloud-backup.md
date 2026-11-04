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

## Recovery objectives

The objectives the deployment commits to, and what in this runbook holds
each one:

| Objective | Value | Held by |
|---|---|---|
| **RPO** (maximum data loss) | **24 hours** | The single daily cadence below: everything written since the last pair of copies is lost, nothing older. The bucket half is the chart's `backup.*`; the Mongo half is the operator's CronJob ([below](#mongo-backup)), and the RPO holds only once that CronJob has a green run against the off-cluster destination and a drill has restored its dump. |
| **RTO** (maximum time to serve again) | **8 hours** | The [restore drill](#restore-drill) on a fresh namespace — objects first, then the base — measured end to end; the [drill log](#drill-log) is the witness. It assumes the live side still answers step 6's replay; with the live side lost too, step 6 fails closed and the RTO covers the data plane only. |
| Drill cadence | quarterly | The [drill log](#drill-log): an objective nobody measured is a wish. |

**One cadence: 02:30 UTC, daily.** The Mongo dump and the `daily` blob tier
run on the same cron, which is the lock-step the invariant above asks for.
02:30 is the chart's `backup.tiers[daily].schedule` default; the Mongo
CronJob below is written on it. A CronJob schedule is read in the time zone
of the cluster's controller manager (the chart renders no `timeZone`, and
the recipe below sets none): on a cluster whose controller is not on UTC
the two still run together, at 02:30 in that zone — check it on the target
cluster and write the zone down with the deployment's values. The `weekly`
(Sunday 03:00) and `monthly` (1st, 04:00) tiers are snapshots layered over
the daily pair and do not move the RPO. `IterionArtifactBackupStale` fires
about 37 hours after the `daily` tier's last success (36 h `maxAgeHours` +
the rule's 1 h `for:`), and `IterionArtifactBackupFailed` on a failed run:
a missed day is seen before the RPO is doubled. The chart renders no rule
for the Mongo CronJob: give it the same pair, on the same kube-state-metrics
series (`kube_cronjob_status_last_successful_time` and
`kube_cronjob_status_last_schedule_time` of its name).

What the objectives do not cover: anything JetStream holds (see
[below](#things-that-are-not-backed-up-by-this-runbook) — a queue of at
most 24 hours of messages, rebuilt by reseed), and the editor's access
tokens (stateless JWTs; the `sessions` refresh-token hashes are in the
dump like any collection, so a restore keeps the logins that existed at
the backup's timestamp).

---

## Mongo backup

### Native — `mongodump` CronJob

The chart renders no Mongo dump: this CronJob is the operator's, applied
next to the release in the same namespace. It writes the dump to the same
off-cluster destination as the blob tiers, through the same rclone image
and the same destination Secret, under `<bucket>/<namespace>/<release>/mongo/`
— a sibling of the tiers' prefixes, which no tier syncs or expires. Adjust
`MONGO_URI` to the cluster's Mongo and the `RCLONE_CONFIG_DST_*` values to
the deployment's `backup.destination`.

The recipe is a template the chart neither renders nor tests: it holds the
RPO's Mongo half only once a run of it is green against the destination
and a [drill](#restore-drill) has restored what it wrote.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: iterion-mongo-backup
  namespace: iterion
spec:
  schedule: "30 2 * * *"         # daily 02:30 — the blob daily tier's cron, lock-step
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 5
  concurrencyPolicy: Forbid       # never two dumps at once
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        spec:
          restartPolicy: OnFailure
          # The mongo image carries mongodump, not rclone: the init container
          # copies the static rclone binary of the backup image next to it.
          initContainers:
            - name: rclone
              image: rclone/rclone@<backup.image.digest>   # the chart's pin
              command: ["cp", "/usr/local/bin/rclone", "/tools/rclone"]
              volumeMounts:
                - { name: tools, mountPath: /tools }
          containers:
            - name: mongodump
              image: mongo:<your server's major>@sha256:<digest>   # pin by digest
              command:
                - /bin/sh
                - -c
                - |
                  set -e
                  TS=$(date -u +%Y%m%dT%H%M%SZ)
                  # Dump to the scratch volume first, then upload: a pipe
                  # in sh would hide a failed mongodump behind rclone's exit.
                  mongodump --uri "${MONGO_URI}" --gzip --oplog \
                    --archive="/backup/dump.gz"
                  /tools/rclone copyto "/backup/dump.gz" \
                    "dst:${DST_BUCKET}/mongo/iterion-${TS}.gz"
              env:
                - name: MONGO_URI
                  valueFrom:
                    secretKeyRef:
                      name: iterion-backup
                      key: mongo-uri
                # <bucket>/<namespace>/<release> — the blob tiers' root.
                - { name: DST_BUCKET, value: "<bucket>/<namespace>/<release>" }
                - { name: RCLONE_CONFIG, value: /dev/null }
                - { name: XDG_CACHE_HOME, value: /tmp }
                - { name: RCLONE_CONFIG_DST_TYPE, value: s3 }
                - { name: RCLONE_CONFIG_DST_PROVIDER, value: "<backup.destination.provider>" }
                - { name: RCLONE_CONFIG_DST_ENDPOINT, value: "<backup.destination.endpoint>" }
                - { name: RCLONE_CONFIG_DST_REGION, value: "<backup.destination.region>" }
                - { name: RCLONE_CONFIG_DST_FORCE_PATH_STYLE, value: "true" }
                - name: RCLONE_CONFIG_DST_ACCESS_KEY_ID
                  valueFrom:
                    secretKeyRef:
                      name: iterion-backup-destination   # backup.destination.existingSecret
                      key: RCLONE_CONFIG_DST_ACCESS_KEY_ID
                - name: RCLONE_CONFIG_DST_SECRET_ACCESS_KEY
                  valueFrom:
                    secretKeyRef:
                      name: iterion-backup-destination
                      key: RCLONE_CONFIG_DST_SECRET_ACCESS_KEY
              volumeMounts:
                - { name: scratch, mountPath: /backup }
                - { name: tools, mountPath: /tools }
                - { name: tmp, mountPath: /tmp }
          volumes:
            - name: scratch
              emptyDir: {}               # holds one dump: size it for the largest
            - name: tools
              emptyDir: {}
            - name: tmp
              emptyDir: {}
```

Under `networkPolicy.enabled`, the Job's pods need egress to the Mongo and
to the destination endpoint: give them the release's selector labels, or a
policy of their own. Nothing expires the dumps: the destination's
lifecycle rule on the `mongo/` prefix is their retention.

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
that has never been restored is unverified. The drill is also how the
[RTO](#recovery-objectives) is measured: start the clock at step 1, stop
it when step 7 is done, and write the duration in the
[drill log](#drill-log) — a drill that takes longer than 8 hours is a
finding, not a formality.

1. **Provision a clean restore target** (separate namespace, separate
   bucket prefix). Do **not** restore on top of a live deployment, and do
   not point the restored server at the live NATS: the namespace brings
   its own. **Cut the target off from the outside world for the whole
   drill.** The restored base holds real tenants' schedules, forge
   connections and BYOK keys, including credentials revoked live since the
   backup (step 6), and the server starts its schedule ticker in `Serve`:
   a restored data plane with open egress can post or push on real
   repositories, spend tenants' LLM keys and send mail. So:
   - `networkPolicy.enabled: true`, with `networkPolicy.egress.extraAllow`
     naming the restored Mongo and the restore bucket's endpoint and
     nothing else (DNS and the bundled NATS need no rule), and
     `networkPolicy.ingress.extraAllow` naming only the source of the
     port-forward of step 5 (the node, as an `ipBlock`);
   - every runner at 0 — `runner.replicas`, each pool's replicas, and KEDA
     off or paused — until step 7.
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
3. **Restore Mongo from `--archive`** — the dump the
   [CronJob](#native--mongodump-cronjob) wrote under `mongo/`:
   ```sh
   rclone cat "dst:${DST_BUCKET}/mongo/iterion-${TS}.gz" \
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
   with `disableAuth=false`, the runners still at 0 and the egress of step
   1, no route from outside the namespace (reach it with `kubectl
   port-forward` or `kubectl exec`). **Before the pod starts**, put back
   the organisations whose deletion was canceled after the backup: the
   org-purge sweeper runs a catch-up at boot and would hard-purge, data
   and blobs, an org the dump holds as `pending_deletion` with a
   `purge_after` already past, before step 6 could replay the
   cancellation. Read the cancellations on the live side
   (`GET /api/admin/audit?action=org.deletion_canceled&from=<backup timestamp>`,
   or its Mongo) and set each org back in the restored Mongo — what the
   restore route does (`auth.Service.RestoreOrg`):
   ```sh
   mongosh "${RESTORE_MONGO_URI}" --eval \
     'db.orgs.updateOne({_id: "<org id>", status: "pending_deletion"}, {$set: {status: "active"}, $unset: {purge_after: ""}})'
   ```
   When the live side cannot answer, see the last item of step 6 first.
   Then verify:
   - `/readyz` returns 200.
   - `kubectl exec` into the server pod and `curl -sf
     localhost:4891/api/runs?limit=1` returns a known historical run.
6. **Replay the erasures and revocations made after the backup.** A dump
   holds every organisation, member, token and secret as it was at its
   timestamp: whatever was erased, revoked or rotated since then comes
   back with the restore. Replay through the restored server's API (step
   5's port-forward) before any route reaches it: in a drill, rehearse the
   step and time it; on a real restore, the route opens only once it is
   done. Everything below is read on the LIVE side (its API, or its Mongo
   when the live server is gone but the database answers), never from the
   dump.
   - **The set is defined by a rule, not a list.** Read every live
     `audit_events` row, both scopes (`platform` and `tenant`, which also
     holds the org-scoped rows), with `created_at` after the backup
     timestamp, and keep the actions matching `*.deleted`, `*.removed`,
     `*.revoked`, `*.rotated`, `*.role_changed`, `*.status_changed`,
     `*.deletion_scheduled`, `*.deletion_canceled`,
     `user.password_changed`, `user.password_reset`, `byok.updated`,
     `secret.updated`, `*.llm_key.updated`.
     [cloud-admin-guide.md §1.5](cloud-admin-guide.md#15-audit-log) lists
     the stable tokens; the `auditTenant`/`auditOrg`/`auditPlatform`/
     `auditSystem` call sites are authoritative. Among them, a
     `schedule.deleted` brings back a schedule that launches a bot again,
     and a `run.deleted` brings back a run and its content.
   - **Removals, demotions and suspensions are redone** through the
     restored API: delete again what was deleted, remove the member,
     revoke the token or the sessions, set the role or the status the live
     side holds.
   - **Rotations cannot be replayed**: the new value exists only live. On
     the restored side, revoke or delete the stale credential (personal
     access token, webhook token, config-share token, secret, BYOK or LLM
     key) so the operator or the tenant sets it again, and force a password
     reset for every user whose password was changed or reset.
   - **Organisation erasures.** An admin schedules an organisation's
     deletion (`purge_after` = now + `orgDeletionGrace`, 24 h, in
     `pkg/server/admin_orgs_routes.go`), and the org-purge sweeper
     (`pkg/cloud/orgsweep`, nightly plus a catch-up at boot) hard-purges its
     Mongo data once that has passed; the purge does not touch the bucket,
     live or restored.
     - An org the dump holds as `pending_deletion` with a `purge_after`
       already past needs nothing: the restored server's sweeper purges
       it at boot — unless its deletion was canceled live after the
       backup, which step 5 put back before the boot.
     - The deletions scheduled after the backup are the
       `org.deletion_scheduled` rows of the platform log
       (`GET /api/admin/audit?action=org.deletion_scheduled&from=<backup timestamp>`,
       and `org.deletion_canceled` for the ones to cancel again); schedule
       each one again on the restored side with the same admin action.
     - A purge takes the org's audit rows with it (`audit_events` rows
       whose `tenant_id` is the org), so an org scheduled AND purged after
       the backup has no row left: diff the org ids of the dump against
       the live `GET /api/admin/orgs` — an org of the dump that no longer
       exists live was erased after the backup; schedule its deletion
       again.
     - A replayed deletion blocks the org at once but purges it only once
       its grace has elapsed, 24 h or more after the replay: the end of the
       RTO is not the end of the erasure.
   - **Account anonymisation** is not an admin action at this version:
     there is nothing to replay. An erasure the platform performed and
     this step cannot replay is an anomaly for the [drill log](#drill-log).
   - **When the live side is lost too** — cluster and Mongo gone, the case
     the off-cluster copy exists for — the audit rows written after the
     backup are lost with it, and nothing names what to replay. The
     restore then fails closed, before any route opens:
     - every bearer credential the restore brings back is cut: revoke all
       sessions, personal access tokens, webhook and config-share tokens,
       and force a password reset for every account; tenants rotate their
       BYOK and LLM keys at the provider, since a key revoked there after
       the backup is unknowable here;
     - no organisation is purged on the dump's word: before step 5, push
       the `purge_after` of every `pending_deletion` org of the dump past
       the boot (`db.orgs.updateMany({status: "pending_deletion"},
       {$set: {purge_after: new Date(Date.now() + 7*24*3600*1000)}})`), then confirm each pending
       deletion with its tenant and replay it or restore the org;
     - the window between the backup and the loss (up to the
       [RPO](#recovery-objectives)) is entered in the DPO's register: the
       deletions and erasures asked in it are asked again of their
       requesters, from the register's own records.
     The RTO then covers the data plane (steps 1 to 5 and 7); the
     confirmations with tenants and the DPO's re-requests run after it.
     The structural fix, an off-cluster export of the replay set with each
     backup, is not in place at this version.
7. **Resume a paused run**, still under the egress of step 1 (the most
   stringent path: it exercises Mongo, blob, and the checkpoint together).
   Bring one runner up and resume a run chosen to have no external effect —
   a drill tenant's, or one whose credentials step 6 has cut. Scheduled
   runs queued since step 5 reach that runner too: the egress of step 1 is
   what keeps them from acting outside.
8. **Tear down** the restore namespace. Add the row to the
   [drill log](#drill-log): date, backup restored, measured duration,
   anomalies.

---

## Drill log

The witness of the [recovery objectives](#recovery-objectives): one row per
drill, in this file, without the bucket name or the endpoint (the
deployment's values hold those). The duration is measured from step 1 to
the end of step 7 (the restored server verified, the erasures replayed, a
paused run resumed), and the RTO is held only while every row stays under
8 hours. An empty table is the honest state — it means the objectives
above are published, not yet proven — and the next audit reads it as such.

| Date | Target | Backup restored (Mongo dump + blob copy, timestamps) | Duration | Anomalies |
|---|---|---|---|---|

No drill has run against this chart's copy yet.

---

## Things that are **not** backed up by this runbook

- **JetStream durable state** (`pkg/queue/nats`) — by decision, not by
  omission: Mongo is the source of truth for every run, and JetStream
  holds nothing older than the figures below. A restored stream from
  another epoch would re-offer runs the Mongo checkpoint already settled
  — a double execution — which is worse than the loss. Reseed instead
  (procedure below).
- **Editor access tokens**: stateless JWTs, nothing to back up. The
  refresh-token hashes (`sessions`, `session_revocations`) are Mongo
  collections and travel with the dump: a login that existed at the
  backup's timestamp survives the restore, one minted after it is gone
  (the user logs in again).
- **Dispatcher in-flight retry windows**: same as JetStream — restore
  re-creates the tracker view; in-flight retries restart from the
  tracker's source of truth (GitHub Issues, Forgejo, native kanban).

### What JetStream holds, and for how long

| State | Name (defaults) | Bound | Source |
|---|---|---|---|
| Run work queue (shared, and one per runner pool) | `ITERION_RUNS`, `ITERION_RUNS_POOL_<pool>` | WorkQueue, `MaxAge` 24 h, dedup window 5 min | `DefaultStreamMaxAge`, [pkg/queue/nats/nats.go](../pkg/queue/nats/nats.go) |
| Dead-letter queue (shared, and one per pool) | `ITERION_RUNS_DLQ`, `ITERION_RUNS_POOL_<pool>_DLQ` | Limits, `MaxAge` 7 days | `DefaultDLQMaxAge` |
| Run leases | KV `iterion-run-locks` | TTL 60 s, refreshed every 20 s by the holder | `DefaultLockTTL`; `HeartbeatInterval` in [pkg/runner/loop.go](../pkg/runner/loop.go) |
| Runner-generation high-water mark | KV `iterion-runner-rollout` | No TTL; one integer, re-created at `config.rollout.runnerEpoch` by the first process that claims (the server, at reseed step 3) | [pkg/queue/nats/rollout.go](../pkg/queue/nats/rollout.go) |

Every stream and bucket is `CreateOrUpdate`d at boot: a shared connection
(the server, a shared runner) creates the shared stream pair and both KV
buckets (`EnsureSchema`); a pool runner's connection creates its pool's
stream pair and both KV buckets; the server's runner-pool reconciler
creates the stream pair of every declared pool (`EnsurePoolSchema`). An
empty JetStream is a valid starting state, and that is the whole reseed.

### Reseed procedure

For a lost or corrupted JetStream file store, or a new cluster, on a
namespace whose Mongo and bucket are intact or just restored:

1. **Drain the runners first.** Pause what would scale them back before
   scaling them: KEDA (the annotation `autoscaling.keda.sh/paused-replicas:
   "0"` on each runner ScaledObject — with `runner.keda.minReplicas` at 1,
   KEDA otherwise brings a runner back) and the GitOps auto-sync of the
   application (Argo CD's self-heal restores the rendered replica count).
   Then scale the runner Deployments (shared and pools) to 0 and wait for
   them to exit. A run still executing when its lease bucket vanishes fails
   its next refresh (`Lock.Refresh` CAS) and parks `failed_resumable` at its
   last checkpoint — recoverable, but a drain avoids it.
2. **Start NATS empty** (or with the file store it has: a partial store is
   worse than none). On a clustered JetStream with replicated streams
   (`config.nats.streamReplicas: 3` in `values-prod.yaml`), one member's
   store is rebuilt from its peers: scale the NATS StatefulSet to 0, delete
   the JetStream PVC (or data directory) of EVERY member, then scale it back
   — it comes back blank.
3. **Start the server**: its connection re-creates the shared stream
   pair and the two KV buckets, its pool reconciler the per-pool
   streams, and its claim re-creates the high-water mark at
   `config.rollout.runnerEpoch`. Then lift the KEDA pause and the auto-sync
   of step 1, and let the runners (shared and pools) come back to their
   replica count. Server and runners carry the same epoch value, so the
   mark comes back at the current epoch whichever process claims first, and
   no `kubectl rollout restart` is needed for it.
4. **Let Mongo re-drive the runs.** Nothing is re-enqueued by hand:
   - a run that was `queued` has no message any more; the orphan sweeper
     ([cloud-architecture.md](cloud-architecture.md), "orphan sweeper")
     flips it to `failed_resumable` once it is stale past the redelivery
     window (about 2 h 40 with the defaults); that pass waits while a
     backlog is reported, and a reseeded queue reports none;
   - a run that was `running` with no lease is flipped the same way after
     10 minutes;
   - every `failed_resumable` run is then resumed from its Mongo checkpoint
     by the operator (`iterion resume <run-id>`, or Retry in the studio),
     which publishes a fresh message.
5. **The DLQ is gone and that is fine**: a parked message pointed at a run
   doc that is still in Mongo, with a `run_delivery_exhausted` event on its
   timeline; the orphan sweeper settles that run like any other run left
   without a message (step 4). Triage from the run list, not from the
   queue.

A deployment whose durable subjects exceed this tolerance may still back
the streams up with NATS' own tooling (`nats stream backup <stream>`, or a
volume snapshot of the JetStream file store); the chart renders nothing for
it — the bundled `nats` sub-chart exposes `nats.enabled` and
`nats.config.jetstream.enabled` only — and a restored stream must never be
mounted beside a Mongo that is newer than it. That is an explicit operator
choice, not a default.

See also:

- [docs/cloud-troubleshooting.md](cloud-troubleshooting.md) — symptom
  → diagnosis recipes that complement this runbook.
- [docs/cloud-admin.md](cloud-admin.md) — day-2 tenant + RBAC ops.
- [docs/cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md)
  — hardening prerequisites before any public exposure.
