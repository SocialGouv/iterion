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

**Critical invariant:** the bucket copy starts after the Mongo dump
ends. A dump taken with `--oplog` restores the base as it stood when the
dump ended, so every object it references existed then. Two jobs on the
same cron do not even give that: a copy that finishes before the dump
misses the objects the dump's last writes point to, and both jobs still go
green. The [cadence](#recovery-objectives) below orders them, and step 2
of the [restore drill](#restore-drill) pairs a dump with a copy by their
timestamps.

The order is necessary, not sufficient. A copy started after the dump
holds what still exists then, and the `sync` tier moves under
`current-deleted/<timestamp>/` what it deletes or overwrites — the version
the PREVIOUS copy held, not one written since. So an object written after
the previous copy and deleted or rewritten before this one is in neither
place: the restored row that references it reads a missing object (a
deleted artifact, upload or IR answers `404`) or a newer version than its
checkpoint (a backend session or a scratch file rewritten in that window).
That residue lives between the previous copy and the dump; the cadence
below bounds it to half a day. A consistent cut needs either the source
bucket's object versioning, restored at the dump's end, or the writers
quiesced across the pair: neither is in place at this version.

---

## Recovery objectives

The objectives the deployment commits to, and what in this runbook holds
each one:

| Objective | Value | Held by |
|---|---|---|
| **RPO** (maximum data loss) | **24 hours** | Two pairs a day, below: everything written since the dump of the last pair is lost, nothing older — about 14 hours at worst (12 h between pairs, the hour between a dump and its copy, the copy's own run), within the objective; the residue named above — objects rewritten or deleted between two copies — is not covered. The bucket half is the chart's `backup.*`; the Mongo half is the operator's CronJob ([below](#mongo-backup)), and the RPO holds only once that CronJob has a green run against the off-cluster destination and a drill has restored its dump. |
| **RTO** (maximum time to serve again) | **8 hours** | The [restore drill](#restore-drill) on a fresh namespace — objects first, then the base — measured end to end; the [drill log](#drill-log) is the witness. It assumes the live side still answers step 6's replay; with the live side lost too, step 6 fails closed and the RTO covers the data plane only. |
| Drill cadence | quarterly | The [drill log](#drill-log): an objective nobody measured is a wish. |

**Two pairs a day: the Mongo dump at 01:30 and 13:30 UTC, the bucket copy
at 02:30 and 14:30.** Set the `daily` tier to it,
`backup.tiers[daily].schedule: "30 2,14 * * *"` (the chart's default runs
once, at 02:30, which pairs only once a day and leaves the worst case past
24 hours); the Mongo CronJob below runs an hour earlier and names its
archive with the dump's start AND end, so a pair is checked, not assumed.
A dump still running at its copy's start does not pair with that copy,
only with the next one, twelve hours later, and for that half-day the
worst case passes 24 hours: watch the dump's duration
(`kube_job_status_completion_time` − `kube_job_status_start_time` of its
Jobs) and move the dumps earlier before it nears the hour. A CronJob
schedule is read in the time zone of the cluster's controller manager (the
chart renders no `timeZone`, and the recipe below sets none): on a cluster
whose controller is not on UTC both run at those hours in that zone and the
order holds — check it on the target cluster and write the zone down with
the deployment's values. The `weekly` (Sunday 03:00) and `monthly` (1st,
04:00) tiers are snapshots that start after the night's dump and do not
move the RPO. `IterionArtifactBackupStale` fires one `for:` hour after the
`daily` tier's `maxAgeHours` without a success: lower it to 18 with two
pairs a day (the chart's 36 is sized for one), so a missed pair is seen
before the RPO is spent; `IterionArtifactBackupFailed` fires on a failed
run. The chart renders no rule for the Mongo CronJob: give it the same
pair, on the same kube-state-metrics series
(`kube_cronjob_status_last_successful_time` and
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
  schedule: "30 1,13 * * *"      # 01:30 and 13:30 — each ends before its bucket copy (02:30, 14:30)
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
                  START=$(date -u +%Y%m%dT%H%M%SZ)
                  # Dump to the scratch volume first, then upload: a pipe
                  # in sh would hide a failed mongodump behind rclone's exit.
                  mongodump --uri "${MONGO_URI}" --gzip --oplog \
                    --archive="/backup/dump.gz"
                  # --oplog restores the base as of the dump's end: the
                  # name carries it, and a restore pairs the dump only
                  # with a bucket copy that started later.
                  END=$(date -u +%Y%m%dT%H%M%SZ)
                  /tools/rclone copyto "/backup/dump.gz" \
                    "dst:${DST_BUCKET}/mongo/iterion-${START}-${END}.gz"
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
VolumeSnapshots on a cron. The same order applies: a snapshot's point in
time is its creation, and it pairs only with a bucket copy that started
after it.

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
| Hot | Twice-daily dump + blob `sync` (`current/`, deletions kept under `current-deleted/`) | 7 days | Offsite, another region |
| Warm | Weekly blob `snapshot` (`weekly/<timestamp>/`) | 35 days | Another region |
| Cold | Monthly blob `snapshot` (`monthly/<timestamp>/`) | 13 months (400 days) | Glacier / Archive tier |

The three blob tiers are the chart's `backup.tiers` defaults; each one
starts after the day's Mongo dump has ended ([above](#recovery-objectives)),
so each pairs with it.

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
     off or paused — until step 7;
   - the Kubernetes sandbox off for the whole drill, `runner.sandbox.enabled:
     false` (the pools follow it): a sandbox pod carries other labels than
     the release's pods, so the chart's NetworkPolicy does not select it, and
     a restored runner creating one would reach the outside with restored
     credentials.
2. **Restore the blob bucket first** — Mongo references blob keys, so
   the keys must exist before any run doc consults them. **Pick the pair
   before copying anything**: the newest dump
   (`mongo/iterion-<start>-<end>.gz`) whose `<end>` precedes the start of
   a copy that went green. A green copy's last log line is
   `backup <tier>: <mode> ok <ts>`, `<ts>` being its start (a dated
   snapshot's directory carries the same `<ts>`). A dump with no green copy
   started after its end is not restored: take an older one. With the
   cluster lost, its Job logs go with it, and at this version the
   off-cluster bucket holds no witness of which copy went green: pair the
   dump with the newest copy that started after its end (a `weekly/<ts>/`
   or `monthly/<ts>/` directory, or the daily mirror when a
   `current-deleted/<ts>/` directory newer than the dump's end shows a copy
   ran), and enter the pair as unproven in the [drill log](#drill-log).
   From the chosen copy, run rclone the other way round, into the RESTORE
   bucket, never the live one (the two remotes as the CronJob defines them,
   `RCLONE_CONFIG_DST_*`, plus a `restore` remote for the target):
   ```sh
   # DST_BUCKET is <bucket>/<namespace>/<release> as the CronJob renders it
   rclone copy "dst:${DST_BUCKET}/current" "restore:${RESTORE_BUCKET}"              # the daily mirror
   rclone copy "dst:${DST_BUCKET}/weekly/<timestamp>" "restore:${RESTORE_BUCKET}"   # or a dated snapshot
   rclone check --one-way --size-only "dst:${DST_BUCKET}/current" "restore:${RESTORE_BUCKET}"
   ```
   Then bring back, from every `current-deleted/<timestamp>/` directory
   whose `<timestamp>` is after the dump's `<end>` (they live for the
   tier's `keepDays`), only the objects the restore bucket does not hold:
   what the live side deleted after the backup, which the restored rows
   still reference (step 6 deletes again what was deleted). Oldest
   directory first, so of two versions of a deleted object the one closest
   to the dump stays:
   ```sh
   rclone copy --ignore-existing "dst:${DST_BUCKET}/current-deleted/<timestamp>" "restore:${RESTORE_BUCKET}"
   ```
   Never overwrite: a directory's `<timestamp>` is the copy that moved an
   object there, not the moment the object changed — a copy archives the
   version the previous copy held, which can predate the dump and would
   replace the right one. An object overwritten after the backup so comes
   back in the copy's newer version, part of the residue (the invariant
   above). An older directory holds what was deleted before the backup:
   leave it out.
3. **Restore Mongo from `--archive`** — the dump of step 2's pair, as the
   [CronJob](#native--mongodump-cronjob) wrote it under `mongo/`:
   ```sh
   rclone cat "dst:${DST_BUCKET}/mongo/iterion-${START}-${END}.gz" \
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
   (`GET /api/admin/audit?action=org.deletion_canceled&from=<dump start>`,
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
   - **The set is every row, not a filter.** Read every live
     `audit_events` row, both scopes (`platform` and `tenant`, which also
     holds the org-scoped rows), with `created_at` after the dump's
     `<start>` (a margin: what the dump already holds replays as a no-op),
     and go through all of them, oldest first: any action after the backup
     changed, removed or restricted something the dump holds as it was —
     a list of action patterns misses the next one (`user.sessions_revoked`,
     `team.run_visibility_changed`, `forge.connection.token_replaced` fit
     none of the obvious suffixes). A row whose object the restored side
     does not hold is a creation after the backup, lost with the
     [RPO](#recovery-objectives): there is nothing to replay for it.
     [cloud-admin-guide.md §1.5](cloud-admin-guide.md#15-audit-log) lists
     the stable tokens; the `auditTenant`/`auditOrg`/`auditPlatform`/
     `auditSystem` call sites are authoritative. Among them, a
     `schedule.deleted` brings back a schedule that launches a bot again,
     a `run.deleted` brings back a run and its content, and an update can
     undo a restriction: `user.updated` carries an account's status
     (disabled) and its super-admin flag (demoted), `schedule.updated` a
     disabled schedule, `platform.settings.sandbox.updated` a tightened
     egress.
   - **Removals, demotions, revocations and suspensions are redone**
     through the restored API: delete again what was deleted, remove the
     member, revoke the token or the sessions, set the role, the status or
     the visibility the live side holds.
   - **Updates are reconciled with the live object**: read it on the live
     side and write its current value on the restored side — for
     `user.updated`, the account's status and super-admin flag
     (`PATCH /api/admin/users/{id}`); for the others, the setting, policy
     or flag the row names. A `*.updated` on a credential's value
     (`byok.updated`, `secret.updated`, `*.llm_key.updated`) is a rotation,
     below.
   - **Rotations cannot be replayed**: the new value exists only live. On
     the restored side, revoke or delete the stale credential (personal
     access token, webhook token, config-share token, secret, BYOK or LLM
     key) so the operator or the tenant sets it again, and force a password
     reset for every user whose password was changed or reset.
   - **Organisation erasures.** An admin schedules an organisation's
     deletion (`purge_after` = now + `orgDeletionGrace`, 24 h, in
     `pkg/server/admin_orgs_routes.go`), and the org-purge sweeper
     (`pkg/cloud/orgsweep`, nightly plus a catch-up at boot) hard-purges it
     once that has passed: every run with its objects in the bucket first,
     then its Mongo data — in the bucket the server points at, live or
     restored.
     - An org the dump holds as `pending_deletion` with a `purge_after`
       already past needs nothing: the restored server's sweeper purges
       it at boot — unless its deletion was canceled live after the
       backup, which step 5 put back before the boot.
     - The deletions scheduled after the backup are the
       `org.deletion_scheduled` rows of the platform log
       (`GET /api/admin/audit?action=org.deletion_scheduled&from=<dump start>`,
       and `org.deletion_canceled` for the ones to cancel again); schedule
       each one again on the restored side with the same admin action.
     - A purge takes the org's audit rows with it (`audit_events` rows
       whose `tenant_id` is the org), so an org scheduled AND purged after
       the backup has no row left: diff the org ids of the dump against a
       complete enumeration of the live Mongo `orgs` collection — not
       `GET /api/admin/orgs`, which answers at most 500 organisations. An
       org of the dump missing live was erased after the backup: confirm
       each missing id against the live database before scheduling its
       deletion again; a failed or incomplete read is not evidence of an
       erasure.
     - A replayed deletion blocks the org at once but purges it only once
       its grace has elapsed, 24 h or more after the replay: the end of the
       RTO is not the end of the erasure.
   - **Account erasures** (`user.erased`, [ADR-159](adr/159-identity-data-lifecycle-erasure-by-anonymization.md))
     are replayed per account: for the `target_id` of each line, and for
     each request of the DPO's register answered since the restored point,
     `POST /api/admin/users/{id}/erase` (or `iterion remote admin users
     erase <id> --yes`). The route is idempotent — an account erased
     before the dump answers `already_erased: true` — and a `500` is
     repeated until it answers `200`; a line whose meta says
     `incomplete: true` is an erasure that stopped half-way, replayed like
     the others. In a drill, replay at least one line and check the
     account reads `erased`; an erasure the register names and no line
     carries is an anomaly for the [drill log](#drill-log).
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
   a drill tenant's, or one whose credentials step 6 has cut — and one that
   does not need the Kubernetes sandbox, which step 1 keeps off. Rehearsing
   a sandboxed resume needs a drill-only NetworkPolicy the chart does not
   ship: one selecting every sandbox pod and denying its egress but the
   runner's proxy (`ITERION_POD_IP`) and the cluster's API server, with the
   matching ingress on the runner's proxy port; it is outside this drill. Scheduled
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

| Date | Target | Backup restored (Mongo dump + blob copy, timestamps; the copy before it, which opens the residue's window) | Duration | Anomalies |
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
