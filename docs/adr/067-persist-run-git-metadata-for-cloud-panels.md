# ADR-067 — Persist run git metadata to the store so cloud Commits/Files panels survive the runner pod

- Status: accepted
- Date: 2026-07-10
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

In cloud mode the studio's run **Commits** and **Modified files** panels
rendered their neutral empty states ("Not a git repository" / "No working
directory") for every run. The handlers (`GET /api/runs/{id}/commits`,
`/api/runs/{id}/files`) inspect the run's working directory to shell `git
log` / `git diff` — but they run on the **server** pod, while a cloud run's
worktree is an ephemeral clone in the **runner** pod's filesystem that is
`os.RemoveAll`-ed the moment the run returns. The server pod has no repo to
inspect and, unlike the local worktree-finalization path, no persistent
storage branch it can reach either (the commits live only on the forge the
runner pushed to). Observed live 2026-07-10 on branch-improve-loop run
`019f4ccb`, which pushed two real commits the panel could not show.

The existing handlers already distinguish live from historical data via the
`available`/`reason` wire shape and an `historicalRefs` path that replays
`BaseCommit..FinalCommit` from a local storage branch — but that path is
inherently local (it needs a resolvable repo root on the serving process),
so it never fires in cloud.

Alternatives considered:

1. **Fetch from the forge on demand** (GitHub/GitLab commits API). Rejected
   as the *primary* source: it only works when a push happened, requires a
   live forge token on the server pod, adds latency and rate-limit coupling
   to a read-only panel, and cannot represent a run that committed but did
   not push. Kept as a possible best-effort enrichment for diff *content*.
2. **Stream the whole worktree/patch to the store.** Rejected: the panels
   need commit + file *metadata*, not the tree. Persisting full patch text
   unbounded would bloat Mongo documents (the 16 MB BSON ceiling) exactly
   like raw command output would.
3. **Persist git metadata into the run store while the run executes, and
   fall back to it when the workdir is absent.** Chosen.

## Decision

Add an optional `store.RunGitMetaStore` seam — mirroring the shape of
`RunLogStore` / `PlanStore` — that persists a whole-snapshot `RunGitMeta`
per run: the commit list over `(base, head]`, the modified-files diffstat
vs the baseline, and each commit's introduced file list. Both backends
implement it: `FilesystemRunStore` with `runs/<id>/gitmeta.json`, the Mongo
store with a `run_gitmeta` collection (one upserted document per run,
unique `run_id`).

`store.BuildRunGitMeta(repoDir, base)` is the shared producer (reusing the
`pkg/git` primitives `Log` / `StatusBetween` / `ShowCommit`). The cloud
**runner** captures the clone HEAD as the baseline *before* the workflow
runs, then after the run — before its `os.RemoveAll` wipes the clone —
computes and persists the snapshot. Recording is best-effort on a
tenant-scoped background context so a cancelled/failed run still banks its
final view; it never changes the run's outcome. Because it fires on every
terminal outcome (finished/failed/paused), a paused cloud run also shows
its commits-so-far, and a resume re-clones and overwrites the snapshot.

The server handlers (`handleListRunCommits`, `handleListRunFiles`, and the
commit-detail endpoint) keep the **live worktree path primary** — local and
studio runs still read on-disk truth, including uncommitted state — and
consult the persisted snapshot only when the worktree is absent, ahead of
the local `historicalRefs` path (which can never resolve on a cloud server
pod). The persisted snapshot is the committed branch range, so a strict
`?mode=uncommitted` request still degrades to `worktree_gone`; every other
mode (default/branch/combined) is answerable from it, and a run that made no
commits serves the "no commits" state, not an error.

Diff **content** (patch text) is deliberately out of scope for this change:
the per-commit file *list* is served from the snapshot's `CommitFiles`, but
fetching or persisting the actual patch bytes is left as a follow-on
(bounded persistence like the run-log chunks, or a forge fetch when a push
happened) — metadata first, content best-effort.

## Consequences

- A finished (or failed/paused) cloud run whose runner pod is gone renders
  its commits (sha/author/message/diffstat) and modified-files list.
- Local runs are unchanged: the live git path stays primary and still shows
  uncommitted state; local finalized runs still use `historicalRefs`. Local
  runs never populate the snapshot (only the cloud runner does), so there is
  no divergence between the two sources in practice.
- The Mongo `run_gitmeta` collection grows one small document per repo-bound
  run; it has no TTL yet (the run document is the lifecycle anchor —
  `DeleteRun` should be extended to drop it when run deletion is wired for
  the cloud store). The document is bounded by the commit/file *count*, not
  patch size.
- The `RunGitMetaStore` seam is the extension point for the deferred
  diff-content work and for any future consumer (e.g. a report generator)
  that needs a run's git activity without a worktree.

### Update 2026-07-12 — the still-running gap gets a "building" empty state

This ADR closes the gap for a *finished* cloud run (the snapshot is written
at finalize by `recordRunGitMeta`). While a cloud run is still **running**,
neither the worktree nor the snapshot exists on the server pod, so the
files handler fell through to `not_git_repo` / `worktree_gone` — the very
error states this ADR set out to replace, just at a different point in the
lifecycle. `handleListRunFiles` now returns reason **`building`** for a
non-terminal run whose worktree dir is absent (`unavailableReason`), and
the studio `FilesPanel` renders an "available when the run finishes" hint
instead. So the panel reads coherently across the whole cloud lifecycle:
*building* while running → the persisted snapshot once finalized.
