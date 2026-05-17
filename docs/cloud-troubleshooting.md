[← docs index](README.md) · [← cloud-deployment.md](cloud-deployment.md)

# Cloud troubleshooting

Symptoms-first reference for operators running iterion in cloud mode (Helm chart, docker-compose, or any setup using Mongo + NATS + S3-compatible blob storage). For each symptom: how to diagnose, what to check, what fixes it.

For deployment / install instructions see [cloud-deployment.md](cloud-deployment.md). For exposing iterion publicly see [cloud-public-exposure-checklist.md](cloud-public-exposure-checklist.md).

## Quick triage

```bash
# Server-side health
curl -fsS http://<server-host>:4891/healthz
curl -fsS http://<server-host>:4891/readyz
curl -fsS http://<server-host>:4891/metrics | head -20

# Runner pool status (Kubernetes)
kubectl -n <ns> get pods -l app.kubernetes.io/component=runner
kubectl -n <ns> logs -l app.kubernetes.io/component=runner --tail=200

# Queue depth (NATS)
nats stream info iterion-runs
nats consumer info iterion-runs iterion-runners

# Mongo connectivity from server pod
kubectl -n <ns> exec deploy/iterion-server -- nc -zv <mongo-host> 27017

# Blob bucket connectivity from server pod
kubectl -n <ns> exec deploy/iterion-server -- aws --endpoint-url $S3_ENDPOINT s3 ls s3://$S3_BUCKET/runs/
```

If `/readyz` returns 503: the server can reach itself but cannot reach Mongo, NATS, or blob storage. The body lists which probe failed.

## Symptoms → diagnosis → fix

### Runs queue but never start (`status: queued` for minutes)

**Probable cause**: no runner pod is consuming the NATS queue, OR runner is consuming but cannot acquire the lock, OR runner is consuming but cannot reach Mongo / blob.

Diagnose:
1. `kubectl get pods -l app.kubernetes.io/component=runner` — replicas > 0?
2. `nats consumer info iterion-runs iterion-runners` — `Num Pending` decreasing? `Num Outstanding Acks` non-zero?
3. `kubectl logs -l app.kubernetes.io/component=runner --tail=200 | grep -E 'lock|claim|mongo|blob'`

Fix:
- KEDA scaled to 0 with no runs queued is normal. Submit a run; KEDA should scale up within ~30s. If it doesn't: check `kubectl describe scaledobject iterion-runner` for KEDA controller errors.
- Lock contention (multiple runners racing on the same run): the loser will see `ErrLockHeld` in logs and Nak the message. Expected — JetStream redelivers. If *all* runners loop on `ErrLockHeld`, the run was leased and orphaned; wait for the 60s TTL or `nats kv del iterion-locks <run-id>` to force release.
- Mongo / blob unreachable: NetworkPolicy or firewall. See [networkpolicy-egress example](../charts/iterion/examples/networkpolicy-egress.yaml).

### Runs hang in `running` past their `max_duration`

**Probable cause**: runner pod was terminated mid-run (OOM, eviction, node drain), the lease expired, but no other runner picked it up; OR the engine lost its sandbox container without aborting.

Diagnose:
1. `iterion inspect --run-id <id> --events | tail -50` (or `kubectl logs … | grep <id>`) — last event before hang?
2. `nats kv get iterion-locks <run-id>` — is the lease still claimed?
3. `kubectl get events -n <ns> --sort-by='.lastTimestamp' | tail -30` — pod evictions, OOM kills?

Fix:
- If the lease is stale (`status: running` in lease but no runner pod alive): release with `nats kv del iterion-locks <run-id>` and the next runner will pick the run up via JetStream redelivery. The engine resumes from the last checkpoint.
- If the run was OOM-killed: increase `runner.resources.limits.memory` in your values overlay; some workflows (especially `claude_code` with long context) need ≥ 2 GiB.
- If the sandbox container is orphaned: `docker ps --filter ancestor=ghcr.io/socialgouv/iterion-sandbox-slim` from the runner host shows lingering containers. Restart the runner pod; the engine drains and recreates sandboxes per run.

### `/readyz` 503 with `mongo: connection refused`

**Probable cause**: server cannot reach Mongo at the configured `ITERION_MONGO_URI`.

Diagnose:
1. `kubectl exec deploy/iterion-server -- env | grep MONGO`
2. `kubectl exec deploy/iterion-server -- nc -zv <mongo-host> 27017`
3. `kubectl get networkpolicy -n <ns>` — does the egress allow port 27017 to the Mongo namespace?

Fix:
- Wrong URI: update the secret backing `ITERION_MONGO_URI` and roll the deployment.
- NetworkPolicy: see [networkpolicy-egress example](../charts/iterion/examples/networkpolicy-egress.yaml). The chart's default egress allows DNS only; cluster traffic is *not* implicit.
- TLS mismatch: cloud Mongo (Atlas, etc.) often requires TLS — set `ITERION_MONGO_URI=mongodb+srv://...?tls=true&retryWrites=true`.

### `/readyz` 503 with `blob: AccessDenied`

**Probable cause**: S3 credentials are wrong, the bucket doesn't exist, or the bucket policy denies the iterion server.

Diagnose:
1. `kubectl exec deploy/iterion-server -- env | grep -E 'S3|AWS'`
2. From inside the pod: `aws --endpoint-url $S3_ENDPOINT s3 ls s3://$S3_BUCKET/`
3. Check the bucket policy / IAM role for the access key.

Fix:
- Rotate the access key + secret in the secret backing `ITERION_S3_ACCESS_KEY_ID` / `ITERION_S3_SECRET_ACCESS_KEY` and roll the deployment.
- For MinIO: ensure the access key has `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:ListBucket` on the configured bucket.
- For AWS: prefer IAM Roles for Service Accounts (IRSA) over static keys — set `serviceAccount.annotations.eks.amazonaws.com/role-arn` in your values overlay and unset the `*_ACCESS_KEY_ID` env vars.

### Editor frontend connects but no events stream in

**Probable cause**: the WebSocket endpoint cannot reach `MongoSource` (when in cloud mode with NATS-driven runs), OR the JWT used by the editor lacks the right tenant scope, OR a proxy strips WS upgrade headers.

Diagnose:
1. Browser devtools → Network → filter on `Upgrade: websocket`. Does the handshake return 101?
2. `kubectl logs deploy/iterion-server | grep -E 'eventstream|ws|tenant'`
3. `iterion inspect --run-id <id> --events` from a TTY against the same store: do events exist?

Fix:
- 101 handshake fails behind a proxy: configure the Ingress to pass WebSocket Upgrade. Example for nginx ingress: `nginx.ingress.kubernetes.io/proxy-set-header: "Upgrade $http_upgrade"`.
- Events exist but the stream is empty: the editor is filtering by tenant mismatch. Re-login to refresh the JWT.
- MongoSource unwired: confirm `ITERION_MODE=cloud` is set on the server. Without it, `runview.service` defaults to `FilesystemSource` which won't see Mongo events.

### Runs fail with `budget_exceeded` immediately

**Probable cause**: the workflow's `max_cost_usd` / `max_tokens` is below the cost of the first node call.

Diagnose:
1. `iterion inspect --run-id <id> --events | grep -E 'budget|cost'`
2. Inspect the `.iter` source's `budget:` block.

Fix:
- Increase the budget in the workflow source, OR override per-run via `--budget-cost-usd <n>` / `--budget-tokens <n>`.
- For long-running review-fix loops, raise `max_iterations` too — a low cap forces premature termination.

### `iterion bench asymptote` shows all runs at iteration 0

**Probable cause**: the `--judge-node` flag does not match a node ID actually present in the workflow, OR no `EventEdgeSelected` events are emitted (no loop-back edges in the workflow).

Diagnose:
1. `iterion inspect --run-id <id> --events | grep -E 'edge_selected|node_finished' | head -20`
2. `iterion validate <workflow.iter>` — confirm node IDs match what `--judge-node` expects.

Fix:
- Pass the right node ID. The IR node ID is the value after `judge` / `agent` in the `.iter` source (e.g. `judge reviewer:` → `--judge-node reviewer`).
- If your workflow has no bounded loop (no `-> as loop_name(N)` edges), the bench has nothing to iterate over. Add a loop or measure a different recipe.

### Trivy CI reports a HIGH CVE

**Probable cause**: a dependency (Go module, npm package, or base image layer) has a newly-published HIGH-severity advisory. The Trivy workflow publishes SARIF and summaries; it does not fail PRs by itself unless your repository adds a separate hard gate through code scanning or branch protection.

Diagnose:
1. Read the SARIF output uploaded to the PR's "Code scanning" tab.
2. `trivy fs --severity HIGH,CRITICAL .` locally to reproduce.

Fix:
- Update the offending dependency. Most Go advisories resolve by `go get -u <module>@<version>` then `go mod tidy`.
- Container base image CVEs: rebuild from a fresh `iterion-sandbox-slim` tag. The release pipeline emits a new tag every Monday.
- Genuinely irrelevant CVE (e.g. a vulnerability only triggered by a code path iterion doesn't use): add a `.trivyignore` entry with a justification comment. Don't bypass without one — drift is how compliance findings accumulate.

### Helm chart upgrade fails with `manifests version drift`

**Probable cause**: the CI guard saw a mismatch between `charts/iterion/Chart.yaml` `appVersion` and `package.json` `version`.

Diagnose:
1. `git show HEAD -- charts/iterion/Chart.yaml package.json`

Fix:
- Run `task chart:sync-version` (bumps Chart.yaml to match package.json) and recommit.

## What's *not* here

- Application-level workflow debugging — see [resume.md](resume.md) and [workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md).
- Sandbox container debugging — see [sandbox.md](sandbox.md).
- DSL / IR errors — see [references/diagnostics.md](references/diagnostics.md).
- Editor UI bugs — file a GitHub issue.

## Escalation

If a symptom isn't covered above and `/readyz` looks healthy:
1. Capture the full event stream: `iterion report --run-id <id> --output report.md`.
2. Capture server + runner logs: `kubectl logs ... > /tmp/iterion-logs.txt`.
3. Open an issue at <https://github.com/SocialGouv/iterion/issues> with both attached. Redact API keys before posting (the privacy_filter / privacy_unfilter tools can help — see [privacy_filter.md](privacy_filter.md)).
