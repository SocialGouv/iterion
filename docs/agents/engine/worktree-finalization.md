# Worktree finalization (`worktree: auto`)

Read it when a run's workspace finalization misbehaves, or you touch `finalizeWorktree`.

### Worktree finalization (`worktree: auto`)

When a workflow declares `worktree: auto`, the engine creates a fresh git
worktree at `<store-dir>/worktrees/<run-id>` and runs all nodes inside it
(see `pkg/runtime/worktree.go`). On a clean exit, `finalizeWorktree`:

1. Reads the worktree's HEAD. If unchanged, no-op (the run made no commits).
2. **Always** creates a persistent branch on that HEAD (default
   `iterion/run/<run id>`, overridable via `--branch-name`). This
   is the GC guard — without it the commits would only be reachable via
   reflog and eligible for `git gc` after ~30 days.
3. **Best-effort** merges the run's commits into the user's
   currently-checked-out branch — by default as ONE **squash** commit
   (title = the first commit's subject, body = the per-commit
   `- <sha> <subject>` list); `--merge-strategy merge` fast-forwards
   instead (preserves history). Skipped — with a warning logged — if any
   guard fails (dirty working tree, branch switched mid-run, non-FF,
   detached HEAD at start), and never attempted for a wip-banked HEAD
   (unreviewed output stays on the storage branch only).
4. Removes the worktree directory.

The result is persisted on `run.json` as `final_commit`, `final_branch`,
`merged_into`, `merged_commit` (differs from `final_commit` under
squash), `merge_status` (`pending|merged|skipped|failed`) and surfaced
in the studio RunHeader so the user always knows where the run's
commits landed.

Override flags (CLI + studio Launch modal + HTTP API):
- `--merge-into <target>` — `current` (default), `none` (skip merge,
  branch only), or a branch name (must match currently-checked-out)
- `--merge-strategy squash|merge` — squash (default) vs fast-forward
- `--auto-merge` — merge synchronously at run end (CLI default true;
  the studio defaults to false and defers the merge to a UI action,
  leaving `merge_status: pending`)
- `--branch-name <name>` — override the storage branch (default
  `iterion/run/<run id>`); on collision a numeric suffix is added

On error, the worktree is preserved at `<store-dir>/worktrees/<run-id>`
for inspection and finalization is skipped — the operator decides what
to do with any partial commits.

