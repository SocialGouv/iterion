# ADR-068 — Persist per-file diff *content* so cloud file/commit diff clicks resolve

- Status: accepted
- Date: 2026-07-12
- Deciders: jo (direction), Claude (analysis + implementation)

## Context

[ADR-067](067-persist-run-git-metadata-for-cloud-panels.md) made the cloud
run **Commits** and **Modified files** panels render by persisting a
`RunGitMeta` snapshot (commit list, modified-files diffstat, per-commit file
lists) from the runner pod before its ephemeral clone is wiped. It
deliberately left diff **content** out of scope: the per-file *list* was
served, but clicking a row to see the actual diff still failed on the server
pod, which has no worktree and (in cloud) no reachable repo:

- `GET /api/runs/{id}/files/diff` → **409** "working directory is not a git
  repository".
- `GET /api/runs/{id}/commits/{sha}/diff` → **404** "commit not in run range".

So the panels populated (looked healthy) but every row errored on click. This
ADR closes that gap — the "deferred diff-content work" ADR-067 named as the
extension point for its `RunGitMetaStore` seam.

## Decision

**Persist rendered before/after content, not unified-diff patch text.** The
studio's `FileDiffDialog` / `CommitDetailDialog` feed Monaco's `DiffEditor`,
which needs the full *before* and *after* sides — not a patch. Serving the
existing `gitlib.DiffPayload` wire shape from the snapshot means the persisted
path and the live git path are byte-identical to the frontend, with zero
frontend forking for the cloud case. A unified patch would have been more
compact for small edits to large files but could not reconstruct the two full
sides Monaco renders, and would have required a second, patch-only render mode
in the studio. Rejected.

**Bounded, with a three-tier storage policy** (the cost of persisting full
sides is real — a vendor bump touches thousands of files):

1. **Inline** in the `RunGitMeta` document for a diff whose combined
   before+after fits the per-file inline cap (128 KiB) *and* the run's
   remaining total budget.
2. **Blob-offloaded** (S3 in cloud, `runs/<id>/gitdiffs/<ref>.json` on the
   filesystem store) for a larger diff within the per-file blob cap (12 MiB),
   via the new `store.RunDiffBlobStore` seam. The Mongo store PUTs it under
   the attachment key layout (`attachments/<id>/__gitdiff/<ref>`) so
   `DeleteRun`'s existing attachment sweep reclaims it, **without** indexing it
   as an operator-visible attachment.
3. **Dropped** (`Truncated=true`, plus a snapshot-level `DiffsTruncated`) once
   the total run budget (48 MiB across inline + offloaded) is exhausted. A
   truncated diff resolves to an `Oversized` placeholder on the wire; the
   studio renders "File too large to display" rather than empty panes or an
   error.

The total budget is the real bound on Mongo-document and S3 growth. The 16 MiB
BSON ceiling is protected by a *cumulative* inline cap (12 MiB across the run,
not merely 128 KiB/file): once it is spent, further sub-128 KiB files are
offloaded to blobs too, so a run touching thousands of small files can never
pile its inline content past the document limit — everything above the inline
budget is a blob reference.

**Producer / consumer split mirrors ADR-067.** The runner's
`recordRunGitMeta` calls `store.PopulateRunDiffs` (reusing `gitlib.DiffBetween`
/ `DiffOfCommit`, which already cap each side at 5 MiB → `Oversized`) right
after `BuildRunGitMeta`, before the clone is wiped, on the same tenant-scoped
background context. The server's `servePersistedFileDiff` /
`servePersistedCommitFileDiff` fall back to the snapshot only when the live
worktree and `historicalRefs` paths cannot serve — the **live path stays
primary**, so local runs are unchanged. The commit-diff fallback re-applies the
same in-range SHA guard (`equalSHA` against the recorded range) the live path
enforces, so a cloud run cannot leak content outside its own history.

## Consequences

- A finished/failed/paused cloud run whose runner pod is gone now shows the
  diff for any file in its Modified-files / commit panels (200, not 409/404).
- Storage is bounded per-file and per-run with an explicit truncation signal;
  a pathological many-file run degrades to placeholders instead of blowing the
  document limit or writing unbounded S3.
- Diff blobs are tenant-scoped (via the attachment key path) and reclaimed by
  `DeleteRun`; they share the run's lifecycle, so TTL parity follows the run
  document exactly as the `RunGitMeta` document does.
- Local runs never populate the diff content (only the cloud runner does) and
  keep reading the live worktree, so there is no divergence between sources.
- The `RunDiffBlobStore` seam is a general run-scoped blob channel; a future
  consumer needing bounded, offloadable per-run bytes (e.g. a persisted report
  asset) can reuse it.
