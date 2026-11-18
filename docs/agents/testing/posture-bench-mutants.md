# The posture bench's mutants

Every row here is a mutant that this bench ships with, and the member **fails
if a mutant passes**: a green never seen red is worth nothing. Only exit `3`
("the child could not confine itself") credits one, because accepting any
non-zero once credited a `126` from an unexecutable binary and a missing image
as proof that the bench could see a confinement failure.

The bench itself, and what it measures:
[when the deployment decides the guarantee](deployment-posture-bench.md)
([#3155](https://github.com/SocialGouv/iterion/issues/3155), ADR-165).

A green never seen red is worth nothing, so the bench carries its own mutants
and **the member fails if a mutant passes**. Only `3` credits one, because
accepting any non-zero credited a `126` from an unexecutable binary and a
missing image as proof that the bench can see a confinement failure.

| mutant | what it removes | result |
|---|---|---|
| `MUTANT=eperm` (both arms) | the syscall, with `EPERM` | exit 3, no confinement reported |
| `MUTANT=enosys` (both arms) | the syscall, with `ENOSYS` | exit 3, `landlock_abi: 0` + the `allow_without_landlock` remedy |
| Landlock installs nothing but reports ABI 4 | the domain's effect | leg 5 red: *open/exec/connect SUCCEEDED inside the confinement* |
| the same, plus `RLIMIT_NOFILE: 0` | the domain, while still failing every attempt | leg 5 red on the **errno**: `EMFILE`, not `EACCES`, so something other than the domain stopped it |
| a global lock around every run | the concurrency | leg 6 red: no sample saw both kinds |
| a scheduler admitting one kind at a time | the *meeting*, keeping concurrency inside each group | leg 6 red: peak 3 children, **0** samples with both kinds — the metric it defeated reported 3 and passed |
| `--privileged` + a permissive seccomp profile | `capabilities: drop: [ALL]` | exit 4: `CapBnd 000001ffffffffff`, 41 bits still there |
| an allow-all seccomp profile, nothing else | the filter's **teeth**, not its mode | exit 4: the metric it defeated read `Seccomp: 2` and passed |
| an allow-all profile denying **only `add_key`** | the rest of the default deny list | exit 4: *let 4 of 5 probes reach the kernel* — the one-syscall probe it defeated answered EPERM and passed, while `unshare` succeeded |
| the same, answering **`ENOSYS`** for `add_key` | the probe's own escape hatch | exit 4: `/proc/sys/kernel/keys/maxkeys` reads 200, so "this kernel has no keyring" was the filter talking |
| pod-level `RuntimeDefault` + container-level `Localhost`(permissive), on a real cluster | the kubelet's merge | the identity read it defeated answered `RuntimeDefault` while `keyctl`, `bpf` and `perf_event_open` reached the kernel |
| `-allow-any-posture` in a `--privileged` uid-0 container | the posture check itself | exit 7: it used to answer **0**, and now replaces every terminal code of a measuring path |
| `-allow-any-posture` + no Landlock | the same, at three exits the first fix missed | exit 7: it answered **5**, because two of the posture switch's five terminal exits were wrapped and three were not — found by the gate reviewer, not by a local round |
| `--group-add 0` | `fsGroup`, which writes `Groups:` and not `Gid:` | exit 4: the guard it defeated read `os.Getgid()` and reported 7/7 while the pod's emptyDir came out root-group |
| a profile denying exactly the **first five** probes | the rest of what a runtime default refuses | exit 4: *let 2 probes reach the kernel* — the five-member set it defeated passed while `io_uring_setup` returned a working fd |

## The coupling, and why it is single-sourced

The `eperm`/`enosys` profiles must deny **everything the posture check probes**,
plus `landlock_create_ruleset`. Otherwise the check refuses them for being
permissive — correctly, and uselessly: a mutant must be refused for its
Landlock denial, at exit 3, not at exit 4.

That happened **twice**. The profile denied one syscall, then two, while the
probe set grew to five, and each time both arms reported `mutant … ended with
4, not the posture-refused code 3`. The exit contract caught it both times,
which is the contract earning its keep — but the second occurrence said the
coupling itself was the defect.

So the set has ONE source: `inpodbench -print-probe-set` prints it, and both
arms build their mutant profiles from that output. Strengthening the posture
check can no longer silently neuter the mutants that prove it.
| `--user 10001:0` | `runAsGroup` / `fsGroup` | exit 4, refused before any leg |
| no Landlock + `-allow-without-landlock` | the domain | exit 5, *it is not a pass* |
| `applyParentDeathSignal` becomes a no-op | the backstop | **assumed survivor**: leg 7 stays green |
| `Confine` skips the socket filter, `sockets_denied` still true | the network wall | leg 5 red: *a UDP send SUCCEEDED* — the only attempt that shows it on an ABI 4 host, where Landlock still refuses `connect` and docker's profile `io_uring` |

Most rows exist because a guard this bench shipped was green on the class it
was written for: leg 5 asked only *whether* an attempt failed; leg 6 compared
timestamps, then counted children of any kind; the posture guard read only the
effective capability set, then only the seccomp MODE. Four adversarial rounds,
each killing a guard the previous had just written, every time by writing the
mutant rather than re-reading the code — and twice the guard had been fixed in
**one field of a two-field class**, which is the shape to look for first.

That last row is named rather than hidden. `applyParentDeathSignal`'s own doc
calls `Pdeathsig` a *backstop* and not the orphan bound, and the mutant agrees:
the descendant is still reaped. Leg 7 therefore witnesses the property a drain
needs — nothing outlives the parent — and nothing about which holder did it.

## Five traps this bench paid

They are measurement lessons rather than facts about codemode, so they live
with their kind: [when a test measures its host](host-shaped-witnesses.md).

## Choosing a probe member

A member of the posture check's syscall set earns its place by what it
**discriminates**, not by reputation. `mount`, `init_module`, `kexec_load`,
`pivot_root` answer `EPERM` from the KERNEL to an unprivileged process whatever
the filter says, so probing them witnesses nothing. `io_uring_setup` and
`mbind` are the opposite — they succeed under a permissive filter and are
`EPERM` under both runtime defaults, measured on docker 29.9.3 and containerd
2.1.1 — which is why they were added when a profile denying exactly the first
five members was found passing.

## The shape, four times over

Four of the rows above are the same defect: **a guard corrected on ONE member
of a multi-member class**. `CapEff` without `CapBnd`. The seccomp mode without
the seccomp profile. One probed syscall instead of a set — then five instead of
seven. `Gid:` without `Groups:`, where `fsGroup` lands. And then `verdict()` on two of
five terminal exits — **inside the fix written to close that very class**, which
is the fifth occurrence and the one the gate reviewer caught. Twice the witness
did not carry the field the reader judged, either.

The two helper-process exits are the named exemption: the escape probe's and
the drain parent's codes are read by their own leg, never by a gate.

So when a guard here is strengthened, the question is not "is this field right"
but **"what is the rest of this class, and who reads it"** — answered by a grep
from the producer, not from memory.

## Why this page exists at all

Four adversarial rounds attacked this bench, and **each one killed a guard the
previous round had just written** — every time by writing the mutant rather
than re-reading the code. Twice the guard had been fixed in **one field of a
two-field class** (`CapEff` without `CapBnd`; the seccomp mode without the
seccomp profile), which is the shape to look for first in anything here.

So this register is not decoration and it is not finished. Adding a guard to
that bench means adding the mutant that proves it, in this table, with the
command that made it red.
