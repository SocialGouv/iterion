# ADR-064: Worktree finalization requires delegated workspace authority

- **Status**: Accepted
- **Date**: 2026-07-07
- **Authors**: Adry
- **Code**: [pkg/runtime/engine_options.go](../../pkg/runtime/engine_options.go) (`WithWorkDir` → `workDirDelegated`), [pkg/runtime/engine_run.go](../../pkg/runtime/engine_run.go) (promotion gate), [pkg/runtime/worktree.go](../../pkg/runtime/worktree.go) (`finalizeWorktree` `samePath` refusal)

## Context

The worktree finalization path (see the engine's `worktree: auto`
handling) is powerful and destructive-adjacent: on run close it creates
an `iterion/run/*` branch on the worktree HEAD, best-effort
fast-forwards the operator's checked-out branch, and removes the tree.
Two failure modes were observed where that authority was exercised over
a tree iterion did not own:

1. A `worktree: none` run launched via the CLI **from inside a foreign
   linked worktree** (e.g. a Claude Code session's own worktree) was
   promoted to a managed worktree and queued close-time finalization —
   branching, FF-merging, and cleaning a tree iterion never created.
2. A phantom-worktree run (`Worktree=true` but `WorkDir == RepoRoot`)
   could, on cancel, `git add -A && git commit` unrelated operator WIP
   directly onto their live branch.

iterion cannot distinguish "a worktree I was handed" from "a worktree
the operator happens to be standing in" without an explicit delegation
signal — the prior heuristic (promote any linked worktree it finds)
guessed, and guessed wrong on both counts.

## Decision

Worktree promotion and finalization are gated on the workspace being
**explicitly delegated** to the engine, on two independent seams:

- **Delegation signal at entry.**
  [`WithWorkDir`](../../pkg/runtime/engine_options.go) sets
  `workDirDelegated = dir != ""` — a non-empty workspace is one a caller
  (dispatcher per-issue worktree, studio-bound dir) explicitly handed
  over. A defaulted CWD (resolved from `os.Getwd()` at run time) is
  **never** adopted. The promotion path in
  [engine_run.go](../../pkg/runtime/engine_run.go) requires
  `workDirDelegated` before adopting a linked-worktree workspace as a
  managed-worktree baseline.
- **Refuse-at-the-root invariant.**
  [`finalizeWorktree`](../../pkg/runtime/worktree.go) (and the recovery
  path `RecoverFinalize`) refuse outright when `samePath(wtPath,
  repoRoot)` — preserving the tree and warning, instead of running the
  wip-bank commit in the operator's live checkout. Finalize therefore
  only ever commits inside a dedicated, delegated worktree.

The two guards are complementary: the delegation flag stops a defaulted
CWD from *becoming* a managed worktree in the first place; the
`samePath` refusal is the backstop that stops any commit landing on the
operator's branch even if a run's persisted state collapses `WorkDir` to
the repo root.

## Trade-offs

Gating on delegation trades a small amount of convenience — a run
launched from inside a linked worktree no longer auto-branches its
commits — for the guarantee that iterion never branches, FF-merges, or
commits onto a tree the operator owns. The honest concession: the
delegation signal is a **heuristic on CWD provenance** ("was a workspace
handed to us explicitly?"), not a first-class ownership assertion; a
caller that explicitly passes the operator's own checkout via
`WithWorkDir` still grants authority over it.

## Alternatives considered

### 1. Promote any foreign linked worktree (prior behaviour)

Detect that the CWD is a git linked worktree and adopt it as a managed
worktree with finalization authority, regardless of how the run was
launched.

**Rejected because**: iterion cannot tell "a worktree I was handed" from
"a worktree the operator happens to be standing in" without an explicit
delegation signal. The heuristic branched, FF-merged, and cleaned trees
it did not own — the exact bug this ADR closes.

### 2. Rely on the step-5 "never merge a wip-banked HEAD" guard alone

Keep promoting freely and lean on the existing late-stage guard that
refuses to merge a HEAD that was wip-banked at close.

**Rejected because**: for the phantom-worktree case the commit is
**already on the operator's branch** by the time that guard runs — the
wip-bank `git commit` executed in the main checkout. The guard cannot
un-commit; the refusal has to happen *before* the commit, at
`samePath`.

## Consequences

- **Finalize only ever touches a delegated, dedicated worktree.** Both
  the promotion gate and the `samePath` refusal must pass, so no
  branch/FF/commit ever lands on a tree iterion did not create.
- **CLI runs from inside foreign worktrees are safe.** A `worktree:
  none` run launched from a Claude Code session worktree is left exactly
  as found — no branch, no FF, no cleanup.
- **Phantom-worktree cancels preserve, not commit.** When a run's
  `WorkDir` collapses to the repo root, finalize preserves the tree and
  warns instead of committing unrelated WIP onto the operator's branch.
- **Delegation is provenance-based, not asserted.** The gate infers
  authority from "was the workspace passed explicitly?"; there is no
  ownership token, so an explicit `WithWorkDir(operatorCheckout)` still
  grants authority.
- **Re-challenge**: if runs gain an explicit workspace-ownership token /
  manifest, the CWD-delegation heuristic could be replaced by that
  assertion.
