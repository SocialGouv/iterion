# The memory bound RLIMIT_DATA does not reach

`Confine` bounds the child's data segment, and its own comment says why that is
not the whole story:

> RLIMIT_DATA, not RLIMIT_AS […] a `MAP_SHARED` or memfd mapping is **not
> charged to it**, so a native payload's real ceiling is the pod's memory cgroup.

The Go runtime's own heap is private anonymous memory, which RLIMIT_DATA does
charge. What escapes it is shmem-backed memory — a shared anonymous mapping, a
memfd — and **no path a script can reach today is known to make one**: the
child's tools are served by the parent's dispatcher, not inside the child, and
its Landlock domain refuses `exec`. So this is defence for a class the kernel
bound does not cover, not the answer to an observed escape. Left uncovered, the
only stop would be the pod's cgroup — whose OOM killer picks its victim **by
size**, and the biggest process in a runner pod is the runner.

## What bounds it

Two things, both started by the parent before the child can allocate:

- **`oom_score_adj = 1000`** on the child, written by the parent so it lands
  whichever interpreter the child runs. A refusal goes to `Host.OnWarn` — the
  node's log on the claw backend — on **every** outcome: written only to the
  child's stderr, which is read only on the death path, it was lost on every
  run that returned a value. Without the mark an OOM may take the runner.
- **A probe of the child's charged memory**: `RssAnon + RssShmem` from
  `/proc/<pid>/status`, sampled every 20 ms. The parent kills the child's
  **process group** when that grows past the bound, measured from the first
  sample. The probe acted, so the verdict is `memory_exceeded` — positive
  identification by the party that killed, ranked beside the Go runtime's own
  out-of-memory message and above every threshold.

**Not `VmRSS`.** `VmRSS` adds `RssFile`, the pages of the binary and of mapped
files: paged in as code first runs, reclaimable, never charged by RLIMIT_DATA —
and most of the resident set (even `cat` shows three quarters of its `VmRSS` as
`RssFile`). The first version read `VmRSS` and killed a legitimate run under the
64 MiB clamp floor in CI. A kernel without the two fields gets an error naming
the missing one, never a silent fall back to `VmRSS`.

It is a bound with an overshoot, and the verdict says how much: at most one
sample of the child's own allocation rate, and the message carries the bound,
the growth and the baseline. That is the trade this codebase chose over a
per-child cgroup, which would be exact — `memory.max` plus `oom.group=1` kills
the child's whole subtree and nothing else — but needs cgroup delegation inside
the pod, refused where it was measured and not guaranteed in a production pod.

## What proves it

- **The premise, executed**: a helper grows a 96 MiB `MAP_SHARED` mapping with
  16 MiB of `RLIMIT_DATA` room left and **finishes**. Without this the probe's
  verdict would be about something else.
- **The kill**: the same helper under the probe is killed, and the reported peak
  is at or above the bound it passed.
- **The quantity**: a synthetic status document whose `VmRSS` is mostly
  `RssFile` charges `RssAnon + RssShmem` only, and a document missing either
  field is an error naming it.
- **The restraint**: a child that stays under its bound is left alone, and a
  bound of zero — "nobody chose" — samples nothing at all.
- **The verdict**: the probe's kill is `memory_exceeded` even when the child
  also burned its CPU budget (the ordering is the correctness), the message
  names the measured growth, and a probe that **never sampled** says so on an
  otherwise unexplained kill.

Five mutants, each reddening the test that owns it: the verdict branch removed,
the comparison widened, `RssFile` added to the sum, `RssShmem` dropped from it
(the live kill test catches that one — a shared mapping is shmem), and the
missing-field guard removed. Two earlier witnesses were wrong and are recorded
because the shape recurs: the kill test **hung** under its mutant instead of
failing (it is bounded now), and a first `RssFile` mutant that *overwrote* a
term instead of adding one survived — a no-op mutant proves nothing either way.

## What is still owed

An **in-pod leg** — the pod survives while the child dies, under the
deployment's own posture. It cannot be written against today's child: no
script-reachable path produces shmem growth there (see the top of this page),
so the bench has no payload to run. The axis is proven in `pkg/scripthost`, in
process, with a helper that maps the memory itself.
