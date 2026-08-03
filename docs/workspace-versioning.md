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
<store>/runs/<run>/workspace/
  objects/<aa>/<sha256 remainder>   file contents, deduped by hash
  snapshots/<id>.json               manifest: parent + path→hash entries
  labels.json / index.json          labels and the stat cache
```

Snapshots carry a `Parent`, so a run's captures form a chain — the node-by-node
history of the workspace, readable without git. Labels name the boundaries:
`pre:<node>:<iter>` and `post:<node>:<iter>`. A rewind resolves the `pre:` label
of its pivot.

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

Files above 32 MiB are recorded in `Snapshot.Skipped` rather than captured, and
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

This matters most for an in-place run, where the workspace is your live
checkout: the deletion pass cannot tell a file a node created from one you
wrote in your editor while the run was paused. If a restore swept up your own
work, the bank is where it went.

## Limits

Symlinks are not captured (following them can escape the workspace, and
restoring one would replace a link with a regular file). Files outside the
workspace are out of scope, as are external effects — board cards, forge
comments, pushed commits. A capture beyond 50 000 files refuses rather than
grinding through a tree it was never meant to version.
