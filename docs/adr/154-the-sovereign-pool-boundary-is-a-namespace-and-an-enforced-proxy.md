# ADR-154: The sovereign pool boundary is the pool's own RBAC, a standing policy and an enforced proxy

- **Status**: Proposed (2026-11-03) — D27 (the operator) decided the mechanism, an in-cluster enforced egress proxy per pool. The operator decided the SHAPE on 2026-11-03, in his words « Namespace partagé, RBAC propre au pool et NetworkPolicy. »: the pool stays in the release's namespace, under a ServiceAccount and a Role of its own, behind a standing NetworkPolicy. This record was first published with this wave's default answer, a namespace per pool; the shape was the operator's to decide and his verdict chose the other one, so the record follows it — decisions 1 to 4 and 7, the consequences and owed items 4 and 6 that assumed a pool namespace or spoke to it are rewritten, and the per-pool namespace stands under *Options considered* as rejected, the verdict its reason. The file name keeps the first wording, because renaming a record breaks every link to it; the title now states the decision. The record becomes Accepted in the commit that lands the first SHAPE slice (decisions 1-4), which the verdict unblocks: none of the owed items 1 to 5 is a SHAPE slice — item 3 implements decision 8, item 5 decision 6, and items 1, 2 and 4 implement no decision at all — so all five may land while this record is still Proposed.
- **Date**: 2026-11-03
- **Authors**: devthejo (sessions with Claude Opus 5, and Claude Opus 5.5 for the verdict), for [#2029](https://github.com/SocialGouv/iterion/issues/2029) P5
- **Relates to**: [sovereign-pools.md](../sovereign-pools.md) (the twelve links of the chain that ship today) · [cloud-deployment.md](../cloud-deployment.md) (`runnerPools[]`, the bring-up) · [ADR-123](123-run-bundle-dek-in-envelope.md) (the per-run DEK: why a pool pod holds no key material) · [ADR-131](131-tenant-gateway-credential.md) (the tenant gateway credential, and the run's frozen content boundary) · [ADR-070](070-kubernetes-sandbox-orphan-gc.md) (the sandbox orphan GC the pool's Role must keep serving)

## Context

A sovereign runner pool isolates a team by CODE: routing, admission,
credentials and a frozen per-run stamp, on the shared install
([sovereign-pools.md](../sovereign-pools.md) lists the chain, link by link,
and each link has its witness). One promise of the ticket is not kept by
that chain: *a mis-routed call must fail, not leak*. Today it fails at
AUTHENTICATION — the pool pod holds no vendor key, so a vendor call has
nothing to send — which is a credential fact, not a network fact. A pool pod
can still open a socket to anywhere its cluster lets it.

Four measurements on `origin/main` say why the network half needs a decision
rather than a patch:

1. **The boundary is weakenable from inside.**
   `charts/iterion/templates/runner-rbac.yaml` grants `networkpolicies`
   `get/list/create/delete` to the ServiceAccount the pool pod shares
   (`runner-pool-deployment.yaml`, `serviceAccountName`), and a
   NetworkPolicy is ADDITIVE: a pod that may author one may widen its own
   egress. The ticket's own words — "the boundary must be immutable to the
   runner". The same ServiceAccount is the server's and the shared
   runner's (`server-deployment.yaml`, `runner-deployment.yaml`), and the
   rule is a namespace-bound `Role` with no `resourceNames`, so every pod
   that mounts it may author or delete any NetworkPolicy of the release's
   namespace. That Role renders only where the release provisions the
   sandbox: `runner-rbac.yaml` is wrapped in `serviceAccount.create` and
   `runner.sandbox.enabled`, the chart default leaves the sandbox off and
   `values-prod.yaml` does not turn it on — there the chart binds the
   ServiceAccount to no Role, and no identity it renders may author a
   policy.
2. **No pool has a boundary of its own.** One release-wide policy selects
   "server, runner, pools, backup" together
   (`charts/iterion/templates/networkpolicy.yaml`), it is off by default
   (`networkPolicy.enabled: false`), and its egress allowance is an operator
   list of CIDRs and selectors. NetworkPolicy has no hostname rule, so
   "egress restricted to the gateway, its forge and in-cluster services"
   cannot be *said* in it.
3. **The broker is a shared relay.** `pkg/queue/nats`'s `Config` carries a
   URL and no credential, and `Connect` passes reconnection options and a
   client name — whatever the URL carries is the only authentication. No
   `ITERION_NATS_USER/PASS/CREDS/TOKEN/NKEY` exists in the chart or in
   `cmd/iterion/nats_config.go`. In production the broker is external
   (`nats.enabled: false`), so per-pool subject ACLs are the broker's own
   configuration, not this repository's.
4. **The ambient-proxy shape is unavailable by deliberate decision.**
   `pkg/secure/httpdial` pins `Proxy: nil` on `SafeTransport` (#2338) and
   `PinnedTransport` (#2337): under an ambient proxy, `net/http` writes a
   proxy-form request — and the operator's `Proxy-Authorization` — to a
   run-chosen receiver. Both pins are the fix for that, both issues are
   closed, and they stay. So `HTTPS_PROXY` on a pool pod routes NOTHING of
   iterion's own Go clients, and a design that assumed it would is a
   boundary on paper only. The pins are not to be unwound here: the
   crossings that must cross set `Proxy` themselves, which
   `pkg/backend/compatgw` already does.

Two items the ticket lists under P5 are answered elsewhere and must not be
re-opened. **Per-pool sealing keys** are superseded by
[ADR-123](123-run-bundle-dek-in-envelope.md): `cmd/iterion/runner.go` builds
the transition ring only while the deployment still mounts key material, and
the pool template keeps `ITERION_SECRETS_KEY*` out of the pod — what remains
owed is a test pinning that invariant, not a key scheme. The shared
namespace bounds what that invariant says: it is the pod's ENV. The Secret
that carries `ITERION_SECRETS_KEY` (`secret-auth.yaml`) lives in the
release's namespace, so any Role there that grants `secrets` `get` can read
it through the API, and any that grants `update`, `patch` or `delete` can
replace or remove it — the pool's own Role included wherever it renders,
which keeps `get`, `patch` and `delete` (decision 2, and its consequence
below). **The storage residual** stays as
[sovereign-pools.md](../sovereign-pools.md) already accepts it: the pool pod
takes the deployment-wide storage secret, and what the code guarantees is
tenant mediation on every blob read.

## Options considered

**Where the boundary lives, and who may author it.**

- *A — a NAMESPACE per pool, rendered by the same release.* The runner's
  Role is namespace-bound (`kind: Role`, bound to `.Release.Namespace`), so
  a pool's own Role simply does not carry `networkpolicies` `create` or
  `delete`, and the standing policy in that namespace cannot be weakened
  by anything running in it: a pod the pool creates there falls under that
  namespace's deny whatever its labels, and may only name a ServiceAccount
  of that namespace, none of which carries the verbs. The kubernetes sandbox driver needs no change
  to follow: it reads its namespace from its own service-account mount
  (`pkg/sandbox/kubernetes.Detect`), so a pool pod places its sandbox
  siblings in the pool's namespace by construction. Cost: a second
  SA/Role/RoleBinding set, a namespace template, a release that now writes
  outside its own namespace, reaching the stores from a second one
  (cross-namespace Service DNS where a sub-chart bundles them), and pools
  out of the release PodMonitor's reach. It was this wave's default and
  this record's first decision, as the only shape that answers an operator
  audit asking "can any identity of the namespace that holds this team's
  code widen its egress?" with *no*. **Rejected by the operator's verdict
  of 2026-11-03**, which chose B knowing that cost; it stays, with C, the
  structural answer should B's residual be judged unacceptable.
- *B — keep the shared namespace: the pool's own ServiceAccount and Role,
  without the authoring verbs, and a standing NetworkPolicy over the
  pool's pods.* Less change: no namespace template, no release writing
  outside its namespace, no store address changing, the release
  PodMonitor keeping the pools. Its cost, stated when it was chosen:
  another identity of the namespace can still widen the additive policy —
  wherever the release provisions the sandbox (`runner.sandbox.enabled`),
  the server and the shared runner, which keep the shared ServiceAccount's
  `networkpolicies` `create` and `delete` (Consequences counts them
  exactly); where it does not, the chart default, the chart renders no
  Role and the cost is the operator's own bindings. **Chosen** — the operator decided it
  on 2026-11-03: « Namespace partagé, RBAC propre au pool et
  NetworkPolicy. » (a shared namespace, the pool's own RBAC, and
  NetworkPolicy). Measured against the chart, the cost is wider than it
  was stated for a pool that runs the sandbox: the pool's own token can no
  longer AUTHOR a policy, but the verbs it keeps for the sandbox driver — `pods/exec`, `pods` `create`,
  `secrets` `create` and `get` — reach the shared ServiceAccount's token
  and let it create a pod no policy selects, so through them it can still
  widen its own egress (Consequences gives each path). The audit question
  above, asked of the whole namespace or of such a pool's own token,
  answers *yes*; a pool without the sandbox mounts no token (decision 2),
  so asked of its token it answers *no*. This record keeps the verdict and states the measured cost, which
  is what the operator weighs should the residual be revisited.
- *C — a Helm release per pool.* Strongest isolation, highest operating
  cost (a release, a values file and an ArgoCD application per team).
  Rejected for now; with A it remains the structural answer named above.

**The per-run NetworkPolicy the sandbox driver synthesises.**

The driver locks each sandbox sibling's egress to its runner pod's CONNECT
proxy plus kube-dns, and its inbound to that same pod IP; when it cannot
(no pod IP) it REFUSES to start the run rather than downgrade isolation in
silence. A pool pod does NOT meet that refusal: the chart wires
`ITERION_POD_IP` for a pool Deployment too (under the same
`runner.sandbox.enabled` as the shared runner's), so the pod has the env and
loses the authoring verbs instead — it fails one step later, as
`kubernetes: apply netpolicy: … forbidden` at the manifest apply. Same
direction, different error, and a remedy an operator reads as RBAC where
the PodIP message names the Helm downward API. Which is why the shape below
has the pool DECLARE the standing boundary rather than leave the driver to
interpret a Forbidden.

- *Keep the authoring verbs on a pool.* Rejected by the ticket's own
  promise.
- *A standing default-deny policy over the pool's pods, and a pool pod
  that DECLARES it.* The standing policy covers the pool's pods and their
  sandbox siblings, selected by the pool's label (decision 1). Its
  allowances are the pool's, so it cannot select the whole namespace; and
  a separate namespace-wide default-deny is no substitute, because
  NetworkPolicy is a union — a deny-all cuts exactly the pods no other
  policy admits, which is every pod of the release when the release-wide
  policy is off (`networkPolicy.enabled: false`, the chart default) and,
  when it is on, the NATS sub-chart's pods, for which the chart ships no
  policy, plus any workload of the operator's in that namespace
  (Consequences weighs it as a hardening). The driver's
  `iterion.io/managed` label alone selects every runner's siblings, so
  the pool label is the selector. So it must itself carry what the
  per-run policy granted — a sibling reaching its runner pod's CONNECT
  proxy, which the driver advertises as the runner's own pod IP — and what
  the per-run policy DENIED, because `BuildNetworkPolicy` is two-sided:
  `policyTypes: ["Egress", "Ingress"]`, and its one `from` is that runner
  pod's `/32`. A standing object that keeps only the egress half would
  leave every sandbox sibling — a pod running the run's untrusted code,
  its per-run Secrets mounted — open to the whole cluster. So the standing
  object carries both, and the per-run policy is then not owed there; the
  per-run `network:` allowlist is enforced by the proxies, which is where a
  hostname rule can exist at all. The declaration is load-bearing: without
  it the driver cannot tell "no policy because RBAC refused" from "no
  policy needed", and continuing silently is the exact footgun its
  fail-closed refusal exists to prevent. **Chosen**, with its isolation
  delta recorded under Consequences: in both directions a pool-wide rule
  says "the pool's runner pods" where the per-run rule pinned one `/32`.
- *A ValidatingAdmissionPolicy refusing NetworkPolicy from the pool's SA.*
  Same effect, one more cluster-scoped object the operator must install and
  keep. Rejected as the primary mechanism. In the shared namespace
  admission is also the only layer that could close B's residual, because
  RBAC scopes a rule by namespace, resource and name, never by what an
  object selects or carries — and the residual is more than NetworkPolicy
  writes (Consequences) — so it stays a named later hardening, not a
  promise of this record.

**How the pool pod's own egress crosses the proxy.**

- *An ambient `HTTPS_PROXY`.* Unavailable: measurement 4.
- *Reuse the engine's in-sandbox channel* (`ITERION_SANDBOX_PROXY_ENDPOINT`,
  read by the gateway client inside the sandbox). Rejected: that name is
  the ENGINE's voice — the engine sets it per run and UNSETS it for an
  unproxied run precisely so an image or a spec cannot forge it. A chart
  that set it on a Deployment would be the operator speaking in the
  engine's voice, on a host-side process.
- *A pool-scoped name the gateway client honours on a pool pod, with the
  same guard and the same refusal.* The value is the pool's egress-proxy
  Service; a set-but-unusable value is an error naming the variable, never
  a silent direct dial. **Chosen.**

**Where the pool's gateway host comes from.**

A gateway host already exists on main in two places, both per RUN:
`store.RunContentBoundary.GatewayHost` (frozen at launch, replayed by every
resume and fork, read server-side) and the sealed bundle's base URLs, from
which the sandbox derives the host it pins its run proxy to. A registry
field would be the THIRD.

- *A registry `GatewayBaseURL` as a new truth.* Rejected: philosophy 5 —
  views are additive, never a second source of truth.
- *A registry field that is explicitly a STATIC PROJECTION for cluster
  policy.* A NetworkPolicy and a proxy allowlist must exist BEFORE any run,
  so the cluster needs a pool-keyed host that no run supplies; the run's own
  frozen boundary stays the authority, and a disagreement is a refusal at
  the run's boundary, never a re-derivation of either. **Chosen.**
- *Validate it with `secrets.ValidateGatewayBaseURL`.* Rejected: that
  validator refuses every single-label host, every `.svc`/`.cluster.local`/
  `.internal`/`.local` suffix and every private literal, and it has no
  override — it would refuse the in-cluster shape this very boundary is
  designed to allow, and make the registry stricter than the chart (which
  only requires `gateway.baseUrl` non-empty). A pool-scoped shape check
  that WARNS on a private or in-cluster host is what the registry gets.

**`iterion_dlq_depth`.**

- *A second, labelled series, the unlabelled one untouched.* An operator's
  dashboard and alert keep working. **Chosen.**
- *Turn the gauge into a `GaugeVec` labelled by pool.* Rejected: every
  existing unlabelled query breaks silently at the next deploy.

**The broker's ACL.**

- *The pod-side half here (a credential on `queue/nats.Config`, the chart
  mounting the pool's creds Secret), the accounts and subject ACLs as a
  documented operator gesture with the exact subject list.* **Chosen** —
  the production broker is external, so its accounts cannot be proven from
  this repository.
- *Grow the vendored NATS sub-chart with accounts so `task chart:kind` can
  prove a refusal end to end.* Rejected: the dev-stack broker is not the
  production broker, so the proof does not transfer.

## Decision

1. **A pool's boundary lives in the release's namespace and is keyed by the
   pool's label** — the operator's verdict of 2026-11-03. No namespace is
   rendered and no store address changes. Every pod of a pool carries
   `iterion.io/runner-pool: <name>`: its runner pods already do (the pod
   template of `runner-pool-deployment.yaml`), its egress proxy does
   (decision 4), and its sandbox siblings do once the driver stamps it from
   the pool pod's `ITERION_RUNNER_POOL` — today `BuildPodManifest` labels a
   sibling with `iterion.io/managed`, `iterion.io/run-id`,
   `iterion.io/component` and, when the run has a friendly name,
   `iterion.io/run-name`, and with no pool label, so in a namespace both
   share, a pool's siblings are indistinguishable from the shared
   runner's. The objects of
   decisions 2 and 3 select by that label.
2. **A pool has its own ServiceAccount and Role**, in the release's
   namespace, and the pool Deployment names that ServiceAccount where it
   names `iterion.serviceAccountName` today. The pool's Role and
   RoleBinding render under `runner-rbac.yaml`'s own condition,
   `serviceAccount.create` and `runner.sandbox.enabled`: the Role exists to
   serve the sandbox driver, and a pool without the kubernetes sandbox
   needs no API access at all — the shared configmap then renders
   `ITERION_SANDBOX_OVERRIDE: "none"`, so `runSandboxReaper` returns before
   any call, and the pool template renders no `ITERION_POD_IP`. Such a
   pool pod renders `automountServiceAccountToken: false`, as the backup
   CronJob does, and gets no Role; today it mounts a token the chart binds
   to no Role. Where the operator brings the ServiceAccount
   (`serviceAccount.create: false`) the chart renders no RBAC today and
   renders none for a pool either: the pool's binding is the operator's
   gesture. Where the Role renders, it carries what the sandbox driver and
   the orphan GC call — `pods` (get, list, watch, create, delete),
   `pods/exec` (create, get), `pods/log`, `pods/status` and `secrets` (get,
   list, create, patch, delete: the per-run Secrets the driver applies and
   the sweep reaps) — which is `runner-rbac.yaml`'s set less two grants.
   `secrets` `update` goes: the driver writes a Secret only through a
   client-side `kubectl apply` (`runtime.go`), which reads, then creates or
   patches and never replaces, as `runner-rbac.yaml`'s own comment says,
   and its other `kubectl` gestures are `delete`, `wait`, `exec` and `get`.
   The `networkpolicies` rule goes WHOLE. `create` is what widens the pod's own egress directly
   (measurement 1) and `delete` would remove the standing policy itself.
   Dropping the rule removes the direct gesture, not the indirect ones:
   the verbs kept reach the shared ServiceAccount's token and create pods
   no policy selects (Consequences), and the driver cannot work without
   any of them. The first draft of this decision kept `list` for the orphan
   reaper: `reapKinds` in `pkg/sandbox/kubernetes/orphans.go` sweeps pod,
   Secret AND networkpolicy, a kind it cannot list is a returned error, and
   both callers (`pkg/runner/reaper.go`, `pkg/runview/service_lifecycle.go`)
   log it as a warning every pass. That held in a pool namespace, where the
   list came back empty. In the shared namespace `listManagedResources`
   lists by `iterion.io/managed` across the whole namespace, so a pool's
   sweep would list the shared runner's per-run policies too and meet
   `Forbidden` deleting any orphan among them — the permanent warning line
   in the one place an operator watches for the plaintext-credential Secret
   that leaks when the sweep fails. So a pool pod that declares the
   standing boundary (decision 3) sweeps pods and Secrets and skips the
   networkpolicy kind: it creates none, and the shared runner's sweep,
   which keeps both verbs, reaps that kind namespace-wide. The driver's
   teardown delete is gated on the apply having happened
   (`networkPolicyApplied`, driver.go), so it is never attempted either.
   What the Role keeps reaches the whole namespace — a Role is scoped by
   namespace, not by label — and Consequences says what that is.
3. **A standing default-deny NetworkPolicy, both directions, selects the
   pool's pods** — by the label of decision 1, never the whole namespace,
   which the server and the shared runner share — and the road a pool run
   travels is named in it allowance by allowance. The list belongs to the
   decision, because a missing allowance is a time-out, not a refusal,
   which is what `charts/iterion/values-prod.yaml` already warns of the
   release-wide policy. **Egress**: kube-dns; the kube-apiserver the
   sandbox driver's `kubectl` dials for every apply, exec, logs and delete,
   owed only where the pool runs the sandbox (as an endpoint `ipBlock` — a `podSelector` cannot name it where the
   control plane is managed); the stores — Mongo, NATS and S3 wherever the
   install puts them, a `podSelector` where the release bundles them, as
   the release-wide policy already names its dev stores and the NATS
   sub-chart, and operator CIDR or endpoint data where they are managed,
   which is what `values-prod.yaml` ships (`nats.enabled: false`, external
   Mongo and S3); the pool's own runner pods, for the sibling →
   CONNECT-proxy hop; the pool's egress-proxy Service; the PROXY's own
   egress to the pool's gateway host, because the policy selects every pod
   of the pool, the proxy's included, and decision 6 treats that host as
   normally public — so it enters as operator CIDR data, for the same
   reason as the forge; the forge, which git leaves the pod for directly —
   the clone guard is a loopback proxy that re-resolves and dials the repo
   host itself rather than chaining to the pool's proxy — and which
   therefore enters the policy as operator CIDR data, decision 6's host
   NAMES being enforceable only at a proxy; and a per-pool `extraAllow`,
   the twin of the chart's `networkPolicy.egress.extraAllow`, so a pool
   that needs one more peer never has to disable its boundary.
   **Ingress**, the half the per-run policy also carried: the sandbox
   siblings — selectable by the pool label of decision 1 — accept the
   pool's runner pods and nothing else, the standing twin of the per-run
   `/32`; the proxy accepts the pool's runner pods only, which closes back
   the reach the egress half hands the siblings; the runner pods accept
   the pool's own pods, for that same CONNECT-proxy hop; the runner pods'
   metrics port, which Prometheus scrapes from a namespace of the
   operator's — its own rule, unscoped exactly as the release policy keeps
   the metrics rule it already ships, or scoped by a `namespaceSelector`
   list in the idiom of `networkPolicy.ingress.controllerNamespaces`; and a
   per-pool ingress `extraAllow`, the twin of the chart's
   `networkPolicy.ingress.extraAllow`, as the egress half has — a pool
   whose boundary lacks one inbound peer must not have to disable it. The
   kubelet's probes are host traffic no policy sees, as the chart's
   dev-store policy already records.
   **No other policy the chart ships selects a pool pod.** The release-wide
   policy selects `iterion.selectorLabels` minus the dev stores, which
   takes in every pool pod today; NetworkPolicy is a union, so with both
   rendered its egress `extraAllow` and its ingress rules would widen
   every pool. Under an enforced boundary its `podSelector` excludes the
   pool's components, as it already excludes the dev stores.
   The pool pod declares that the standing boundary exists, so the sandbox
   driver's fail-closed refusal keeps biting everywhere else.
   **The standing policy and the proxy's objects carry no
   `iterion.io/managed` label.** The orphan sweep lists by that label
   across the namespace (`listManagedResources`) and treats a managed
   object with no `iterion.io/run-id` as an orphan (`ReapOrphanResources`,
   and `sandboxResourceReapable` in `pkg/runner/reaper.go`: no run owner,
   reap). The shared runner's sweep would delete the standing policy,
   leaving the pool's pods selected by no policy — the residual's
   strongest gesture, reached by accident — and a pool's own sweep would
   delete its proxy pod.
4. **The egress proxy is the Nth variant of an existing seam, not a new
   component family.** `pkg/sandbox/netproxy` already enforces a host
   policy on every CONNECT, authenticates with `Proxy-Authorization:
   Bearer` and hands refusals to a hook; `pkg/runner/gitproxy.go`'s clone
   guard is variant 1 (a long-lived netproxy with a one-host allowlist and
   connect-time SSRF re-resolution). The pool's proxy is variant 2, with
   one difference that is the whole point: it runs OUT of the pod it
   bounds — its own Deployment, in the release's namespace and carrying its
   pool's label, which is why the standing policy above selects it like any
   other pod of the pool. An in-pod loopback proxy is bypassable by the
   process it is meant to bound. The proxy calls no API, so it mounts no
   ServiceAccount token (`automountServiceAccountToken: false`, as the
   backup CronJob already renders): in a namespace where a shared identity
   may author policies, a pod that needs no token holds none.
5. **The denied-connection proof is produced by the proxy's refusal hook.**
   netproxy logs nothing of its own — it calls `OnBlocked(host, reason)`,
   and the per-run variant turns that into a typed run event
   (`store.EventNetworkBlocked`) plus a warning. A pool proxy serves many
   runs and holds no run context, so its hook writes a log line naming the
   pool, the host and the reason, and increments a counter. That line is
   the artefact the ticket calls the denied-connection proof.
6. **The pool's gateway host and forge hosts are registry data**, a static
   projection for cluster policy as decided above, validated by a
   pool-scoped shape check that warns (a C1xx warning) on a private or
   in-cluster host and never refuses it.
7. **The broker credential is configuration, not a branch**: a credential
   field on `queue/nats.Config` (creds file, user/password or nkey), the
   pool Deployment mounting its own Secret, absent credentials keeping
   today's connect exactly as it is. The broker's accounts and subject
   ACLs are the operator's gesture, documented with the subject list a
   pool needs. The shared namespace changes who can reach that Secret, not
   the decision: like the pool's gateway Secret, it is readable by every
   Role of the namespace that grants `secrets` `get`, and replaceable or
   deletable by every one that grants `update`, `patch` or `delete`,
   another pool's included (Consequences).
8. **`iterion_dlq_depth_by_pool{pool=…}` joins the unlabelled gauge**, fed
   from the sweeper's existing registry walk, keeping its fail-closed
   posture (an unreadable leg holds the last value; no zero is written).
9. **`boundary.enforced: false` renders exactly today's shape and warns
   loudly** — the escape hatch philosophy 1 requires, in the idiom the
   chart already uses for the ingress warning (an annotation on the
   object, a WARNING comment in the manifest, a note at install).

## Consequences

- **The companion pages become stale the day the chart slice lands, not
  before.** [sovereign-pools.md](../sovereign-pools.md) opens with "there is
  no dedicated namespace and no egress filtering",
  [cloud-deployment.md](../cloud-deployment.md) repeats it under *Sovereign
  runner pools*, and adds that the pool pods use the release-wide policy,
  which decision 3 ends. All of it is TRUE today; the chart slice owns their
  rewrite — the egress half changes, "no dedicated namespace" stays true by
  the verdict — and this record is what they will point at.
- **The residual the verdict accepts: the authoring verbs stay in the
  namespace, one step from the pool's own token — wherever the release
  provisions the sandbox.** `runner-rbac.yaml` renders under
  `serviceAccount.create` and `runner.sandbox.enabled`; the chart default
  leaves the sandbox off and `values-prod.yaml` does not turn it on, and
  there no Role renders, so no identity the release renders may author a
  policy and what remains is the operator's own bindings. Where the
  sandbox is on, exactly one identity the chart renders may author a
  NetworkPolicy: the shared ServiceAccount
  (`iterion.serviceAccountName`), bound by `runner-rbac.yaml`'s Role to
  `networkpolicies` `get/list/create/delete` — no `patch`, no `update`, no
  `resourceNames`, so the rule reaches every policy of the namespace. Its
  token is mounted by the server Deployment, the shared runner Deployment
  and, until decision 2 lands, every pool Deployment; the backup CronJob
  names it with `automountServiceAccountToken: false`, the sandbox siblings
  render `automountServiceAccountToken: false` (`BuildPodManifest`), and
  the dev stores mount none. After decision 2 the server and the shared
  runner keep it, because the shared runner's per-run policies need it.
  With `create`, either may add a policy selecting a pool's pods, which
  the CNI unions into the pool's allowance; with `delete`, either may
  remove the pool's standing policy outright — the stronger of the two,
  since the pool's pods are then selected by no policy at all and their
  egress is open. The shared runner executes every non-sovereign team's
  runs, and where a run executes without a sandbox
  (`ITERION_SANDBOX_OVERRIDE=none`) the runner process is the isolation
  boundary: a compromise of the shared tier is one API call from a
  sovereign pool's network boundary. Beyond the chart, any identity the
  operator binds in that namespace counts too.
  The token of a pool that runs the sandbox reaches the same verbs through
  what decision 2 keeps, because a Role cannot scope those verbs by label
  and decision 3 lets the pool's pods reach the kube-apiserver; a pool
  without the sandbox mounts no token (decision 2), and none of these
  paths exists for it:
  - `pods/exec` into the server or the shared runner pod — both run
    `iterion.image`, a Debian image with a shell and `kubectl` — acts with
    the shared ServiceAccount token they mount;
  - `pods` `create` may name any ServiceAccount of the namespace (the
    Kubernetes RBAC good-practice guide: the right to create a pod is
    implicitly the API access of every ServiceAccount there), so a pod
    the pool creates under the shared one, then execs into, holds its
    `networkpolicies` `create` and `delete`; `secrets` `create` and `get`
    reach the same token through a `kubernetes.io/service-account-token`
    Secret annotated with the shared ServiceAccount's name;
  - a pod the pool creates without the release's selector labels is
    selected by no policy the chart ships, so its egress is open, and one
    created under the shared runner's labels takes the release-wide
    policy's allowances.
  So under B a sandbox pool's token can no longer author a policy, and can still
  widen its own egress by creating a workload or exec'ing into one. The
  cost as stated when the operator chose B named the other identities
  only; this is the measured one. What still holds after any of these
  paths is the credential fact of the Context for iterion's own calls: the
  pool pod's env holds no vendor key, so a mis-routed call fails at
  authentication, as it does today. A process driving the pool's token is
  not bounded by it: it reads the platform's vendor keys
  (`secret-llm.yaml`) as it reads any Secret of the namespace (below).
- **What the pool's own Role still reaches, namespace-wide.** A Role is
  scoped by namespace, not by label, and the namespace is the release's.
  Where it renders (a pool that runs the sandbox, decision 2), the pool's
  `secrets` verbs — get, list, create, patch and delete: `kubectl apply`
  reads the named per-run Secret before it creates or patches it, the
  sweep lists by label and deletes — reach every Secret there, both ways. Read: `secret-auth.yaml`'s, which carries
  `ITERION_SECRETS_KEY`; the platform's vendor keys (`secret-llm.yaml`'s,
  or the operator's `secrets.llm.existingSecret`);
  another pool's gateway and broker Secrets; the shared runner's per-run
  Secrets. Write: it can overwrite or delete any of them — the platform
  key the server and the shared runner mount, another pool's gateway or
  broker credential, a substitution and not only a read. Its `pods/exec`
  `create` reaches every pod there, the server's included, and its `pods`
  `delete` every pod there — the server, the shared runner, other pools'
  runner pods and proxies, and every sandbox sibling (the sweep is built
  to reach siblings: a healthy runner reaps a dead one's orphans) — so a
  denial of the shared tier is one call away. The reverse holds too: the
  shared identity's `pods/exec` reaches a pool's pods, and the gateway
  credential in their env. `runner-pool-deployment.yaml` already records
  the Secrets half for today ("can read the namespace's Secrets for the
  sandbox reaper"); a pool namespace would have closed it, and the verdict
  keeps it. It is why owed item 4 pins the pod's ENV, not what its token
  can read.
- **What narrows the residual now, and what only names a later
  hardening.** In reach, in the chart slice: a render test pinning the pool
  Role's rules to exactly the set of decision 2 — no `networkpolicies`
  rule, no `secrets` `update` — and the pool Deployment's ServiceAccount
  to the pool's own; the same test with the sandbox off, where no pool
  Role renders and the pool pod mounts no token; a render test that no
  NetworkPolicy the chart ships, the pool's own excepted, selects a pool
  pod; and one that the standing policy and the proxy's objects carry no
  `iterion.io/managed` label (decision 3). Those pin what this repository renders and
  say nothing of what an identity of the namespace — the pool's own token
  included — authors, creates or execs at run time.
  Named as a later hardening, not promised by this record: admission (a
  ValidatingAdmissionPolicy) over every gesture the residual counts — a
  NetworkPolicy that selects a pool's pods, or the delete of a pool's
  standing policy, from any identity but the deploy's; a pod the pool's
  ServiceAccount creates without the pool label or under another
  ServiceAccount; a `kubernetes.io/service-account-token` Secret it
  creates; and its `pods/exec` into a pod that is not its own sibling,
  where the request names the pod but does not carry its labels, so
  whether a policy can key it is for that hardening to prove. Not decided
  here either: a namespace-wide catch-all default-deny. Where the
  release-wide policy is on it closes the unselected-pod path and cuts the
  NATS sub-chart's pods unless the chart grows them a policy; where it is
  off it cuts every pod of the release; and it leaves the exec and
  ServiceAccount paths, and a pod created under the shared runner's
  labels, untouched. Should an audit require *no* to "can any identity of
  this namespace, the pool's own included, widen this team's egress?",
  options A and C are the structural answers.
- **Reaching the stores keeps its addresses and becomes explicit.** The
  pool shares the release's namespace, so a store reached today by a short
  name keeps it. Bundled by the release, Mongo, NATS and S3 are pods of
  that namespace, which the policy names by selector. Managed — what
  `values-prod.yaml` ships, on the `ovh-prod` target this record names
  below — they sit outside the cluster and the policy can only carry the
  operator's CIDR or endpoint data. Either way all three, plus the
  kube-apiserver the sandbox driver shells `kubectl` at, must be in the
  list: a store it forgets is the time-out mode above, the pool pod
  holding its `ITERION_MONGO_URI` and never reaching the row.
- **The release PodMonitor keeps the pools, and the policy must let it
  in.** `charts/iterion/templates/prometheus-podmonitor.yaml` carries no
  `namespaceSelector`, so prometheus-operator discovers pods in the
  PodMonitor's own namespace, which a pool pod shares: it carries
  `iterion.selectorLabels`, which is the PodMonitor's whole selector, and
  names its port `metrics` — and `values-prod.yaml` ships
  `metrics.podMonitor.enabled: true`, so the day a pool is activated there
  it is scraped. No PodMonitor change is owed; decision 3's metrics
  ingress rule is. Forgotten, it is the time-out mode in its quietest
  form: the pool's series stop, its target reads down, and nothing in
  iterion errors.
- **A standing rule is coarser than the per-run rule it replaces, in both
  directions.** Outbound, the per-run policy pinned a sibling to its own
  runner pod by `/32`; a pool-wide rule can only say "the pool's runner
  pods", and the pool's egress-proxy Service becomes addressable from the
  siblings too — which the standing inbound rule closes back, the proxy
  accepting the runner pods alone. Inbound, a sibling accepted its own
  runner pod's `/32` and now accepts any runner pod of the pool. What
  separates two runs at that layer is then netproxy's token, fresh per run
  — so the pool proxy MUST require its own, or a sibling crosses the pool
  boundary without its run's `network:` allowlist. Where the authoring
  verbs are still granted — the shared runner, and a pool rendered with
  `boundary.enforced: false` — the per-run policy stays the finer
  instrument in both directions and is still synthesised.
- **Git stays outside the by-name rule until the clone guard chains.**
  The forge is an IP-layer allowance because the clone guard dials the
  repo host itself; teaching it to chain to the pool's proxy would put
  git under the same by-name allowlist as the LLM egress. A follow-up,
  not this record.
- **The proxy becomes a hard dependency of every pool run.** D27 accepted
  that; it also means the pool's readiness is now two objects (the runner
  Deployment and the proxy Deployment), and a pool whose proxy is down
  fails its runs rather than leaking them — which is the posture asked for.
- **`httpdial`'s `Proxy: nil` pins stay.** Nothing in this decision asks
  `SafeTransport` or `PinnedTransport` to honour an ambient proxy; the pool
  pod's LLM egress crosses a proxy by explicit wiring (the shape
  `pkg/backend/compatgw` already uses), and everything else is bounded by
  the standing policy — a direct dial simply fails.
- **A gate prerequisite nobody had written down**: `go test
  ./charts/iterion/` cannot run in a fresh worktree, because
  `charts/iterion/charts/` and `Chart.lock` are gitignored — every render
  subtest fails with "missing in charts/ directory: nats". Run `helm
  dependency build charts/iterion` first. This is the chart analogue of the
  `pnpm -C studio install` rule.
- **The pool's queue topology has no witness in the free deterministic
  layer**: `TestPoolConnectBringsUpPoolTopology` SKIPS unless
  `ITERION_TEST_NATS_URI` is set, so it only runs in the nats-conformance
  CI job. A change to the pool's `Connect` path is unwitnessed locally.
- **What stays the operator's gesture**, outside this repository: the
  pool's gateway Secret, the broker's accounts and subject ACLs, every
  binding added in the release's namespace beyond the chart's (each one
  counts in the residual above) — a pool's own binding included, where
  the operator brings the ServiceAccount — and the activation of a pool on
  `ovh-prod` with its denied-connection log captured as the receipt.

## State of play — what ships today, by witness

- **P1, the queue**: pool on the message and per-pool topology
  (`queue/nats.Config.Pool`, the pool stream/DLQ/consumer). Witness:
  `TestPoolConnectBringsUpPoolTopology` — which SKIPS without a broker, so
  the deterministic evidence is the admission and publication tests, not
  this one.
- **P2, the mapping, ~90%**: every team write is a field patch
  (`identity.TeamPatch`, `PatchTeam` in both store twins), the super-admin
  route is the single writer, and one-team-per-pool is enforced at the
  route and backstopped at the store. Witnesses:
  `TestUpdateTeamHasNoProductionCallers` (a whole-document replace is
  uncommittable), `TestOrgTeamCaps_PatchPreservesTheRunnerPool`,
  `TestPatchTeam_RunnerPoolHeldByAnotherTeam`, and the patch conformance
  suite over both twins.
- **P3, the server-side surfaces**: the merge-conflict resolver, declared
  supervisors and the session board refuse a pool run's content
  (`runview.ErrPoolContentRefused`, 403 on the route). Witnesses:
  `TestPoolContentRefusalTable`, `TestConflictAgentRefusesPoolRunContent`,
  `TestStartDeclaredSupervisorsRefusePoolRunContent`,
  `TestStartSessionBoardRefusesPoolRunContent`, and
  `TestPoolGuardCallSitesArePinned` for the class.
- **P4, the credentials**: a pool team's bundle consults no shared tier, and
  the run bundle seals under a per-run DEK that travels in the message
  ([ADR-123](123-run-bundle-dek-in-envelope.md)), so the pod holds no
  platform key material. Witness: the sovereign fallback posture tests
  (`TestAdminSetTeamLLMFallback` and the publisher's unfunded refusals).
- **P5-a, the isolation audit**, and the identity invariant: the frozen
  stamp travels to forks and subbot children, and the run lease carries the
  admitted (tenant, pool). Witnesses:
  `TestFork_TheFrozenPoolStampTravelsToTheChild`,
  `TestSubbotRunnerTheChildDocumentCarriesThePool`, and the lease-identity
  tests of `pkg/queue/nats`.
- **P8, the chart**: `runnerPools[]` renders a Deployment per pool with its
  name grammar, its gateway required, its own stream/DLQ, two auth keys by
  specific `secretKeyRef`, its env-pin union and a PDB. Witness: the pool
  subtests of `TestChartRenderScheduling` over
  `charts/iterion/testdata/values-pools.yaml`.
- **The DLQ surface**: the four admin routes are super-admin and pool-aware,
  and the depth gauge holds its value on an unreadable leg. Witnesses:
  `TestDLQAdmin_ThePoolReachesTheBackend`,
  `TestDLQGaugeValue_HoldsOnUnreadableLegs`.

## What is owed, in order, each with its witness

1. **P2's last item, the cutover of in-flight runs.** The mapping route
   consults no run store, and an already-queued run keeps executing on a
   shared pod after the operator maps the team. The protocol is the
   lifecycle that already exists — set the old pool `draining` (no new
   routing, the topology lives until in-flight runs end), let the alive runs
   end, then map — and the route gains the BACKSTOP for an operator who
   skips it: a refusal naming the count, the run ids and `draining` as the
   remedy, with `?force=1` as the audited escape hatch. Witness:
   `TestAdminSetTeamRunnerPool_RefusesWhileRunsAreAlive`.
2. **The admin CLI.** `iterion remote admin runner-pools list|set`,
   `teams runner-pool`, `teams llm-fallback`, and `--pool` added to the
   `dlq` verbs that already exist (`show|replay|delete`, in the shared
   `remote_admin.go`; the server side is already pool-aware). Witness: the
   route-reachability gate, which derives its union and needs no new row.
3. **`iterion_dlq_depth_by_pool`.** Witness:
   `TestDLQDepthCarriesThePoolLabel` — the new assertion is the label; the
   fail-closed leg is already pinned.
4. **The unwitnessed invariant: a pool pod's env holds no key material.** A
   chart render test asserting no `ITERION_SECRETS_KEY*` in env or
   `envFrom`, exactly two auth keys by `secretKeyRef`, and the env-pin
   union. The invariant is the env's; what the pool's token can read in the
   shared namespace is the residual recorded under Consequences. Witness:
   `TestChartRenderRunnerPoolCarriesNoPlatformKeyMaterial`, after `helm
   dependency build`.
5. **Decision 6's registry projection.** `platformcfg.RunnerPool` gains the
   pool's gateway base URL and its forge hosts, written by the super-admin
   registry route and validated by the pool-scoped shape check decided
   above — which warns on a private or in-cluster host and never refuses
   it. It waits on no verdict (registry data, not chart shape), and item 6
   has no pool-keyed host to enforce without it. Witnesses:
   `TestAdminPutRunnerPools_ValidatesTheGatewayHost` (an unparseable host
   is a 422 naming the rule; an in-cluster host is accepted WITH the
   warning; re-validation is idempotent) and
   `TestPoolBootWarnsWhenItsGatewayDisagreesWithTheRegistry` — the pod is
   the only place both facts are reachable, its own gateway variable and
   the registry entry for its pool: disagreement warns and boot continues,
   agreement is silent, an unreadable registry warns naming the read
   error and never passes silently.
6. **Then, the SHAPE slices the operator's verdict unblocked**: the pool's
   label, RBAC and standing policy (witness: the pool Deployment names the
   pool's own ServiceAccount; the pool Role's rules are exactly decision
   2's and carry no `networkpolicies` rule and no `secrets` `update`; with
   `runner.sandbox.enabled` off no pool Role renders and the pool pod
   mounts no token; the standing policy selects the pool label, renders
   both policyTypes with exactly its allowed peers and carries no
   `iterion.io/managed` label;
   no other rendered NetworkPolicy selects a pool pod; the driver stamps
   the pool label on a pool pod's siblings and, under the declaration,
   skips both the per-run policy and the networkpolicy sweep;
   `boundary.enforced: false` renders the legacy shape and warns), the
   egress proxy (witness: a host outside the policy is refused and the
   refusal is logged with the host and the pool; the proxy pod mounts no
   ServiceAccount token and its objects carry no `iterion.io/managed`
   label), and the broker credential (witness: the
   credential reaches the client options; absent, today's connect is
   unchanged).

Item 5, the registry's gateway-host projection, is the highest-leverage small
slice of the list: it is also the input D27's by-name policy needs, and it
closes a blocker recorded outside this ticket.
