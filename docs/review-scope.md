# 🔍 What a review gate shows

A human gate approves work. The question it has to answer for the operator is
*which* work — and the answer that holds up is **everything the run changed
since the previous gate**.

## Why the range, and not a declared list

The obvious design is to let the gate name the nodes it covers
(`review_nodes: [implement, write_docs]`). It does not survive contact with the
graph:

- A per-node range needs per-node boundary refs, and only nodes that go through
  the main executor path record them. Subbots, fan-out branches, `compute`,
  routers and `emit`/`wait` record none — and a pipeline delegates its
  implementation to subbots.
- A declared list is a thing an author maintains. Forget to add a node and the
  review silently misses its work. For a panel whose output is an *approval*,
  a silent omission is a defect, not an approximation.

A gate-to-gate range is a **workspace before/after**. Nothing a run did can fall
outside it — not a subbot's writes, not a parallel branch's, not a `Bash`
command's. Attribution to individual nodes becomes presentation on top of a
complete set, and whatever cannot be attributed is shown under *Other changes*
rather than dropped.

## How the range is anchored

Every human pause takes a **real capture** of the workspace rather than
aliasing the last node boundary: the node before a gate may have been specially
dispatched, in which case the remembered anchor was deliberately invalidated
and aliasing would miss that node's work.

```
gate/0 ─── implement ─── (subbot) ─── write_docs ─── gate/1
   ▲                                                    ▲
   base of the range                       what the reviewer approves
```

Two backends, same contract:

| Run mode | Gate anchor | Diff |
|---|---|---|
| `worktree: auto` | git ref `refs/iterion/runs/<id>/gate/<seq>` | `git diff` between refs |
| **in-place (default)** | workspacetrack label `gate:<seq>` | path→hash compare of snapshots |

Gate 0's base is the run's start state: `BaseCommit` on a worktree run, the
root of the snapshot chain on an in-place run. Grouping uses the per-node
`pre:`/`post:` boundaries where they exist, with the latest boundary winning
when two nodes touched the same file — the reviewer cares who left it in the
state under review.

A worktree node that leaves the tree clean (a bot committing in stride) writes
no snapshot commit, so its `post` ref is anchored at `HEAD` instead. Without
that fallback `pre..post` would not resolve for exactly those nodes.

In-place runs honour **`.iterionignore`** (falling back to `.gitignore`), so a
media pipeline can version `runs/**/exports/*.mp4` without forcing those paths
into git. See [workspace-versioning.md](workspace-versioning.md).

## API

```
GET /api/runs/{id}/review/scope[?gate=N]   → range + groups + total_files
GET /api/runs/{id}/review/diff?path=…[&gate=N]  → one file's before/after
```

Refs / snapshot ids are resolved server-side from the gate number, never taken
from the caller.

`available: false` always carries a `reason` in the operator's terms. A panel
that shows nothing without saying why is worse than no panel.

When a run is `paused_waiting_human` but predates `gate:<seq>` labels, the
panel falls back to "everything since the first workspace capture" so a
mid-flight upgrade does not strand the operator with an empty range.

## What it cannot show

| | |
|---|---|
| Paths excluded by `.iterionignore` / `.gitignore` (in-place) | the tracker never captured them |
| Gitignored files on a **worktree** run | `git add -A` honours ignore rules |
| Binary / oversized files | flagged, not rendered (5 MiB blob cap on the per-file diff) |
| Cloud runs without a surviving store | the runner's clone is recycled and the snapshots die with the pod |
| A node resumed after failure | its boundary is re-written against the post-failure tree, so the group shows the last attempt |

Later nodes' edits to the same file are excluded from an earlier node's group by
construction — the panel shows the file *as of that node*, not as it will merge.
The gate still merges the whole branch (worktree path).
