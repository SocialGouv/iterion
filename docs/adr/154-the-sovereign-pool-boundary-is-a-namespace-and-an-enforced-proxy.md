# ADR-154: The sovereign pool boundary is a namespace and an enforced proxy

- **Status**: Proposed (2026-11-03) — D27 (the operator) decided the mechanism, an in-cluster enforced egress proxy per pool. The SHAPE below is this wave's default answer to fleet question #88, which the operator has not answered; the chart, proxy and broker slices wait on that verdict and nothing in them starts before it. The record becomes Accepted in the commit that lands the first SHAPE slice (decisions 1-4), which is after that verdict: items 1 to 4 of the owed list implement none of the decisions below and land under Proposed.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude Opus 5), for [#2029](https://github.com/SocialGouv/iterion/issues/2029) P5
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
   runner".
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
owed is a test pinning that invariant, not a key scheme. **The storage
residual** stays as [sovereign-pools.md](../sovereign-pools.md) already
accepts it: the pool pod takes the deployment-wide storage secret, and what
the code guarantees is tenant mediation on every blob read.

## Options considered

**What makes the boundary immutable to the runner.**

- *A — a NAMESPACE per pool, rendered by the same release.* The runner's
  Role is namespace-bound (`kind: Role`, bound to `.Release.Namespace`), so
  a pool's own Role simply does not carry `networkpolicies` `create` or
  `delete`, and the standing policy in that namespace cannot be weakened
  by anything running in it. The kubernetes sandbox driver needs no change
  to follow: it reads its namespace from its own service-account mount
  (`pkg/sandbox/kubernetes.Detect`), so a pool pod places its sandbox
  siblings in the pool's namespace by construction. Cost: a second
  SA/Role/RoleBinding set, a namespace template, a release that now writes
  outside its own namespace, and reaching the stores from a second one —
  cross-namespace Service DNS where a sub-chart bundles them, the same
  external endpoints as today where they are managed. **Chosen** — it is
  the only shape that answers an operator audit asking "can the pod that
  holds this team's code widen its own egress?" with *no*, on paper and in
  the cluster.
- *B — keep the shared namespace, drop the authoring verbs from a
  pool-scoped Role.* Cheap: no DNS change, no namespace. Rejected: any
  OTHER pod of the namespace may still author an additive policy the CNI
  unions in, so the boundary stays weakenable from inside — it loses
  exactly the audit above.
- *C — a Helm release per pool.* Strongest isolation, highest operating
  cost (a release, a values file and an ArgoCD application per team).
  Rejected for now; option A keeps one release and reaches the same blast
  radius.

**The per-run NetworkPolicy the sandbox driver synthesises.**

The driver locks each sandbox sibling's egress to its runner pod's CONNECT
proxy plus kube-dns, and its inbound to that same pod IP; when it cannot
(no pod IP) it REFUSES to start the run rather than downgrade isolation in
silence. A pool pod does NOT meet that refusal: the chart wires
`ITERION_POD_IP` for a pool Deployment too, so the pod has the env and
loses the authoring verbs instead — it fails one step later, as
`kubernetes: apply netpolicy: … forbidden` at the manifest apply. Same
direction, different error, and a remedy an operator reads as RBAC where
the PodIP message names the Helm downward API. Which is why the shape below
has the pool DECLARE the standing boundary rather than leave the driver to
interpret a Forbidden.

- *Keep the authoring verbs on a pool.* Rejected by the ticket's own
  promise.
- *A standing namespace-wide default-deny policy, and a pool pod that
  DECLARES it.* The standing policy covers the pool's pods and their
  sandbox siblings (same namespace), so it must itself carry what the
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
  delta recorded under Consequences: in both directions a namespace-wide
  rule says "the pool's runner pods" where the per-run rule pinned one
  `/32`.
- *A ValidatingAdmissionPolicy refusing NetworkPolicy from the pool's SA.*
  Same effect, one more cluster-scoped object the operator must install and
  keep. Rejected as the primary mechanism; it remains available as
  defence in depth.

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

1. **A pool is a namespace.** `runnerPools[].namespace` is required when the
   boundary is enforced, and the release renders it.
2. **A pool has its own ServiceAccount and Role**, carrying what the sandbox
   driver and the orphan GC need (pods, exec, logs, status, the per-run
   Secrets) and, on `networkpolicies`, `list` alone — never `create`,
   never `delete`. `create` is what widens the pod's own egress
   (measurement 1) and `delete` would remove the standing policy itself,
   while `list` is the orphan reaper's, exactly as `runner-rbac.yaml` and
   [sandbox.md](../sandbox.md) already say of the Secrets: `reapKinds` in
   `pkg/sandbox/kubernetes/orphans.go` sweeps pod, Secret AND
   networkpolicy, a kind it cannot list is a returned error, and both
   callers (`pkg/runner/reaper.go`, `pkg/runview/service_lifecycle.go`)
   log it as a warning every pass — a permanent `Forbidden` line in the one
   place an operator watches for the plaintext-credential Secret that
   leaks when the sweep fails. Nothing else needs a verb there: a pool
   namespace holds no per-run policy (decision 3), so the sweep's list
   comes back empty, and the driver's teardown delete is gated on the
   apply having happened (`networkPolicyApplied`, driver.go), so it is
   never attempted either.
3. **A standing default-deny NetworkPolicy, both directions**, lives in the
   pool's namespace, and the road a pool run travels is named in it
   allowance by allowance — the list belongs to the decision, because a
   missing allowance is a time-out, not a refusal, which is what
   `charts/iterion/values-prod.yaml` already warns of the release-wide
   policy. **Egress**: kube-dns; the kube-apiserver the sandbox driver's
   `kubectl` dials for every apply, exec, logs and delete (as an endpoint
   `ipBlock` — a `podSelector` cannot name it where the control plane is
   managed); the stores — Mongo, NATS and S3 wherever the install puts
   them, a `namespaceSelector` where a sub-chart bundles them and operator
   CIDR or endpoint data where they are managed, which is what
   `values-prod.yaml` ships (`nats.enabled: false`, external Mongo and
   S3); the pool's own runner pods, intra-namespace, for the sibling →
   CONNECT-proxy hop; the pool's egress-proxy Service; the PROXY's own
   egress to the pool's gateway host, because a namespace-wide rule
   selects every pod of the namespace, the proxy's included, and decision
   6 treats that host as normally public — so it enters as operator CIDR
   data, for the same reason as the forge; the forge, which git leaves the
   pod for directly — the clone guard is a loopback proxy that re-resolves
   and dials the repo host itself rather than chaining to the pool's
   proxy — and which therefore enters the policy as operator CIDR data,
   decision 6's host NAMES being enforceable only at a proxy; and a
   per-pool `extraAllow`, the twin of the chart's
   `networkPolicy.egress.extraAllow`, so a pool that needs one more peer
   never has to disable its boundary. **Ingress**, the half the per-run
   policy also carried: the sandbox siblings — selectable by the
   `iterion.io/managed` label the driver stamps on every one of them —
   accept the pool's runner pods and nothing else, the standing twin of
   the per-run `/32`; the proxy accepts the pool's runner pods only, which
   closes back the reach the egress half hands the siblings; the runner
   pods accept the pool's own pods, for that same CONNECT-proxy hop. The
   kubelet's probes are host traffic no policy sees, as the chart's
   dev-store policy already records.
   The pool pod declares that the standing boundary exists, so the sandbox
   driver's fail-closed refusal keeps biting everywhere else.
4. **The egress proxy is the Nth variant of an existing seam, not a new
   component family.** `pkg/sandbox/netproxy` already enforces a host
   policy on every CONNECT, authenticates with `Proxy-Authorization:
   Bearer` and hands refusals to a hook; `pkg/runner/gitproxy.go`'s clone
   guard is variant 1 (a long-lived netproxy with a one-host allowlist and
   connect-time SSRF re-resolution). The pool's proxy is variant 2, with
   one difference that is the whole point: it runs OUT of the pod it
   bounds — its own Deployment, in the pool's namespace, which is why the
   standing policy above selects it like any other pod there. An in-pod
   loopback proxy is bypassable by the process it is meant to bound.
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
   pool needs.
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
  no dedicated namespace and no egress filtering" and
  [cloud-deployment.md](../cloud-deployment.md) repeats it under *Sovereign
  runner pools*. Both are TRUE today; the chart slice owns their rewrite,
  and this record is what they will point at.
- **Reaching the stores becomes explicit, and their address is the
  install's.** Bundled by a sub-chart, Mongo, NATS and S3 are
  cross-namespace Services: a store reached today by a short name needs
  its fully qualified form, and the policy names them by selector.
  Managed — what `values-prod.yaml` ships, on the `ovh-prod` target this
  record names below — they sit outside the cluster and the policy can
  only carry the operator's CIDR or endpoint data. Either way all three,
  plus the kube-apiserver the sandbox driver shells `kubectl` at, must be
  in the list: a store it forgets is the time-out mode above, the pool pod
  holding its `ITERION_MONGO_URI` and never reaching the row.
- **A standing rule is coarser than the per-run rule it replaces, in both
  directions.** Outbound, the per-run policy pinned a sibling to its own
  runner pod by `/32`; a namespace-wide rule can only say "the pool's
  runner pods", and the pool's egress-proxy Service becomes addressable
  from the siblings too — which the standing inbound rule closes back, the
  proxy accepting the runner pods alone. Inbound, a sibling accepted its
  own runner pod's `/32` and now accepts any runner pod of the pool. What
  separates two runs at that layer is then netproxy's token, fresh per run
  — so the pool proxy MUST require its own, or a sibling crosses the pool
  boundary without its run's `network:` allowlist. Where the authoring
  verbs are still granted (a shared-namespace install), the per-run policy
  stays the finer instrument in both directions and is still
  synthesised.
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
  namespace's existence in GitOps if ArgoCD manages namespaces, the pool's
  gateway Secret, the broker's accounts and subject ACLs, and the
  activation of a pool on `ovh-prod` with its denied-connection log
  captured as the receipt.

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
4. **The unwitnessed invariant: a pool pod holds no key material.** A chart
   render test asserting no `ITERION_SECRETS_KEY*` in env or `envFrom`,
   exactly two auth keys by `secretKeyRef`, and the env-pin union. Witness:
   `TestChartRenderRunnerPoolCarriesNoPlatformKeyMaterial`, after `helm
   dependency build`.
5. **Then, after the operator's verdict**: the namespace + RBAC + standing
   policy render (witness: the pool Role carries `networkpolicies` `list`
   and neither `create` nor `delete`, the policy renders both
   policyTypes with exactly its allowed peers, `boundary.enforced: false`
   renders the legacy shape and warns), the egress proxy (witness: a host
   outside the policy is refused and the refusal is logged with the host
   and the pool),
   and the broker credential (witness: the credential reaches the client
   options; absent, today's connect is unchanged).

The registry's gateway-host projection (decision 6) is the highest-leverage
small slice of the list: it is also the input D27's by-name policy needs, and
it closes a blocker recorded outside this ticket.
