# The posture bench, against a cluster this host does not own

The `kind` arm in [deployment-posture-bench.md](deployment-posture-bench.md)
proves the SCRIPT. It cannot answer for the deployment's own runtime: a
`RuntimeDefault` seccomp profile is a property of a **containerd build**, and
the production node ships its own. Re-running that arm there is impossible as
written — it hands the bench binary to the node through `extraMounts` and the
pod reads it from a `hostPath`, and a production node accepts neither.

Two members carry the arm across the boundary.

## `task bench:codemode-bench-image`

Builds the deployment's own image, pinned by digest, plus `/bench/inpodbench`
— and nothing else. It prints `BENCH_IMAGE=<repo@sha256:…>` on stdout.

- **A tag is refused.** A tag lets the image move under the verdict; every arm
  of this bench pins.
- **The base is refused unpinned**, for the same reason: the posture measured
  has to be the posture that runs.
- **`LOAD_KIND=<cluster>`** is the witness lane: it loads into an existing kind
  cluster and prints a TAG, which the driver accepts only with
  `--pull-policy Never` (nothing pulls, so nothing can move). It proves the
  DRIVER, never a production node.

## `task bench:codemode-remote`

Applies the pods with `kubectl` against a kubeconfig passed **by reference**
(this script never reads it), under a unique prefix, and removes exactly the
pods it created. `default`, `kube-system`, `kube-public` and `kube-node-lease`
are refused outright: this arm creates pods.

It varies **one** thing. The measured pod and its `Unconfined` twin share uid,
capabilities, `no_new_privs` and resources, so `Seccomp: 2` against
`Seccomp: 0` attributes to the profile alone — the twin keeps `no_new_privs` so
only seccomp differs, which also settles that `NoNewPrivs: 1` comes from
`allowPrivilegeEscalation: false`.

It reports the `containerRuntimeVersion` of the node the **measured pod
landed on**, not the fleet's: nodes differ after a rolling upgrade, and a
version read elsewhere would attribute the verdict to a build that never ran
it. If the admitted pods land on nodes reporting different runtimes the run
fails — two runtimes attribute nothing.

Exit codes are the bench's contract, borrowed from the other arms because a
caller reading "non-zero" as "the bound bit" accepts a broken run as proof:

| code | meaning |
|---|---|
| 0 | the arm held |
| 1 | a red verdict |
| 2 | nothing was tested (setup, admission, RBAC) |

The bench's own `2` (it refused its arguments), `4` ("this pod is not the
posture asked about"), `6` (a leg could not run here) and `7` (the pod never
checked its posture) all map to **2 — nothing was tested**, never to a green and
never to a red about the cluster. So does every step the script does not handle
by name: one `ERR` trap, as in the sibling arms, because under `set -e` a naked
failure exits with the failing command's own status — and that was 1.

**A namespace-scoped credential** — the usual production one — runs the bench
but cannot `get nodes`, so it ends on **2** naming the refusal: this arm exists
to report the node's runtime, and a credential that cannot read it gets no
verdict. It cannot `get` its own Namespace object either; that read answers
*Forbidden*, which the script tells apart from *NotFound* and goes past.

`EXPECT_GROUPS=27,100,1000` (numeric ids — a name is refused before any cluster
is touched) declares the image's supplementary groups through the task, and
`KEEP=1` keeps the pods **and** a namespace the run created.

## Measured on production (2026-11-18)

The image built from `iterion-runner-devbox@sha256:3d269f8d…` (the digest the
production deployment runs), one pod on `node3`, namespace created for the run
and deleted after it:

| | production | `kind` arm |
|---|---|---|
| containerd | **2.3.6** | 2.1.1 |
| kernel | 6.1.0-54 | 6.8.0-149 |
| **Landlock ABI** | **2** | 4 |
| legs | 7/7 held | 7/7 held |
| a compromised child's `connect`, before #3371 | **SUCCEEDED** | refused `EACCES` |
| the same with #3371's `Confine` (leg 5, 2026-11-18, `node3`) | refused `EACCES`, and UDP + `io_uring` too | refused |

**The one that matters is the ABI.** Landlock gained TCP rights in ABI 4, so on
this kernel the domain cannot refuse a socket connection: leg 5 reported the
attempt as **NOT covered** — the gap #3371 closes with a seccomp filter — and what the other legs prove — open and exec
refused, memory and CPU bombs killed, siblings surviving — is unchanged. A
kind-hosted bench on a newer kernel would have reported a confinement the
production node does not have on that axis. That is the class of answer this arm
exists for, and it is a property of the NODE KERNEL, not of codemode.

The runner image also gives its process two supplementary groups the chart does
not name — `sudo` and `users`, because the image's `devbox` user is in them.
`-expect-groups` exists for exactly this: the runtime adds the image's groups,
so the operator declares the deployment's real set and the bench requires an
**exact match in both directions** (an undeclared observed group refuses, and so
does a declared group that is absent). Refusing them outright would make the
runner image unmeasurable in a cluster while the same binary measures clean
under docker, where the groups are the single one passed to `--user`.

## Witnessed, not assumed

Against a `kind` cluster driven purely through `kubectl`:

| what | result |
|---|---|
| measured pod + readback + twin, namespace created with the privileged label | 7/7, exit 0, twin admitted, `containerd://2.1.1` attributed |
| same arm in a namespace whose PodSecurity is `restricted` | twin refused by admission → a `GAP` line, verdict green and *declared* |
| measured pod switched to `Unconfined` (mutation) | the bench refuses its own posture, exit 4 → *nothing was tested* |
| **readback** switched to `Unconfined` beside a confined bench (mutation) | the `Seccomp: 2` assertion reddens, exit 1 |
| a base image whose `iterion` predates `__script-host` | exit 1, two named failures — a green it cannot fake |
| a namespace-scoped credential that may read its namespace but not nodes | bench 7/7, then **exit 2** naming the RBAC refusal — the previous version exited **1, silently**, after the bench had held |
| the same credential without even the namespace read | goes past the *Forbidden* read, same exit 2 — the previous version stopped on "does not exist" for a namespace that existed |
| `--pull-policy Never` with the image absent from the node | exit 2 in 6 s naming `ErrImageNeverPull` — the previous version waited out the whole window |
| a credential with every right but `pods/log` | **exit 2** naming the unreadable logs — the previous version exited 1 claiming the pod "carried NO seccomp filter": kubectl's error had become the posture log |

## What it does not earn, printed in every run

The two `Localhost` seccomp **mutants are not run remotely**. A Localhost
profile lives in the kubelet's seccomp root on the node, i.e. node-level
access this arm refuses to require (a privileged DaemonSet on a production
machine). That control stays where it is green — the `kind` arm — and its
register is [posture-bench-mutants.md](posture-bench-mutants.md).

Leg 5 covers the BENCH's own `Confine`, built from HEAD; legs 1-4, 6 and 7
exercise the image's own `iterion`. Same caveat as every arm.
