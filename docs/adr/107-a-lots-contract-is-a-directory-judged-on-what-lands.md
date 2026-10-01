# ADR-107 — A lot's contract is a directory, judged on what lands

- **Status**: Accepted
- **Date**: 2026-09-30
- **Applies to**: `bots/modernize` (Morphy) — `plan_read`, `lot_verify`, `lot_gate`, `mark_done`; the `plan-contract` skill it shares with `bots/assessment`; the engine's wip bank (`pkg/runtime`, `finalizeWorktree`)
- **Neighbours**: [ADR-104](104-product-docs-route-table-resolved-once-at-the-run-base.md) (a verdict read from a commit, not from what the judged agent wrote)

## Context

The modernize bot carries one lot of a programme to its exit gate, and writes
`done` itself once the gate, the oracle and the references hold. The party the
contract binds must not be able to rewrite it. The bot refused a lot that
rewrote `plan.yaml`, and only for the keys it listed.

The contract a lot is judged against is wider than the plan:

- the programme's outcomes — each `check` is the command the programme verdict
  runs;
- the owner's brief — goals, decisions, permitted and forbidden changes;
- the arbitration doctrine a separate judge applies to blocked divergences;
- the defects register.

A lot that rewrote an outcome's check to `true` and renamed a brief goal
converged green.

Two more gaps sat in **when** and **on what** the contract was judged:

- **When.** It was judged once, before the exit_gate commands, and those
  commands run the lot's own code.
- **On what.** It was judged on the working tree, but what lands is committed.
  `mark_done` commits HEAD plus its status flip. A dirty tree is banked by the
  engine as a wip commit. The merge endpoint accepts a wip-banked run like any
  other: its merge status is `skipped`, a claimable state, and the run record
  does not say the head was banked.

Measured before this decision, on synthetic programmes, through the engine's
own finalize and deferred merge:

| lot | outcome |
|---|---|
| a gate command commits a contract rewrite | converged; `mark_done` committed `done` over it |
| a gate command leaves the rewrite uncommitted | converged, marked; the wip bank carried it, and a merge landed it on the target branch |
| a rewrite committed, then the file restored in the working tree | converged; `mark_done` committed `done` over the rewrite. The wip bank of the restore is what a squash landed |
| the same, with the restore marked `assume-unchanged` | converged; no wip bank, and the rewrite was auto-merged under `done` |

A review of a first implementation, which judged the working tree and HEAD,
measured five more ways a rewrite reached the landing under `done`:

- a rewrite staged in the **index** under `skip-worktree` or
  `assume-unchanged`, with the file on disk and HEAD both clean: `git add -A`
  leaves the entry alone, and the engine's own bank committed it;
- a **clean filter** keyed on `GIT_NO_REPLACE_OBJECTS`: the judge, which
  exports it, read the owner's bytes; the bank, which does not, stored the
  rewrite;
- a **pre-commit hook** that rewrote a contract file inside the bank's own
  commit;
- a worker commit storing the plan as a **symlink** (mode 120000) whose target
  is the plan's flipped text, taken for `mark_done`'s own earlier commit;
- an owner file beside the plan under **another name** than the five, judged by
  nothing.

## Options

- **(a) A list of forbidden edits per file.** A list protects what it names,
  and leaves the rest open. That is how `brief_targets` and the plan's
  top-level keys stayed writable.
- **(b) The owner files byte for byte, nothing else.** The register must take
  writes: a lot records the defects it finds and the fix of the ones it was
  given. A byte comparison refuses a reformatting and every legitimate record.
- **(c) One comparison, at `mark_done` only.** A refusal there ends the run
  instead of going back to the worker. And a rewrite is seen only after the
  full gate has run, which can take hours.
- **(D) One comparison, deny by default.** It covers the whole directory, runs
  before and after the gate commands, and judges both trees that land.
  `mark_done` commits only what that comparison judged.

## Decision

(D).

- **The contract is the plan's directory.** One table in `lot_verify` names
  five files, how each parses and what a lot may write in it: `plan.yaml`,
  `outcomes.json`, `brief.yaml`, `ARBITRAGE.md`, `defects-ledger.json`. Every
  other path the base holds in that directory is judged too, byte for byte.
- **Deny by default.** The table's files are compared with the run's base as
  parsed documents:
  - the owner files take no write;
  - the plan takes the running lot's own status, and new lots, which are
    proposals;
  - the register takes new entries, and the entries the lot names in
    `remediates:`. That list is read from the plan at the **base**, so a lot
    cannot widen its own licence.

  A removal, a duplicated id, a changed header, a symlink, a created or deleted
  owner file, or a duplicate key is a rewrite. A reformatting that parses to
  the same document is not. A table file that is not a regular file at the
  base, and a plan that is not a regular file at all, is a contract that
  cannot be read: what a link points at is not judged.
- **What a lot may write beside the plan.** Only its own records:
  - any path one of whose names carries the running lot's id as a prefix:
    `<lot>-report.md`, `<lot>-probe.py`, `sweeps/<lot>.md`, all of
    `<lot>-captures/`. Created, updated or removed. The prefix ends at `-`,
    `.` or `_`, so lot `L1` does not own `L10-report.md`;
  - any path the base does not hold, whatever its name: a record the lot
    creates.

  Everything else the base holds there — the owner's files, and the earlier
  lots' records — is byte-frozen. A file the owner wants protected is
  protected from the first run whose base carries it.
- **Judged on what lands.** Three trees are judged:
  - the working tree, as git would store it: `hash-object --path`, with the
    path's line-ending and encoding conversions applied;
  - the index, which a commit stores for an entry `git add` leaves alone
    (skip-worktree, assume-unchanged, a stat it trusts);
  - HEAD's committed tree.

  Two things a commit can store without either tree showing it are refused
  outright on a contract path, because they cannot be judged:
  - an index flag (skip-worktree, assume-unchanged);
  - a `filter` attribute. A clean filter is code of the judged party's
    choosing, run each time the path is stored, and it can answer the judge
    and the landing differently: the judge exports `GIT_NO_REPLACE_OBJECTS`,
    the engine's bank does not.

  History is read as committed (`GIT_NO_REPLACE_OBJECTS`).
- **Judged twice:**
  - before any gate command, as a cheap refusal;
  - after the last one, which covers the gate commands, the oracle and the
    net's harness.

  A refusal after the gates goes back to the worker through the repair loop.
  `lot_gate`'s `converged` and `stop` both carry the contract as a term, so a
  refused verdict neither lands nor ends the run.
- **`mark_done` commits only what was judged.** The verdict carries the HEAD it
  judged and, for every judged path, what a commit of the working tree would
  take: the working tree as git would store it, the index entries and flags,
  the `filter` attribute. `mark_done` refuses any of them that moved since.
  Its own work is recognised exactly, so a re-execution after a crash stays
  idempotent:
  - its line in the plan: the plan the verdict judged in the working tree
    (proposals left uncommitted included), with the one line flipped;
  - its index entry for that plan;
  - its own commit on the judged HEAD: one parent, the judged HEAD, and one
    change, the plan at its base mode carrying the blob it commits. A commit
    storing the same bytes at another mode (a symlink whose target is the
    plan's text) is not its own.
- **The engine's bank runs no hook.** The wip bank commits a run's own output
  in the run's tree, and the hooks directory is one the run can write. The
  bank's commit points `core.hooksPath` at nothing: `--no-verify` alone leaves
  `prepare-commit-msg` and `post-commit` running, and a `post-commit` hook can
  commit again on top of the bank.
- **One implementation.** Tool scripts share no code in the DSL: includes are
  for prompts only. So the comparison lives once, in `lot_verify`, and
  `mark_done` checks identity with the verdict rather than re-judging the
  contract.

## Consequences

- A contract change the programme needs is proposed by a lot, never made: an
  added lot, a paragraph in its report, or the owner's commit before the next
  run.
- A lot that records a fix in the register must be named for that entry in
  `remediates:` by the plan's author. A remediation lot drafted before this
  decision needs the field before it runs; without it, its record is refused
  by name.
- What a lot writes on an entry it remediates is not interpreted, because the
  register's vocabulary belongs to the programme. The lot's exit gate is where
  the required disposition is checked. A new entry may carry any disposition.
- Accepted limits, measured:
  - **What happens after `mark_done` is not judged by any node.** `mark_done`
    is the last node; the run's landing is made after it, by the engine.
    - On a **cloud run** the store has no working tree, so `finalizeWorktree`
      never runs: the runner banks and pushes **HEAD itself**. Its bank checks
      that the export is intact, not where HEAD stands relative to the commit
      `mark_done` reported. A process the lot left running can wait for the
      gate's commit subject and commit on top of it: the banked head carries
      the rewrite, above `done`, and no verdict of the run says so.
    - On a **local run** the engine banks the working tree as a wip commit on
      top of HEAD. Its merge status is `skipped`, which the merge endpoint
      claims like any other, and the run record does not say the head was
      banked. A write after `mark_done` rides that bank.

    No bot can close this. It needs the engine to refuse or flag a head that
    moved past the commit the terminal node reported. Until it does, a pilot
    merges a run only after re-judging the contract on the banked head.
  - The scripts an outcome's `check` runs (`ci/*`) are product files that lots
    legitimately write. They are not part of the contract.
  - A plan at the repository's root has no directory of its own: only the
    table's five files are the contract then.
  - The contract's files are found beside the plan. A brief kept elsewhere is
    not judged.
  - A supervising bot's own state kept under the plan's directory (a campaign
    journal) is frozen like any file the base holds there. A path it has not
    created yet is one a lot may create.
