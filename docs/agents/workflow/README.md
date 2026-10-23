# Workflow — how work reaches `main`

| Leaf | Read it when |
|---|---|
| [worktrees.md](worktrees.md) | Before changing a file from an interactive session: the worktree default, the in-a-worktree test, the store commands, the git traps. |
| [review-and-merge.md](review-and-merge.md) | Opening, merging or unblocking a PR: queue, Revi gate, parked gate. |
| [push-to-main.md](push-to-main.md) | Preparing a fresh worktree and landing an operator train: peer notice, full local gate, review, trailers and CI witnesses. |
| [adversarial-review-loop.md](adversarial-review-loop.md) | Before any push: why the round is required, budget, who pays, trailers. |
| [billy.md](billy.md) | The paused fixer lane, and its re-arm procedure. |
| [release.md](release.md) | Floors, mirrors, refused cuts, the changelog pipeline. |
| [dogfood.md](dogfood.md) | Launching a catalog bot against this repo for real. |

Board mechanics live in [../../board-epics.md](../../board-epics.md); the
work-tracking contract is the root [AGENTS.md](../../../AGENTS.md).

After a change that affects generated artifacts, STAGE your doc additions and deletions first — the generators read the index (`git ls-files`), so an unstaged page is invisible to them — then run `task gen`, then commit the artifacts; `task gen:check` checks freshness (the reading golden stays review-only at the release cut via `cmd/corpus-reading --cut`).
