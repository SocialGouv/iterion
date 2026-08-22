# The worktree pool and its bound

A `worktree: auto` run executes in a fresh git worktree under
`<store-dir>/worktrees/<run-id>`. That directory is a **full checkout of
the repository** — on iterion's own tree, 355 MB, of which 309 MB is the
vendored dependencies every worktree faithfully copies.

A run that exits cleanly removes its checkout. A run that **fails or is
interrupted keeps it**, deliberately, so the operator can inspect what the
agent left behind. Nothing used to come back for those: `iterion runs
prune` only touches `runs/`, and [`iterion clean`](cli-reference.md#iterion-clean)
is a command you have to know exists and remember to run.

So a store whose runs fail grew by one full checkout per failure, with no
ceiling and no signal. On 2026-08-22 a studio left unattended for forty
minutes reached **32 worktrees and 12 GB**, on a host where the store had
been pointed at a 16 GB tmpfs; the machine ran out of RAM and started
killing processes, which is how anyone found out.

The **pool bound** closes that. It is not a cap and it never refuses a
run — see [What it will not do](#what-it-will-not-do).

## What happens, and when

Every time a run is about to create a worktree, before `git worktree add`:

1. **Count the pool.** One `ReadDir`. At or under the budget, nothing else
   happens — which is what makes the check affordable on the path that
   creates every worktree.
2. **Set live runs aside.** A worktree an executing run owns is never a
   candidate and **never counts against the budget**, so raising
   parallelism never fights the bound and a busy store does not warn on
   every launch. Run status plus the per-run process lock distinguish a
   live owner from a stale `running` record left by SIGKILL or OOM.
3. **Reclaim the excess, oldest first** — and only the excess. A ceiling is
   a ceiling, not a sweep. Classification is lazy, so a healthy pool pays
   for the few entries it actually reclaims rather than for all of them.
4. **Warn if it could not get back under**, naming how many, why, and a
   command that would work on that pool.

Above the budget, classification invokes git for candidates and can add
launch latency. It is bounded both to 10 seconds and to four newly
classified candidates per launch. A small cursor in the pool advances the
next process through later candidates, so repeated one-shot `iterion run`
commands do not rescan the same refused prefix forever. Refusals are also
cached for five minutes inside a long-lived process such as Studio.

The moment of creation is the only one where acting is both cheap and
timely. Startup is too early — the pool that filled the disk did not exist
when the server booted — and a sweep at run end is too late, because the
failures that fill it are exactly the runs that never reach a clean exit.

## What it reclaims

The bound takes a worktree only when **nothing at all is lost**: every
commit in it is held by a ref in the parent repository that outlives the
directory, and there is nothing uncommitted in the tree.

It shares the classifier with `iterion clean` — `pkg/worktreepool` — so
there is no second, weaker set of rules. What differs is the *admission*,
because the two are asking different questions of the same facts:

| | `iterion clean` | the pool bound |
| --- | --- | --- |
| the question | what is the operator willing to lose | what can be taken with nothing lost |
| `merged`, clean | conservative | **taken** |
| `own-branch` held by a branch/tag/remote, clean | aggressive | **taken** |
| `own-branch` held only by `refs/iterion/…` | aggressive | refused |
| anything dirty | moderate (merged) / aggressive | refused |
| gitignored content | selected level | refused |
| `orphan` | aggressive | refused |
| `unlanded`, `nested-repo` | never | refused |
| a live run's worktree | never | refused, and not counted |
| a resumable run's worktree | `--include-resumable` | refused |

The row that makes the bound bite is the second one. A run that failed
before committing anything leaves a checkout parked at a commit its own
branch already points at: deleting it loses nothing, and `iterion clean
--level conservative` still spares it as work nothing has *adopted*. That
distinction is right for a command an operator drives and wrong for a
bound whose only job is to stop the disk filling — which is why the bound
carries its own predicate rather than a fourth level.

## What it will not do

**It never refuses to create a worktree.** The run in front of it was
asked for; failing it over some other run's leftovers would be a
limitation nobody chose.

**It never deletes uncommitted or gitignored content.** "Preserved for
inspection" is a feature, and an eviction nobody asked for must not be
what destroys it. Ignored paths may be generated output, but they may also
be an operator-owned `.env`; the automatic bound leaves that distinction
to an explicit `iterion clean` dry run.

**It never breaks a resume.** `iterion resume` restarts a
`failed_resumable`, `cancelled`, `paused_operator` or
`paused_waiting_human` run *in that very checkout*. On a long-lived store
these are usually most of what accumulates. Giving up a terminal resume is
the operator's call, made with `iterion clean --include-resumable`;
non-terminal pauses remain protected and are named separately in the
warning.

When those refusals leave the pool over budget, the bound says so, once
per launch:

```
runtime: worktree pool: 12 parked worktrees in /path/.iterion/worktrees
exceed the budget of 8 (3 live worktrees excluded); 3 carry uncommitted
work, 9 belong to runs `iterion resume` would restart. Review them
with `iterion clean --store-dir /path/.iterion --older-than 0 --level moderate
--include-resumable` (add --apply to delete). Raise or lift the budget
with ITERION_WORKTREE_POOL_MAX=<n> (`off` disables it).
```

The suggested command is a **dry run** — `iterion clean` reports by
default — and its flags are derived from what is actually blocking, so it
is the line that would work on your pool rather than a generic one. When
the only things left are `unlanded` or `nested-repo`, no command is
offered: no level of `iterion clean` takes those either, and that pool
needs git, by hand.

## The budget

| Variable | Effect | Default |
| --- | --- | --- |
| `ITERION_WORKTREE_POOL_MAX` | How many worktrees **no live run owns** a store may park before the bound reclaims. `off` / `none` / `0` disables it. | `8` |

Eight is chosen against what a worktree costs rather than what feels tidy.
On a repository that vendors its dependencies, eight of them is already a
few gigabytes — set it from what you can afford, not from how many runs
you expect. A malformed value is reported and **disables** the bound
rather than silently reverting to the default: an operator who typed
`ITERION_WORKTREE_POOL_MAX=20GB` should not be left believing they raised
a ceiling that is still at 8.

There is deliberately **no DSL field and no CLI flag**. The pool belongs to
the *store*, not to any one workflow or run — letting a `.bot` set it
would let one bot decide how much disk every other bot's leftovers may
hold.

## Cleaning up by hand

The bound is a floor under the problem, not a replacement for the command:

```sh
iterion clean                                    # dry run, conservative, this project
iterion clean --apply
iterion clean --level moderate --include-resumable --apply
iterion clean --all-projects --older-than 720h   # every project's store
```

Full reference, including every landing class and guard:
[cli-reference.md#iterion-clean](cli-reference.md#iterion-clean).

## Do not put a store on a tmpfs

The incident was a leak; what turned it into a machine outage was
`--store-dir /tmp/...` on a host where `/tmp` is a 16 GB tmpfs — so the
worktrees were RAM. A store holds run records, worktrees, workspace
objects and scratch, all of which are meant to survive a reboot. Point
`--store-dir` at real disk; the same rule is why dogfood runs belong in
the operator's own `.iterion`, never a throwaway path.

## See also

- [cli-reference.md#iterion-clean](cli-reference.md#iterion-clean) — the
  operator-driven sweep and its safety rules.
- [environment-variables.md](environment-variables.md) —
  `ITERION_WORKTREE_POOL_MAX` alongside `ITERION_SCRATCH_RETENTION`, the
  other automatic reclamation.
- [resume.md](resume.md) — why a resumable run still owns its checkout.
