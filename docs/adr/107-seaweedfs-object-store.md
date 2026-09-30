# ADR-107: SeaweedFS is the object store of the cloud deployment

- **Status**: Accepted
- **Date**: 2026-09-29 (the operator's decision of 2026-09-25; the cloud control-plane epic #1407, ticket #1943)
- **Relates to**: [073-cloud-twins-for-fs-only-run-detail-seams.md](073-cloud-twins-for-fs-only-run-detail-seams.md) (tool blobs and run files in S3), [075-irref-fallback-for-oversized-cloud-queue-ir.md](075-irref-fallback-for-oversized-cloud-queue-ir.md) (the offloaded IR), [089-session-persist.md](089-session-persist.md) (backend sessions), #1852 (the sync freeze that forced the move), the deployment in SocialGouv/infra-apps (`iterion/`)

## Context

MinIO no longer distributes its community edition. On 2026-09-29, `quay.io/minio/minio` answers 401, the Docker Hub repository `minio/minio` is gone, `dl.min.io` answers 410, and `github.com/minio/minio` is archived. Production ran the official MinIO chart (4 nodes, EC:2). Two consequences were measured before anything else changed:

- the bucket Jobs could no longer pull their image, which left every Argo CD sync operation Running (#1852);
- the cluster pulls images on every container start (`AlwaysPullImages`), so a MinIO pod that restarts does not come back.

iterion's whole S3 contract lives in `pkg/store/blob/s3.go` (aws-sdk-go-v2, path style, CRC32 flexible checksums on PutObject and DeleteObjects). It uses HeadBucket, Put, Get (with Range), Head, Delete, DeleteObjects, paginated ListObjectsV2 and PresignGetObject. It needs no conditional writes, ETags, versioning or object metadata.

One property matters more than the call list: **what one pod writes, another pod reads next**. That covers artifacts (runner → server), run files, attachments (server → runner), backend sessions resumed by another runner, and the offloaded IR. Whatever replaces MinIO must give read-after-write across every gateway replica.

## Options

1. **A third-party MinIO rebuild** — rejected: nobody would be accountable for the binary that holds every tenant's artifacts.
2. **Garage** — tried and abandoned by the operator.
3. **Managed object storage** — not retained: the decision of 2026-09-25 keeps the store in the cluster. It remains the fallback if operating SeaweedFS proves too costly.
4. **SeaweedFS** — retained.

## Decision

1. **SeaweedFS, upstream chart, image pinned by digest.** Version 4.48 at the time of writing; it fixes three defects of 4.47:
   - a delayed data loss on S3 PUT when `CreateEntry` is ambiguous;
   - a 201 answered for a write that landed on no volume;
   - every S3 write hanging after a master leader change.

   The topology is 3 masters (raft), 4 volume servers keeping 3 copies of every needle (`002`), 2 filers and 2 S3 gateways.
2. **The filers share ONE metadata store**: a dedicated Valkey with Sentinel (AOF `everysec`, `noeviction`, `min-replicas-to-write 1`, a password). Two filers on local stores replicate each other asynchronously, so an object written through one gateway can be missing on the other for a while — and that is exactly the read pattern above.
3. **Nothing is reachable without a credential.**
   - Unsigned S3 requests are refused: there is never an `anonymous` identity.
   - The volume and filer HTTP APIs require JWTs, with separate write and read keys.
   - NetworkPolicies restrict every SeaweedFS port and the Valkey store to SeaweedFS itself. The S3 gateway is open only to the iterion server, the runners and the bucket Jobs.
   - The gateway's single identity reuses the application's existing key pair, so a cutover or a rollback changes only the endpoint.
   - The gateways run with `-autoCreateBucket=false`. By default, a PUT into a missing bucket creates it for an identity with the `Admin` action, so a wrong bucket name would fill a new, empty bucket instead of failing.
4. **Argo CD shapes the manifests.**
   - No value is rendered through Helm `lookup` or `randAlphaNum`: under Argo CD they re-roll on every sync. The JWT keys, the S3 identities and the Valkey password come from SealedSecrets.
   - There is no Helm hook either: a PostSync hook waits for every resource of the application to be Healthy. The bucket and migration Jobs are plain resources, named after a hash of their own spec, so a sync never re-runs them.
5. **Migration is online, and every run proves itself.** A migration run is rclone driven by a shell script, in one of two explicit modes:
   - **mirror** — only into a store nothing writes to (the render refuses the live endpoint). One bulk copy compares the source against a listing of the destination. It then deletes the destination keys the source no longer has, but only migration copies: objects carrying rclone's `mtime` metadata, which any PUT by the app drops. Objects the app wrote are listed and never deleted.
   - **catchup** — for a destination the app writes to. It copies the source's app writes modified since a given instant; a migration copy on the source came from the destination, which decides its fate, so it is left alone. That instant is copied from the log of the previous run in the same direction, so catchups chain: a key the app deleted comes back only if the source rewrote it after the previous run began. It deletes only on request (`deleteGone`), and then only the migration copies of keys the source deleted after they were copied — an attachment of a deleted run, say.

   Both modes share the same rules:
   - The newer write of a key wins. A migration copy that no longer matches the source is stale, and the source wins, whatever the times say. Between two app writes, compared at full precision with rclone's own one-second window, the one newer by at least a second wins; within a second of each other they are a conflict, and the run fails for an operator to decide.
   - No copy replaces a destination object newer than the source.
   - A catchup copies one key at a time: each destination object is looked up right before its own PUT, and a lookup error stops the run. A bulk copy looks objects up well ahead of their PUT and takes any lookup error for "absent", writing blind.
   - A copy counts only when rclone reports a status for that very key. rclone takes any error on a source lookup for "a directory", copies nothing and exits 0, so its exit code alone proves nothing.
   - A run works on the keys listed at its start, so a source that keeps being written does not fail it. Keys that change, come back, are created, rewritten or deleted during the run (the source listing's own server times tell) are listed, and the last line then says the run is not decisive.
   - Nothing but the migration Job writes either bucket with rclone: rclone's `mtime` metadata is what marks a migration copy, and the app's PutObject sets none.
   - The verification re-reads both sides byte for byte (`--download`): a size or ETag comparison lets a corrupted multipart object through.
   - A verification is trusted by coverage, not by error counts. Every key in scope that is still on the source must come back with a status, so a listing that failed fails the run. A key deleted on either side while being read is looked up again rather than failing the run.

   The cutover is one commit that swaps the endpoint. The catchup that runs after the old runners have drained (up to 8 h) is the decisive one: by then, nothing writes to MinIO. A rollback is the reverse swap, alone in its commit so that no migration setting can block it, then a reverse catchup. It does not carry over what the app deleted on SeaweedFS; MinIO keeps its copies, listed. A new cutover after a rollback starts from an emptied SeaweedFS bucket.

## Consequences

- **The Valkey store is as critical as the volumes.** Losing it leaves every object on disk but unreachable by key. Hence Retain volumes, AOF, a password and a NetworkPolicy. One residual risk is accepted: a Sentinel failover can drop the last second of metadata writes, because replication is asynchronous.
- **The migration keeps one race open:** an app write that lands on the destination between a key's lookup and its PUT can still be overwritten, because rclone has no conditional PUT. The window is one object's lookup-to-PUT. The cutover happens in a quiet window for that reason.
- **Migrated objects carry no checksum.** rclone sends none, so a read of an object the migration copied is not validated; a new write still is, since SeaweedFS stores and returns the CRC32 iterion sends (measured on 4.48). The SDK logs a WARN for every read it cannot validate, and `NewS3` turns off that one log line.
- **More moving parts than MinIO:** 11 SeaweedFS pods and 3 Valkey pods, against 4.
- **Not provided here:** an off-cluster backup of the bucket, mTLS on gRPC (the NetworkPolicies stand in), and metrics scraping. Each is a follow-up, not an implied promise.
- **Upgrading SeaweedFS** means first running `pkg/store/blob/s3_gateway_compat_test.go` against the new version, in the production topology.
- **The development stack** (`docker-compose.cloud.yml`) runs the same SeaweedFS release and digest. `task cloud:test:s3` runs the compatibility suite against it. The Helm chart shipped in this repository (`charts/iterion`) still bundles Bitnami MinIO for its dev values; replacing it is a follow-up.

## Measured, not assumed

- **The compatibility suite** (12 cases, including a write on one gateway read back on the other, and presigned URLs refused once tampered with or expired) against 4.48 in the production topology. It was run again after killing, one at a time, the Valkey master, a filer, a volume server and the raft leader.
- **The migration's defects were each reproduced before being fixed**, in the production topology with the real rclone, and each fix was seen to turn the case green:
  - an endpoint swap that deleted the app's newer writes;
  - a bulk copy and a repair that overwrote a newer write;
  - a live source that made every verification fail;
  - a repair that brought back a key the app had deleted;
  - a destination lookup that failed and was read as "absent" (through a proxy answering 503), after which the app's newer object was overwritten.

  Each fix was then made to fail again by putting the defect back.
- **Silent bucket creation** by the default gateway flags was observed, and so was the refusal with `-autoCreateBucket=false`.
- **Production figures** — object count, bytes, cutover timings, the post-cutover probe — are recorded on #1943.
