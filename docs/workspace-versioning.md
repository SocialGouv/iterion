# 🗂️ Workspace versioning

A node's real work is often not in its output. A docs bot writes dozens of
files and returns a summary; a coding bot writes source and returns a verdict.
Replaying such a node without putting its files back replays it *on top of its
own previous production*, which is a different experiment from the one the
operator meant to run.

`pkg/workspacetrack` versions the workspace so [`iterion rewind`](resume.md#rewind-resume-from-an-earlier-node)
can undo that work.

## Why iterion owns it rather than delegating to git

Per-node git snapshots already exist and are used for runs that declare
`worktree: auto`. They cannot be generalised:

- **The default run has no worktree.** It executes in place, so the workspace
  *is* the operator's checkout. Capturing it with `git add -A` would stage
  their own uncommitted work as a side effect of running a bot. This is not a
  gap to fill later — it is a reason git cannot be the mechanism there.
- **`.gitignore` is a packaging policy, not a recovery policy.** A repository
  ignores `dist/` because it should not be committed, not because iterion
  should be unable to undo writing it.
- **Cloud runs skip worktree isolation entirely**, so they have no snapshots
  at all.
- **Namespace and lifecycle.** `refs/iterion/**` lives in the user's
  repository, outside `iterion runs prune`, `memory export`, and the cloud
  store. Every other durable per-run artefact — events, artifacts,
  interactions, turns — is iterion-owned.

## Layout

Everything lives beside the run's other state, never inside the project:

```
<store>/workspace-objects/<aa>/<sha256 remainder>   file contents, deduped by hash
<store>/runs/<run>/workspace/
  snapshots/<id>.json               manifest: parent + path→hash entries
  index.json                        stat cache + labels + head
```

The object pool is **store-global**, not per-run: content is content, and a
per-run pool re-stores the whole workspace for every run (measured: 318 MiB per
run on this repo). That is also why deleting a run cannot blind-delete its
objects — see *Reclaiming disk*.

Snapshots carry a `Parent`, so a run's captures form a chain — the node-by-node
history of the workspace, readable without git. Labels name the boundaries:
`pre:<node>:<iter>`, `post:<node>:<iter>`, `fail:<node>:<iter>` and
`gate:<n>`. A rewind resolves the `pre:` label of its pivot for the state to
restore, and the newest boundary label for how far the run got.

`fail:` is written when a node's execution does **not** complete — a failure,
an interruption, an operator cancel. It is deliberately not `post:`, which two
consumers read as "the node completed that iteration" (the rewind's staleness
guard and the review panel's per-node attribution). But the files a dying node
wrote are real, and without a boundary recording them nothing downstream can
tell them from the operator's: `pre:` is an *alias* and does not advance the
chain head, so a run that stops inside a node would otherwise end with its most
recent recorded state being the one that node started from.

## Cost

The expensive part is never storing blobs, it is knowing what changed without
re-reading everything — which is what git's index buys. The tracker keeps the
equivalent stat cache (`path → size, mtime, hash`), so only files whose stat
moved are re-hashed. The first capture of a run pays a full hash; later ones
pay a directory walk plus the changed files. The `pre:` marker costs nothing at
all: nothing touches the workspace between one node finishing and the next
starting, so it is a label pointing at the previous capture.

## What is excluded

`.git/` and `.iterion/` always — the first is a database, the second is the
store itself, and capturing it from inside a run would have the tracker record
its own objects without bound. Then dependency and build trees
(`node_modules`, `.venv`, `.next`, …), which are heavy and regenerable.
`vendor/` is deliberately **kept**: in Go projects it is committed source a bot
may legitimately edit.

Beyond that, a project states iterion's rules in **`.iterionignore`**, falling
back to `.gitignore` when absent. The fallback is pragmatic; the precedence is
the point. A project can control what iterion versions without changing how it
packages itself for git.

Rules apply in file order and the **last match wins**, `**` spans any number of
path segments, and a `!` negation re-includes **even when a parent directory is
excluded** — which git refuses to do, and which is what makes an allowlist
expressible. That shape is what a media pipeline needs: none of `runs/`, except
the delivered files.

```
runs/**
!runs/**/exports/*.mp4
!runs/**/audio/*.mp3
!runs/**/music/*.wav
runs/_archived/**
```

The cost of allowing this is that an excluded directory can no longer be pruned
from the walk once any negation exists — the walk descends and tests each entry.
Stat-only, no hashing: measured at 409 ms over a 13 GB / 14k-file tree.

Files above 32 MiB — raise it with **`ITERION_WORKSPACE_MAX_FILE_MB`** — are
recorded in `Snapshot.Skipped` rather than captured, and
a restore **never deletes a skipped path** — it was there and we chose not to
store it, so removing it as "absent from the snapshot" would destroy data the
tracker has no copy of. Coverage gaps are always reported, never silent.

## Recovering from a restore

A rewind banks the workspace before it restores, so the state you had at that
instant stays reachable:

```bash
iterion rewind --run-id RUN --list-snapshots      # newest first, with labels
iterion rewind --run-id RUN --restore-snapshot <id>
```

Every label a snapshot carries is listed, not only the one its manifest was
created under — a bank routinely lands as a second label on an existing
snapshot (an unchanged workspace dedupes against its parent), and a listing
showing only `pre:`/`post:` rows would leave you unable to tell which id is
your bank.

`--restore-snapshot` is **full-tree by design**, unlike the rewind that
produced the bank: "put my workspace back" cannot be served by a partial
answer. It banks the current state first, so it is itself undoable — but it is
the larger operation of the two, and the CLI says so at the point of use.

This matters most for an in-place run, where the workspace is your live
checkout. A rewind now bounds itself to what the run *recorded* changing
(`--restore-scope`, see [resume](resume.md#how-much-comes-back---restore-scope)),
which removes most of the exposure — but not all of it: an edit you make while
the run is **paused** is swallowed by the next node's capture and becomes
indistinguishable from that node's output. If a restore swept up your own work,
the bank is where it went.

## Reclaiming disk

The object pool is shared across runs, so deleting a run cannot blind-delete
its content — `PruneObjects` marks every hash still named by a surviving
manifest and sweeps the rest. It refuses to delete anything if a manifest is
unreadable: a partial mark set is exactly how content that is still referenced
gets removed.

## Limits

Symlinks are not captured (following them can escape the workspace, and
restoring one would replace a link with a regular file). Files outside the
workspace are out of scope, as are external effects — board cards, forge
comments, pushed commits. A capture beyond 50 000 files refuses rather than
grinding through a tree it was never meant to version.
