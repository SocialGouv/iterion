# ArgoCD sync liveness

A zero-LLM CronJob compares a GitHub branch HEAD with one ArgoCD Application
every five minutes. It requires the expected single Git source, a fresh
`status.reconciledAt`, `status.sync.status == Synced`, and a matching compared
revision. It never refreshes or syncs the app and never executes repository code.

Install on the **Argo control-plane cluster**, in a dedicated ops namespace.
The pod's service account can GET only the named Application and maintain its
own incident ConfigMap. No interactive Argo token is needed. Kubernetes cannot
restrict ConfigMap creation by name, hence the separate namespace; read/patch
are restricted to one state object. Secrets are mounted, not API-readable.

```sh
helm template iterion-sync charts/argocd-liveness --namespace iterion-ops \
  --set application=iterion-prod --set applicationNamespace=argocd-sre \
  --set repository=SocialGouv/infra-apps --set branch=master \
  --set credentials.webhookSecret=ops-alerts
```

Deploy this chart through the cluster's GitOps repository, pinning its source
revision and the Python image digest. The existing Secret `ops-alerts` needs a
key `url` containing the incoming ops webhook (Mattermost/Slack `{ "text": … }`,
the same sink as Iterion operator alerts). Keep its value out of values files;
use the cluster's sealed-secret mechanism. For a private GitHub repository,
`credentials.githubSecret` references another existing Secret with key `token`,
scoped only to repository contents:read. Public repos need no GitHub credential;
authenticated reads avoid sharing GitHub's unauthenticated per-IP rate limit.
HTTPS with certificate validation is mandatory; redirects are refused.

The target currently supports one HTTPS GitHub source tracking a branch.
Multiple sources, a pinned tag/SHA, a different repository/branch, unreadable
APIs, and missing/invalid timestamps are unhealthy observations, never healthy
fallbacks. This is a **sync/comparison probe**, not a rollout readiness probe:
an old successful operation is legitimate if a new repository revision changed
no manifests for this app. Continue checking Deployment generation and imageID
after a deployment-changing push.

## Incident and delivery semantics

- Default threshold: 900 seconds. A freshly discovered HEAD mismatch or API
  error starts at first observation; detection is conservative (up to the
  threshold plus one schedule interval after a push). Commit author/committer
  dates are not push times and are deliberately not used.
- An already stale reconciliation timestamp can alert on the first run.
  New HEADs and changing error causes **never reset a continuous unhealthy
  episode**. A healthy observation ends it.
- One `gitops_stalled` alert per episode, then at most one reminder per six
  hours; one `gitops_recovered` only if a stall was actually reported.
- State persists in `<release>-argocd-liveness-state`, created by the probe,
  outside Helm/Argo ownership. Upgrades and ephemeral pods do not reset it.
  Do not add this object to a GitOps desired-state template or delete it to
  silence an incident.
- `concurrencyPolicy: Forbid` plus a resourceVersion CAS lease handles duplicate
  Jobs. The 180-second lease outlives the 120-second Job deadline and expires
  after a crash. No unbounded daemon, host state, or leader memory is involved.
- Webhook failure does not consume the receipt or reset the incident age;
  subsequent jobs retry. Delivery is **at least once**: a crash after POST but
  before persisting the receipt can produce a duplicate.
- Broken state/configuration/delivery fails the Job and attempts a
  `gitops_probe_failed` notification. No URL, token or response body is logged.
  Missing Secret mounts and a dead scheduler cannot be detected from inside
  the pod: keep the cluster's failed/missing CronJob alerting enabled and watch
  `status.lastSuccessfulTime`. This probe is not a replacement for it.

Network policy must allow DNS, the Kubernetes API, `api.github.com:443`, and
the configured webhook endpoint. No Argo external ingress access is needed.

## Verification

```sh
python3 -B charts/argocd-liveness/tests/test_probe.py
devbox run -- go test ./e2e -run '^TestArgoCDLivenessProbe$' -count=1
devbox run -- helm lint charts/argocd-liveness \
  --set application=test --set repository=acme/infra \
  --set credentials.webhookSecret=ops-alerts
```

The deterministic test drives the actual probe through fake GitHub/Kubernetes
and webhook transports: state creation, moving HEADs, API authentication
failure, duplicate jobs, expired lease, failed delivery/retry, dedup, recovery,
source mismatch and stale timestamps. It sends no live messages. Before an
installation, render and server-dry-run the manifests, check the service
account's GET permission and absence of Application mutation permission, then
observe a healthy Job and its persisted state. Exercise an artificial stall
against test endpoints and a test webhook, not by delaying production.
