# The memory bound RLIMIT_DATA does not reach

`Confine` bounds the child's data segment, and its own comment says why that is
not the whole story:

> RLIMIT_DATA, not RLIMIT_AS […] a `MAP_SHARED` or memfd mapping is **not
> charged to it**, so a native payload's real ceiling is the pod's memory cgroup.

A script cannot call `mmap` itself, but the child is a Go process and every
mapping the runtime or a native call makes is outside the bound. Left there, the
only thing that stops such a payload is the pod's cgroup — whose OOM killer picks
its victim **by size**, and the biggest process in a runner pod is the runner.

## What bounds it

Two things, both started by the parent before the child can allocate:

- **`oom_score_adj = 1000`** on the child, written by the parent so it lands
  whichever interpreter the child runs. It is inherited by descendants, which is
  what covers a native payload a tool call spawns. A refusal is reported on the
  child's stderr, never swallowed: without the mark an OOM may take the runner.
- **A resident-set probe.** The parent reads `/proc/<pid>/status` `VmRSS` every
  20 ms and kills the child's **process group** when it passes the bound. The
  probe acted, so the verdict is `memory_exceeded` — positive identification by
  the party that killed, ranked beside the Go runtime's own out-of-memory
  message and above every threshold.

It is a bound with an overshoot, and the verdict says how much: at most one
sample of the child's own allocation rate, and the message carries both the
bound and the peak the parent measured. That is the trade this codebase chose
over a per-child cgroup, which would be exact — `memory.max` plus
`oom.group=1` kills the child's whole subtree and nothing else — but needs
cgroup delegation inside the pod, refused where it was measured and not
guaranteed in a production pod.

## What proves it

- **The premise, executed**: a helper grows a 96 MiB `MAP_SHARED` mapping under
  a 32 MiB `RLIMIT_DATA` and **finishes**. Without this the probe's verdict would
  be about something else.
- **The kill**: the same helper under the probe is killed, and the reported peak
  is at or above the bound it passed.
- **The restraint**: a child that stays under its bound is left alone, and a
  bound of zero — "nobody chose" — samples nothing at all.
- **The verdict**: the probe's kill is `memory_exceeded` even when the child
  also burned its CPU budget (the ordering is the correctness), the message
  names the measured peak, and a probe that **never sampled** says so on an
  otherwise unexplained kill — "saw nothing" and "could not look" are the same
  silence otherwise.

All four mutants redden the test that owns them. The first version of the kill
test **hung** under its mutant instead of failing; it is now bounded.

## What is still owed

An **in-pod leg**. `scripts/ci/inpodbench` wires no tool channel, so no script
it runs can produce a native payload — the axis is proven in `pkg/scripthost`,
in-process, and not under the deployment's own posture. Closing it means giving
the bench a tool that can allocate natively and asserting that the pod survives
while the child dies.
