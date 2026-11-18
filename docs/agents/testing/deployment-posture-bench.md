# When the deployment, not the code, decides the guarantee

Some guarantees are not properties of this repository. Codemode's confinement
is one: the code asks the kernel for it, and whether the kernel answers is
decided by the image, the pod's securityContext and the container runtime in
use. This page is about measuring such a guarantee where it has to hold.

The live instance is the codemode posture bench — `task bench:codemode-inpod`
(docker) and `task bench:codemode-cluster` (containerd),
[#3155](https://github.com/SocialGouv/iterion/issues/3155), ADR-165.

## What the unit suite already does, and what it cannot

`pkg/scripthost`'s suite does **not** go green without Landlock. It fails
loudly, in its own words — *"It does not skip […] must say so rather than
report green"* — skipping only when a lane sets
`ITERION_CODEMODE_ALLOW_NO_LANDLOCK=1`. Measured with a profile denying the
syscall: 5 passes become 5 failures under `ENOSYS` and 5 under `EPERM`. That
guard is sound and nothing here replaces it.

What it cannot do is judge the **shipped image under the deployment's
posture**: it compiles and runs on the host and knows nothing of the pod's
securityContext or the runtime's seccomp profile. In the default cloud posture
that is the whole question — `runner.sandbox.enabled: false` renders
`ITERION_SANDBOX_OVERRIDE: "none"`, *the pod is the isolation boundary*, so
loop, bash and a script host are siblings under one uid, and a container
profile can refuse the syscall the confinement is built on. Only something
inside that container finds out.

## The replies to `landlock_create_ruleset`, and why they must stay apart

`Confine` keeps them distinct because collapsing two of them once produced a
child with a full filesystem and no error at all:

| reply | what it means | what codemode does |
|---|---|---|
| success | the domain is in force | runs, and `Applied.LandlockABI` says which ABI |
| `ENOSYS` | this kernel has no Landlock | **refuses** (`codemode_unavailable`), reporting `landlock_abi: 0`, and names `allow_without_landlock` as the deliberate acceptance |
| `EOPNOTSUPP` | the kernel HAS Landlock and it is disabled at boot — the remedy is the `lsm=` command line, not a newer kernel | same refusal, same reported ABI |
| `EPERM`, or any other errno | something refused the syscall — the shape a container profile gives a call it does not allow | **refuses**, reporting no confinement at all; a deployment fault, not an absence |

A bench that only checks "did it run" cannot tell the first row from a silent
fourth. Read `Applied`, then make the child try to escape: an ABI number says a
ruleset was *installed*, never that it *refuses*.

## What the bench runs

The shipped binary of the image you pass, inside that image, under the pod
securityContext of `charts/iterion/values.yaml`. The posture is not trusted to
the flags, and not to `/proc/self/status` alone either: the bench **refuses to
measure** unless the uid and the primary gid match and are not 0, **no supplementary
group other than that one is held**, neither the effective nor the bounding
capability set holds a bit, `no_new_privs` is set, a seccomp filter is loaded —
**and that filter denies everything a runtime default denies**.

Every clause was earned by a container that passed without it: `--privileged`,
then `--uid 0`, then `--privileged` with a permissive profile (an unprivileged
uid has `CapEff: 0` whatever it may *acquire*, so only `CapBnd` catches it),
then an **allow-all profile alone** (`Seccomp: 2` names the MODE, never the
PROFILE), then a pod holding **group root** — `fsGroup` writes `Groups:` and
`os.Getgid()` cannot see it, measured: `--group-add 0` leaves `Gid: 10001`
untouched and the pod's scratch emptyDir came out root-group. That last one matters most: the profile is the single input this gate
exists to judge, it comes from the daemon's `--seccomp-profile` and the
kubelet's `RuntimeDefault`, and nothing read it.

**Teeth and identity are two questions, and neither alone is worth much.**

Teeth are the KERNEL's answer, over a **set**: `add_key`, `keyctl`, `bpf`,
`perf_event_open` and `unshare` must all answer `EPERM`, and the whole
fingerprint is printed. One member was not enough — a profile denying only
`add_key` passed and reported every leg held while four of the others reached
the kernel, `unshare` outright succeeding. An `ENOSYS` from `add_key` is
accepted as *not covered* only when procfs corroborates it: that hatch used to
be controlled by the very input under judgement, and a profile answering
`ENOSYS` produced a green while `/proc/sys/kernel/keys/maxkeys` read 200.

Identity is the RUNTIME's own record, and both halves of it. The docker arm
requires `docker info` to report `name=seccomp,profile=builtin` **and** the
measured container's `HostConfig.SecurityOpt` to carry no `seccomp=` — the
first describes the daemon's default and says nothing about this container.
The cluster arm requires the pod-level `seccompProfile` to be `RuntimeDefault`
**and** every container-level one to be absent or the same, because the
kubelet merges the container field OVER the pod field: reading only the pod
field answered `RuntimeDefault` for a pod running `Seccomp: 0`.

What the pair establishes is bounded, and stating the bound is the point. The
fingerprint is a **necessary condition**, not a characterisation: it says this
filter refuses every member a runtime default refuses, and a profile denying
exactly the probed members would satisfy it. The earlier wording — *as far as a
process inside the container can tell* — was wrong, and measurement is what
refuted it: a process inside the container can tell more, which is how a
profile denying exactly the first five members was caught letting
`io_uring_setup` return a working fd. Members earn their place by what they
**discriminate**, which [the mutant register](posture-bench-mutants.md)
records. The identity records say no one asked for a different profile, and
neither half proves its bytes.

No `-require-uid` is refused outright rather than silently measuring nothing,
and `-allow-any-posture` exits on its own code: it used to answer 0 from a
`--privileged` uid-0 container, which is the container this guard exists to
refuse.

Seven legs, each counted:

1. **the posture** — and it is judged, not just printed: `LandlockABI > 0` and
   `AllThreads`, or the bench exits non-zero;
2. **a memory bomb** → `memory_exceeded`;
3. **a native regex bomb** → `cpu_time_exceeded`;
4. **a benign program** after them, which must still return its value;
5. **a compromised child** that tries to `open`, `connect` and `exec` — the
   leg that proves the domain *refuses*. Each attempt runs twice, before and
   after `Confine`, because one that fails on both sides proves nothing. And
   it must fail with **`EACCES`**, the errno Landlock refuses with on ABI 4
   (connect included, measured): a mutant that installed no domain and set
   `RLIMIT_NOFILE` to zero made all three fail with `EMFILE`, and a leg asking
   only *whether* they failed printed "too many open files" and passed. Below
   ABI 4 there are no TCP rights, so connect is reported **not covered**;
6. **three bombs beside three benign programs**, where all six must end the
   way their bounds promise *and* some 5 ms sample must have seen a bomb and a
   sibling alive **at the same instant**. Two weaker forms both died to a
   mutant: timestamps taken around the call include the wait in queue, so a
   global lock still reported maximum overlap; and counting live children is
   satisfied by either group alone, so a scheduler admitting the three
   siblings together and then the three bombs together passed with a peak of
   3. The kinds are told apart by `argv[0]` — the bombs run through a second
   `Host` whose `Binary` is a symlink of the same file — so that mutant now
   reports **0 samples with both kinds** and reddens;
7. **no descendant outliving a `SIGKILL`ed parent**.

## Two runtimes, because `RuntimeDefault` is not one profile

`RuntimeDefault` names *whatever the runtime defaults to*, so it is a different
filter under docker than under containerd, and a bench measuring one would be
answering for the other by assumption. Both are measured:

| runtime | how | verdict |
|---|---|---|
| docker 29.9.3, default profile | `task bench:codemode-inpod` | ABI 4, all threads, 7/7 |
| **containerd**, `seccompProfile: RuntimeDefault` | a real Pod with the chart's securityContext on a local `kind` cluster (`kind` is a devbox package here) | ABI 4, all threads, 7/7, exit 0 |

The containerd version belongs to the **node image**, pinned by digest and
reported in the log: unpinned, the same command measured 2.1.1 then 2.2.0.

Each arm reads its filter back against an `Unconfined` twin reporting
`Seccomp: 0`, which also settles that `NoNewPrivs: 1` comes from
`allowPrivilegeEscalation: false`: the twin keeps that flag, so only seccomp
differs.

The claim these arms support is precise: **under the full posture a child still
OBTAINS a Landlock domain (ABI 4, all threads) — none of these flags refuses
`landlock_create_ruleset`.** That the domain then *refuses* an open, a connect
and an exec is leg 5's business, not an inference from the ABI. For a
non-blocking claim the conjunction suffices: these flags are restrictions and a
syscall's success is monotonically decreasing in restrictions, so if one
blocked Landlock the run carrying all of them would have failed.

## The remote arm

`bench:codemode-bench-image` + `bench:codemode-remote` carry this bench to a
cluster this host does not own: [its own page](posture-bench-remote.md).

## The mutants, and why the bench ships with them

A green never seen red is worth nothing, so the bench carries its own mutants
and **the member fails if a mutant passes**. Only exit `3` credits one.

The register — seven mutants, six killed and one assumed, and what each one
defeated — is [its own page](posture-bench-mutants.md), because it is the part
that grows every time someone attacks this bench.

## What it does not cover

**Leg 5 runs the BENCH's `Confine`, not the image's** — the probe is this
binary, built from HEAD, so legs 1-4, 6 and 7 exercise the image's own
`iterion` and that one does not. Immaterial while the pinned digest ships this
branch's parent and nothing here touches `pkg/scripthost`. And **the cluster
arm's posture readback is a sibling pod**: the bench pod checks itself through
`-require-uid`, which is what makes the arm sound.

**The production node's containerd.** The arm that can measure it exists and is
witnessed on a local cluster, but has **not yet run against production**: its
own build is unmeasured.

The production **runner image** is no longer owed: `iterion-runner-devbox`
(uid 1000, `iterion v6.1.0+f446b258cf38`, `tini`) holds 7/7 with the escape leg
refused `EACCES` on all three attempts — `RUN_AS_UID=1000`. Not `UID`, because
`UID=1000 task …` is refused by bash (readonly) and the member would fall back
to its default in silence. The first version of this note gave the opposite
reason, "go-task would inherit it from the shell", and measurement refuted it:
`UID` is not exported.

**A native payload's real ceiling.** `RLIMIT_DATA` bounds the Go heap, and
`Confine`'s own comment is explicit that *a `MAP_SHARED` or memfd mapping is
not charged to it, so a native payload's real ceiling is the pod's memory
cgroup*. That axis is the ground `oom_score_adj` and RSS probing were meant to
cover in #3155, and neither mechanism exists in this codebase (zero production
sites, every spelling). So those two legs cannot be written as specified — but
the axis is **owed**, not covered: leg 6 bounds each bomb by its own
`RLIMIT_DATA` and cannot observe an eviction.

**A floor on the memory bound.** Found by this bench: `Limits.Clamp` caps
`Memory` and fills an unset value from the default but enforces no minimum, so
a bound below what the child's Go runtime needs makes it **crash** — measured
at 64 MiB, `script_host_died` with a SIGSEGV where the caller asked for
`memory_exceeded`. The bench uses the shipped default and refuses leg 6 when
the cgroup cannot hold the bombs; the product behaviour is its own ticket.
