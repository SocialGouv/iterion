# ADR-016: In-run worktree file editor — handler-layer write reusing one audited path boundary, live-worktree-only

- **Status**: Accepted
- **Date**: 2026-06-03
- **Authors**: devthejo
- **Code**: [pkg/server/runs_files.go](../../pkg/server/runs_files.go)
  (`handleGetRunFileContent`, `handleSaveRunFileContent`,
  `resolveRunWorktreePath`, `maxRunFileEditBytes`),
  [pkg/server/server.go](../../pkg/server/server.go) (`safePathWithin`,
  factored out of `safePath`),
  [pkg/server/runs.go](../../pkg/server/runs.go) (route registration),
  [studio/src/api/runs.ts](../../studio/src/api/runs.ts)
  (`getRunFileContent`, `saveRunFileContent`),
  [studio/src/components/Runs/FileEditDialog.tsx](../../studio/src/components/Runs/FileEditDialog.tsx),
  [studio/src/components/Runs/FilesPanel.tsx](../../studio/src/components/Runs/FilesPanel.tsx)
  (`LargeChangesetHint` "Edit .gitignore"),
  [studio/src/components/Runs/FileDiffDialog.tsx](../../studio/src/components/Runs/FileDiffDialog.tsx)
  ("Edit" affordance)

## Context

A 2026-05-28 dogfood run produced a worktree with an un-gitignored
`.tmp-gocache` (22k files) that flooded the diff panel. The operator
wanted to fix `.gitignore` and patch a file inline, without dropping to a
terminal. The studio already ships Monaco read-only (`FileDiffDialog`'s
`DiffEditor`), so the ask was to add an *editable* tab plus a one-click
"Edit .gitignore" from the large-changeset banner, backed by file
read/write endpoints scoped to the run's `work_dir`.

Two facts shaped the design:

1. The studio already has a **single audited path-traversal boundary** for
   its own workdir Save: `safePath` in `server.go`, which does
   symlink-aware containment (`evalSymlinksLongestPrefix` + `pathContains`)
   — exactly the boundary the `runs_files.go` gosec path-traversal finding
   is about. But `safePath` is hardcoded to `s.cfg.WorkDir` (the studio's
   one editor workdir), **not** a per-run worktree.
2. The natural precedent for a *run-worktree-scoped write* is the
   merge-conflict resolver: `handleResolveMergeConflict` →
   `Service.ResolveMergeConflictFile` (in `pkg/runview`), which does the
   store lookup + write inside the runview service and stages the result
   through git.

## Decision

### 1. One audited path boundary, reused at the handler layer

Factor the body of `safePath` into `safePathWithin(base, relPath)` and have
`safePath` call it with `s.cfg.WorkDir`. The new run-file handlers call
`safePathWithin(run.WorkDir, path)`. Request input is validated twice:
`gitlib.ValidateRelPath` for a fast 400 on obviously-bad input, then
`safePathWithin` for the real symlink-aware containment check before any FS
access. The path can never escape `run.WorkDir`, and the symlink-escape
case (a link *inside* the worktree pointing out) is caught by the same
resolution `safePath` already uses.

This **diverges from the plan's first instinct** (put read/write in the
runview service, mirroring `ResolveMergeConflictFile`). The deciding factor
is that the security boundary — the part a reviewer must trust and the part
gosec flags — should have exactly **one** implementation. Re-implementing
symlink-aware containment inside `pkg/runview` would create a second copy
to keep in lock-step; a future fix to one would silently miss the other.
The read/write themselves are a plain `os.ReadFile`/`os.WriteFile` once the
path is contained, so there is little service-layer logic to host.

### 2. Editing is live-worktree-only

Both endpoints require `run.WorkDir != ""` **and** `dirExists(run.WorkDir)`,
returning `409 Conflict` otherwise. A finalized/gc'd run has no on-disk
worktree; its committed state lives on the persistent storage branch. We do
**not** try to edit through the branch (that would mean a checkout + commit,
a different and far heavier operation). The studio mirrors this: the "Edit
.gitignore" button is hidden when `worktree_gone`, and the diff dialog's
"Edit" affordance only appears for a non-binary, non-deleted path.

### 3. Direct worktree write, no locking, no git staging

`handleSaveRunFileContent` does `os.WriteFile(abs, content, 0o644)` straight
into the worktree — unlike the merge-conflict path it does **not** `git
add`/stage. The edit is just a working-tree change the run's normal commit
flow (or the operator's review) picks up. There is no optimistic-concurrency
token and no lock: a single local operator is the assumed model. A 4 MiB cap
(`maxRunFileEditBytes`) bounds both read (protect Monaco/the browser) and
write (bound the request) — the same large-blob pathology that motivated the
feature.

## Trade-offs

| Dimension | Handler-layer write + `safePathWithin` (chosen) | runview service method, mirror `ResolveMergeConflictFile` |
|---|---|---|
| Path-traversal boundary | one impl, shared by Save + run editor | second copy in `pkg/runview` to keep in sync |
| gosec finding surface | single place to fix/annotate | two |
| Service-layer cohesion | low (plain read/write, little logic) | higher, matches merge-conflict precedent |
| Git staging | none — plain working-tree edit | n/a (merge path stages; editor shouldn't) |

| Dimension | Live-worktree-only (chosen) | Edit finalized runs via branch |
|---|---|---|
| Implementation | stat the worktree, 409 otherwise | checkout + commit machinery |
| Mental model | "edit the run as it stands on disk" | "rewrite history of a finished run" |
| Risk | low | high — touches the persistent branch |

## Alternatives considered

1. **Read/write in `pkg/runview` mirroring `ResolveMergeConflictFile`.**
   Rejected: would duplicate the symlink-aware containment boundary, the
   exact code gosec flags and a reviewer must trust. One audited
   implementation beats service-layer symmetry here.
2. **Reuse the existing `/files/diff` `after` field for the editor's
   initial content.** Rejected: diff only covers *changed* files; the
   motivating case is an **unchanged or untracked** `.gitignore` (and
   creating one that doesn't exist yet). A dedicated raw-read endpoint with
   an `exists` flag is needed so the editor can seed a fresh buffer.
3. **Optimistic-concurrency token / file lock.** Deferred: the local studio
   is single-operator; a lost-update guard is complexity without a
   demonstrated need. Noted as a future enhancement if multi-operator cloud
   editing lands.
4. **Make `FileDiffDialog` itself editable (toggle `readOnly`).** Rejected:
   keeps the diff view honestly read-only; editing is a deliberate switch to
   a separate `FileEditDialog` (own dirty-state, Save, Ctrl/Cmd-S), reusing
   the same Monaco/theme/`inferMonacoLanguage` plumbing.

## Consequences

- **Operators can fix `.gitignore` inline** from the large-changeset banner
  and edit any non-binary worktree file from the diff dialog's "Edit"
  button, without a terminal. On save, the `["run-files", runId]` query is
  invalidated so the tree + changeset count refresh immediately.
- **The path-traversal boundary is now shared.** A fix to `safePathWithin`
  covers both the studio Save and the run editor. The `runs_files.go`
  `#nosec G304` annotations point at this single boundary.
- **Editing requires a live worktree.** Finalized/gc'd runs cannot be
  edited; the UI hides the affordances rather than failing on click. This
  pairs with the worktree-GC rule (keep `failed_resumable`/`cancelled`
  worktrees, which remain editable).
- **No lost-update protection.** Two concurrent editors (or an editor racing
  the agent mid-run) can clobber each other; accepted for the local
  single-operator model. The dialog warns when editing — the motivating use
  (`.gitignore`, between/after runs) is the safe path.
- **Phase 2 (mini file-tree + multi-tab VS-Code-lite editor) reuses these
  endpoints unchanged** — only a directory-listing endpoint and the
  `EditorTabsView` wiring remain.

### Update 2026-07-12 — the "hides the affordances rather than failing on click" consequence is now actually enforced

The consequence above ("the UI hides the affordances rather than failing on
click") was only half-true: `FilesPanel`'s "Edit .gitignore" was gated on
the files-response `worktree_gone` reason, but `FileDiffDialog`'s "Edit
file" button was wired unconditionally, so on a **cloud run** (worktree on
the runner pod) or a finalized/gc'd local run it would still 409. The
files-response reason was also the wrong signal — it's only populated once
the panel fetches, and doesn't cover the cloud "worktree never on this pod"
case cleanly.

Fix: the run detail now carries a server-computed **`worktree_available`**
boolean (`runview.RunHeader`, mirroring `resolveRunWorktreePath`'s own gate
— `WorkDir != "" && dirExists(WorkDir)`). `RunView` derives one
`onEditFile` handle from it and passes it (or `undefined`) to **both**
entry points, so every editor affordance is uniformly absent when the
endpoint would 409. This is the single, authoritative gate the earlier
per-affordance heuristics only approximated.
