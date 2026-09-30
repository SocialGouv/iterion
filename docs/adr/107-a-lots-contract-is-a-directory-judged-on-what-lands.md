# ADR-107 — A lot's contract is a directory, judged on what lands

- **Status**: Accepted
- **Date**: 2026-09-30
- **Applies to**: `bots/modernize` (Morphy) — `lot_verify`, `lot_gate`, `mark_done`; the `plan-contract` skill it shares with `bots/assessment`
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

- **The contract is the plan's directory:** `plan.yaml`, `outcomes.json`,
  `brief.yaml`, `ARBITRAGE.md` and `defects-ledger.json`. One table in
  `lot_verify` lists each file, how it parses, and what a lot may write in it.
- **Deny by default.** Each file is compared with the run's base as a parsed
  document:
  - the owner files take no write;
  - the plan takes the running lot's own status, and new lots, which are
    proposals;
  - the register takes new entries, and the entries the lot names in
    `remediates:`. That list is read from the plan at the **base**, so a lot
    cannot widen its own licence.

  A removal, a duplicated id, a changed header, a symlink, a created or deleted
  owner file, or a duplicate key is a rewrite. A reformatting that parses to
  the same document is not.
- **Judged on what lands.** Two trees are judged:
  - the working tree, as git would store it: `hash-object --path`, with the
    path's filters and line endings applied, and the index's flags ignored;
  - HEAD's committed tree.

  History is read as committed (`GIT_NO_REPLACE_OBJECTS`).
- **Judged twice:**
  - before any gate command, as a cheap refusal;
  - after the last one, which covers the gate commands, the oracle and the
    net's harness.

  A refusal after the gates goes back to the worker through the repair loop.
  `lot_gate`'s `converged` and `stop` both carry the contract as a term, so a
  refused verdict neither lands nor ends the run.
- **`mark_done` commits only what was judged.** The verdict carries the HEAD it
  judged and the fingerprints of the working tree. `mark_done` refuses a HEAD
  or a file that moved since then. Two exceptions keep a re-execution after a
  crash idempotent, and both are exact: its own line in the plan, and its own
  commit on the judged HEAD.
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
- Accepted limits:
  - The scripts an outcome's `check` runs (`ci/*`) are product files that lots
    legitimately write. They are not part of the contract.
  - A process a lot leaves running can still write after `mark_done`, before
    the engine banks the working tree. The banked head is then mergeable
    through the merge endpoint. Closing that belongs to the engine's finalize,
    not to a bot.
  - The contract's files are found beside the plan. A brief kept elsewhere is
    not judged.
