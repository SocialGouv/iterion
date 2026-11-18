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

The bench's own `4` ("this pod is not the posture asked about"), `6` (a leg
could not run here) and `7` (the pod never checked its posture) all map to **2
— nothing was tested**, never to a green and never to a red about the cluster.

## Witnessed, not assumed

Against a `kind` cluster driven purely through `kubectl`:

| what | result |
|---|---|
| measured pod + readback + twin, namespace created with the privileged label | 7/7, exit 0, twin admitted, `containerd://2.1.1` attributed |
| same arm in a namespace whose PodSecurity is `restricted` | twin refused by admission → a `GAP` line, verdict green and *declared* |
| measured pod switched to `Unconfined` (mutation) | the bench refuses its own posture, exit 4 → *nothing was tested* |
| **readback** switched to `Unconfined` beside a confined bench (mutation) | the `Seccomp: 2` assertion reddens, exit 1 |
| a base image whose `iterion` predates `__script-host` | exit 1, two named failures — a green it cannot fake |

## What it does not earn, printed in every run

The two `Localhost` seccomp **mutants are not run remotely**. A Localhost
profile lives in the kubelet's seccomp root on the node, i.e. node-level
access this arm refuses to require (a privileged DaemonSet on a production
machine). That control stays where it is green — the `kind` arm — and its
register is [posture-bench-mutants.md](posture-bench-mutants.md).

Leg 5 covers the BENCH's own `Confine`, built from HEAD; legs 1-4, 6 and 7
exercise the image's own `iterion`. Same caveat as every arm.
