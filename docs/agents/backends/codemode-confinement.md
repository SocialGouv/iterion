# Codemode confinement — the bounds, and what each one is proven by

The script host runs model-authored JavaScript in a child process that holds no
credential ([the leaf](codemode.md),
[ADR-165](../../adr/165-codemode-runs-the-models-code-in-a-child-process-behind-the-tool-door.md)).
Nothing of a node reaches it yet, on purpose: the whole content of its first
slice ([#3175](https://github.com/SocialGouv/iterion/issues/3175)) is the
confinement, so it can be reviewed as confinement.

Code: [pkg/scripthost](../../../pkg/scripthost). Every bound carries an
operator override and a platform ceiling a tenant cannot raise (`DefaultLimits`,
`Ceiling`, `Clamp`); a reduction is **reported** through `Host.OnClamp`, never
silently applied. Each bound is proven by a witness that was **seen to fail**
when the bound was removed — 36 mutants, 34 red, 2 deliberate survivors named
at the end.

| bound | mechanism | what the witness shows |
|---|---|---|
| memory | `RLIMIT_DATA` | a 400 MiB allocation dies under a 128 MiB bound |
| CPU | `RLIMIT_CPU` | a native regex bomb is killed; nothing else kills it |
| wall clock | the parent's deadline | a regex bomb the interpreter cannot interrupt is still stopped |
| wall clock (soft) | `vm.Interrupt` | a preemptible loop yields a typed refusal from the child |
| recursion | `SetMaxCallStackSize` | a self-calling function is refused with a cause |
| filesystem | Landlock, deny-all | a file readable a moment earlier is refused |
| network | Landlock ABI 4 | a TCP connect to a live listener is refused |
| exec | Landlock `FS_EXECUTE` | `execve` of an existing binary is refused at `fork/exec` |
| **every thread** | `AllThreadsSyscall` | a sibling OS thread is refused too |
| credentials | empty `cmd.Env` + `PR_SET_DUMPABLE` | a same-uid sibling cannot read the child's `/proc` |
| script size | parent pre-spawn, child again | an oversized script costs no process |
| result size | child at production, parent cap | an oversized value never crosses, and never grows the parent |

## The five statements measurement refuted

**`landlock_restrict_self` restricts the calling THREAD.** This is the one that
mattered. In a Go process a plain syscall from a goroutine confines whichever
OS thread happened to carry it; `Confine` reported `LandlockABI: 4` while a
sibling thread read, wrote, connected and exec'd freely, and four goroutines out
of four landed on unconfined threads. `no_new_privs` is per-thread too. Both are
now driven with `syscall.AllThreadsSyscall`, which also replays onto threads
created afterwards — measured: every probe refused, on the confining thread, on
a pre-existing sibling, and on every goroutine.

A **cgo-linked** binary cannot do that: the runtime answers `ENOTSUP`. iterion
ships `CGO_ENABLED=0`, so it never arises in production — and where it does,
`Applied.AllThreads` is false and the child **refuses** rather than presenting a
per-thread bound as a per-process one.

**`RLIMIT_AS` is not a memory bound for a Go child.** It counts reserved
address space, and the runtime dies at startup ("runtime: out of memory")
anywhere below about 2 GiB — far above any heap budget worth setting.
`RLIMIT_DATA` starts a child at 64 MiB and still kills the bomb. The two are
not interchangeable, and only one can express what codemode needs.

**The interpreter cannot bound itself.** A backreference regex ran **past 60 s**
of wall clock with `vm.Interrupt` posted at 1 s; `RLIMIT_CPU=2s` killed the same
script. That is ADR-098's F19 on the engine this tree pins, and it is why the
child's *death* is the stop for that class.

**A bare `goja.Runtime` is why there is no deny list.** `require`, `console` and
the timers come from goja_nodejs's event loop, which this package does not use;
`Math.random` is seeded from the parent's seed and `Date` is pinned. A
breadth-first walk of the reachable graph found 517 objects and zero functions
whose source is not `[native code]`. The 54 globals are a **golden**, not a
list of names someone thought of.

**"Landlock refused" is not "Landlock absent."** The ABI probe mapped every
errno to zero and zero meant absence, which `Confine` swallowed: a seccomp
filter answering `EPERM` for `landlock_create_ruleset` — the shape a container
profile gives a syscall it does not allow — produced a child with a full
filesystem, full TCP and no error at all. Only `ENOSYS` and `EOPNOTSUPP` are
absence; everything else stops the run. And absence is now a **verdict**: a
kernel without Landlock refuses unless the request carries
`allow_without_landlock`, the greppable escape hatch, which travels in the
request because the child's environment is empty by design.

## What the bounds do NOT cover

- **They are per PROCESS, not per process tree.** `RLIMIT_CPU` and
  `RLIMIT_DATA` accounting restarts at zero in a forked task while the value is
  inherited: four forks burned 8 s of CPU under a 1 s bound. What bounds a tree
  is the parent's wall deadline plus the process-group kill, which does collect
  descendants — measured. A `pids` and `memory` cgroup is the cloud twin this
  owes.
- **`RLIMIT_DATA` bounds the Go HEAP, not the process.** A `MAP_SHARED`
  anonymous mapping and a memfd are not charged to it: 8× the bound, resident,
  no fork needed. The goja-heap claim holds; "the child is bounded at N bytes of
  memory" does not.
- **The network bound is TCP.** UDP, AF_UNIX (pathname and abstract) and netlink
  all cross a deny-all ruleset on ABI 4, verified by what arrived at a listener
  outside the confinement. ABI 6's two scopes close the abstract socket and
  signals, and are declared where the kernel offers them. Closing the rest needs
  a netns or seccomp, which is not this slice.
- **A path-existence oracle survives.** `connect(AF_UNIX, path)` returns
  distinguishable errnos, so a confined child can tell a file from a missing
  path it can never open.
- **The CPU bound overshoots by one second.** The soft limit sits at the
  operator's number and the hard limit one second above it, clamped to whatever
  the environment already set. The gap is what makes the death attributable: the
  Go runtime ignores `SIGXCPU`, so the `SIGKILL` at the hard limit is what
  kills, and with equal limits that kill lands AT the bound while the rusage the
  parent reads is quantised just under it — measured 987 ms and 999 ms against a
  1 s bound on different runs of the same script. A parent comparing
  `burned >= bound` therefore flipped at random between `cpu_time_exceeded` and
  "nothing names a bound". A verdict that flips at random is worse than one that
  costs a second.

## Naming a bound, or saying you cannot

`deathToRefusal` names a bound only where the evidence identifies one, in order
of evidence strength: the caller's own context, the parent's deadline, a memory
death the runtime printed, a CPU kill (signalled **and** over the enforced
bound), the parent's stdout cap, then an honest ambiguity. Two measured reasons
for that order:

- A threshold on burned CPU ran before the positive identification of a memory
  death, so an allocation failure that had burned its budget was reported as
  `cpu_time_exceeded` **while the parent held the stderr saying
  `runtime: out of memory`**. An operator reading that raises the CPU bound
  forever.
- The same bomb prints `runtime: out of memory` under a 128 MiB bound and dies
  with `SIGSEGV` under 64 MiB. A crash under a tight bound is usually the bound,
  and the runtime does not say so — so the verdict says *that*, rather than
  guessing. A crash is the script host failing, not the deployment: a child that
  answered nothing this code understands is a host **error**, and a child that
  crashed is a refusal.

## Three witnesses that first passed for the wrong reason

Each is fixed, and each is why the arms now carry a premise and an error class:

- The `/proc` arm asked the question **of itself**. A confined process cannot
  open `/proc/self/environ` because *Landlock* denied the path, whichever way
  `PR_SET_DUMPABLE` went — so removing the prctl left the suite green. The
  threat model is a sibling in the pod; the parent probes now, and the helper
  waits for a handshake so the premise is a measurement rather than a race.
- The `exec` arm never reached `execve`: with nil stdio, `os/exec` opens
  `/dev/null` first and the ruleset refused *that*.
- `refused()` credited any error, so the write arm passed on `ENOENT`. Every
  attempt now reports whether the kernel refused it for **permission**, and
  every arm asserts the same act succeeded before confinement.

## What each lane proves, and what it says it cannot

A bound an arm cannot test is **named**, never absorbed by a skip: the suite
fails loudly unless the lane is on record, and the record is an environment
variable set in the workflow beside a comment saying what that lane does not
test.

- **No Landlock at all** — the filesystem and network arms cannot run.
  `ITERION_CODEMODE_ALLOW_NO_LANDLOCK=1`.
- **Landlock below ABI 4** — the network rights arrive there, so a confined
  child can open a TCP connection and the arm cannot conclude.
  `ITERION_CODEMODE_ALLOW_NO_LANDLOCK_NET=1`. **This repo's ARC runners answer
  ABI 1** (measured 2026-11-11), so the record is set at `tests.yml`'s workflow
  level — four jobs run the Go suite there and a record on one of them is a
  record on none. Both variables are consulted only BELOW the ABI an arm needs,
  so a runner that offers the right proves it and ignores them.

So on CI the memory, CPU, wall-clock, recursion, filesystem, exec, credential
and every-thread bounds are proven, and the TCP one is not.

Two things the record used to get wrong about its own legs, both measured:

- The memory arm does **not** skip under `-race`. Its child is the shipping
  `CGO_ENABLED=0` binary that `TestMain` builds, which ThreadSanitizer never
  reaches, so it is proven on every leg. The race floor
  (`ITERION_SCRIPTHOST_RACE_FLOOR_MIB`) applies to the arms whose child IS the
  instrumented test binary — the escape helper and the hand-built requests.
- Whether the test binary is cgo-linked is a property of the LEG, not a fact:
  `CGO_ENABLED=0` on CI's unit leg, 1 on its race shards. So the process-wide
  guarantee is asserted against the shipping artefact, and its REFUSAL against
  an iterion deliberately built with cgo — the one configuration in which a
  child cannot drive its own threads.

## The two deliberate survivors

Both are belts with no behavioural witness, stated rather than dressed up:
`time.Local = time.UTC` before Landlock (without it the denied zoneinfo read
makes Go fall back to UTC, the same answer) and the `recover` in `runScript`
(the two sites that panicked are closed at the site, and nine further
adversarial shapes refuse without it). A mutation bench finds them as
survivors by design.
