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

Every human pause writes `refs/iterion/runs/<run>/gate/<seq>` (see
`ReviewGateRef`). The gate takes a **real capture** rather than aliasing the
last node boundary: the node before a gate may have been specially dispatched,
in which case the remembered anchor was deliberately invalidated and aliasing
would miss that node's work.

```
gate/0 ─── implement ─── (subbot) ─── write_docs ─── gate/1
   ▲                                                    ▲
   base of the range                       what the reviewer approves
```

Gate 0's base is the run's `BaseCommit`. Grouping uses the per-node
`pre:`/`nodes:` refs where they exist, with the latest boundary winning when two
nodes touched the same file — the reviewer cares who left it in the state under
review.

A node that leaves the tree clean (a bot committing in stride, i.e. the
best-behaved case) writes no snapshot commit, so its `post` ref is anchored at
`HEAD` instead. Without that fallback `pre..post` would not resolve for exactly
those nodes.

## API

```
GET /api/runs/{id}/review/scope[?gate=N]   → range + groups + total_files
GET /api/runs/{id}/review/diff?path=…[&gate=N]  → one file's before/after
```

Refs are resolved server-side from the gate number, never taken from the
caller: they become git arguments.

`available: false` always carries a `reason` in the operator's terms. A panel
that shows nothing without saying why is worse than no panel.

## What it cannot show

| | |
|---|---|
| Gitignored files | `git add -A` honours ignore rules, so a generated artifact under an ignored path is invisible |
| Binary / oversized files | flagged, not rendered (5 MiB blob cap) |
| In-place runs | `interaction: review` is a compile error without `worktree: auto`, so the range machinery is worktree-only |
| Cloud runs | the runner's clone is recycled and the refs die with the pod |
| A node resumed after failure | its boundary is re-written against the post-failure tree, so the group shows the last attempt |

Later nodes' edits to the same file are excluded from an earlier node's group by
construction — the panel shows the file *as of that node*, not as it will merge.
The gate still merges the whole branch.
