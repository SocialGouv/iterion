# Worktrees — where an interactive session changes files

**Read it when** you are about to change anything in this repository — an
edit, a commit, a branch — from an interactive session. The rule itself is in
[AGENTS.md](../../AGENTS.md); this page is the mechanics and the traps.
An iterion bot run is out of scope: the engine owns its workspace.

Several agent sessions (Claude Code, Codex, pi …) and the operator share one
machine and one primary checkout. That checkout belongs to the operator and
stays on `main`: a session never edits, checks out, pulls, stashes or resets
there unless the operator asks. Read-only work may run there.

## Start a task

```sh
git fetch origin main
git worktree add --no-track -b <type>/<slug> .claude/worktrees/<slug> origin/main
```

- `.claude/worktrees/` is git-ignored and is where Claude Code's own worktree
  tooling puts its worktrees. Never put one under `/tmp`: it is wiped, and
  uncommitted work goes with it.
- `--no-track` is required in a sandboxed session. The Claude Code sandbox
  mounts `.git/config` read-only and plants an empty `.git/config.lock`, so
  recording an upstream fails. Without the flag, `worktree add -b` fails and
  leaves the new branch behind, with no worktree (measured 2026-10-01).
- **Continuing an existing branch** — a PR branch, say:
  `git worktree add .claude/worktrees/<slug> <branch>` when the branch exists
  locally; otherwise
  `git worktree add --no-track -b <branch> .claude/worktrees/<slug> origin/<branch>`.
  The short form on a remote-only branch creates a tracking branch, and so
  hits the same config refusal. If git refuses with `already used by
  worktree at '<path>'`, that worktree holds the branch (one mid-rebase shows
  as `(detached HEAD)` in `git worktree list`, so trust the refusal, not a
  grep). Work there if it is yours for this task. If its directory is gone
  (`prunable`), `git worktree remove <that path>` drops only that
  registration and frees the branch. If it is the primary checkout, or
  another session's live worktree (`locked`, or its ticket claimed), ask the
  operator.

## Am I already in one?

```sh
git rev-parse --path-format=absolute --git-dir --git-common-dir
```

Two different paths: you are in a linked worktree. Stay there if it was made
for this task. Do not work in a worktree the engine owns — any
`<store>/worktrees/<run-id>` or `<store>/dispatcher/workspaces/…`, wherever
the store lives (`.iterion/` or `~/.iterion/projects/<key>/`): it belongs to
the run. Without
`--path-format=absolute`, the two answers also differ from any subdirectory
of the primary checkout, so the test says "linked" where it is not.

## A fresh worktree

- **devbox.** The first `devbox run` in a new worktree downloads its
  environment from `codeload.github.com` and `cache.nixos.org`. A sandboxed
  session must be allowed to reach both, otherwise the setup aborts with a
  `403` on the nixpkgs tarball.
- **The run store.** Every `iterion` command that reads or writes it — `run`,
  `resume`, `inspect`, `report`, `rewind`, `issue …`, `secret`,
  `connections` — resolves it from the working directory. From a worktree it
  silently lands in `~/.iterion/projects/<worktree-key>/`: an `issue import`
  succeeds onto a board the operator's dispatcher never reads. Pass the
  primary checkout's store, the one the operator's studio and dispatcher
  read, to each:
  `--store-dir "$(dirname "$(git rev-parse --path-format=absolute --git-common-dir)")/.iterion"`
  ([dogfood.md](dogfood.md)).
- **Sibling clones** (`.works/claw-code-go`, `.repos/`) exist in the primary
  checkout only. Reach them through the same expression;
  `scripts/bump-claw.sh` does.

## Traps (measured on this repo, 2026-10-01)

- **A branch held by another worktree.** Git refuses a plain `checkout`,
  `switch` or `worktree add`. `--ignore-other-worktrees` and `worktree add -f`
  bypass that refusal under both the host git 2.43 and the pinned git 2.55.
  `checkout -B` and `switch -C` also bypass it under 2.43; 2.55 refuses them.
  Never bypass the refusal; see "Continuing an existing branch" above.
  Two worktrees on
  one branch then disagree: a commit in one shows up as a staged revert in
  the other. Never check out `main` in a linked worktree.
- **`git worktree prune`.** It drops the registration of every worktree whose
  path is not visible from where it runs: another mount, a sandbox, a
  container. When the path comes back, that checkout says "not a git
  repository", and another worktree can take its branch. Never run it.
- **Removing your worktree.** Once its branch is merged, run
  `git worktree remove <path>`. It refuses tracked or untracked changes but
  deletes ignored content, **including a worktree nested in an ignored
  directory, with that worktree's uncommitted work**. Check
  `git worktree list | grep -F "<path>/"` first; the repository already has
  worktrees nested under `.claude/worktrees/*/.iterion/scratch/`.
