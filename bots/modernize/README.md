# Morphy 🧱 — `modernize`

Carries a repository through a programme of modernisation **lots** — steps
whose entry and exit are both deterministic gates — one gate-to-gate step at a
time.

## The unit is the lot

A dependency-upgrade pipeline works package by package and its failure path is
*revert this package and continue*. That is right for a dependency sweep and
useless here: a runtime upgrade is one indivisible change touching hundreds of
files, and there is no "continue without it".

## What it knows: nothing

The bot names no build tool and no runtime. Every command it runs comes from
the target repository's `.modernize/plan.yaml`. That is what makes one bot
serve any stack, and what lets a human audit the programme without reading the
bot.

## The verdict

A conjunction, never a score:

| | |
|---|---|
| `gate_passed` | every command in the lot's `exit_gate` exited 0 |
| `oracle_passed` | the behavioural net replayed green |
| `refs_untouched` | **not one line** changed under the oracle's reference dir |

Two of three is not "mostly done" — it is a lot that builds and lies, or one
that behaves and cheated.

The third check is the separation of powers, and it is verified in git rather
than trusted. A golden master dies by re-baselining: if whoever breaks a
reference can also rewrite it, green means "someone made it green", which is
not information. A missing oracle is **not** a pass either — a lot verified
without the net is verified against nothing.

## Contract

See [skills/plan-contract.md](skills/plan-contract.md). Minimal shape:

```yaml
version: 1
oracle:
  refs_dir: .golden-master/refs
lots:
  - id: L1
    title: "..."
    status: todo            # a bookmark, never evidence
    rebaseline_allowed: false
    depends_on: []
    intent: |
      what may change, and what may not
    exit_gate:
      - "the command that decides this lot"
```

`status` is read to know what to skip and **ignored** when deciding success.
A self-reported status and a verified one are different kinds of claim, and a
programme that conflates them eventually reports a milestone that never
happened.

Who may write which word is part of the contract, and it is enforced in git:

| word | written by | believed as |
|---|---|---|
| `blocked` | the worker, with the reason committed | a STOP — a claim of failure cannot cheat toward green |
| `done` | the **gate** (`mark_done`, after `gate ∧ oracle ∧ refs` went green), one line in a commit of its own | the programme's "accepted" — a landing has exactly one commit to check for it |

A `done` the worker wrote is refused by `lot_verify` **before any gate
command runs** (the revert costs seconds, the gate an hour), because a run
interrupted after such a write relaunches as a green no-op. Measured: four
`finished` runs in 24 h that crossed no gate, every one a relaunch from a
banked branch carrying a completion nobody had proven.

The contract is the plan's whole directory, and all of it is read-only inside
a lot. `lot_verify` compares the plan, the files its owner keeps beside it —
`outcomes.json`, `brief.yaml`, `ARBITRAGE.md`, `defects-ledger.json` — and
every other file the base holds there with the run's base, deny by default:

| a lot may write | nothing else |
|---|---|
| its own `status` (`blocked`) | every field of every existing lot, the plan's top-level keys |
| a NEW lot, as a proposal | the outcomes, the brief, the arbitration doctrine |
| a NEW register entry, for a defect it found | every other register entry, a removal, the register's header |
| the register entries its lot declares in `remediates:` (read at the base) | |
| its own records: a file named with its id (`<lot>-report.md`, `sweeps/<lot>.md`), or one the base does not hold | every other file the base holds beside the plan: the owner's, the earlier lots' records |

An index flag (`skip-worktree`, `assume-unchanged`) or a `filter` attribute on
a contract path is refused outright: a commit would store what no tree shows.

Anything else is refused with one named cause per file —
`.modernize/outcomes.json: outcomes[engine-target].check changed` — and goes
back to the worker like a self-written `done`.

The contract is judged on what lands, not only on the tree the gate starts
from: before the exit gate runs and again after its last command (the gate's
commands are the lot's own code), on the working tree as git would store it,
on the index and on what is committed. `mark_done` then writes `done` only on the HEAD and the
working tree that verdict judged — a commit or an edit that arrived since is
refused, never committed under the gate's subject
([ADR-107](../../docs/adr/107-a-lots-contract-is-a-directory-judged-on-what-lands.md)).

## Running

```sh
iterion run bots/modernize/main.bot --var only_lot=L1
```

An explicit `only_lot` is answered explicitly: a lot the contract carries as
`done` or `blocked`, does not declare, holds behind an unmet dependency, or
declares no `exit_gate` is a **typed verdict** (`lot_not_actionable` with its
`lot_status`) that `work_gate` routes to `fail` — run failed, never a green
no-op, never a tool error the engine would retry. The unfiltered mode keeps
its legitimate no-op on an exhausted programme.

Prerequisite: a behavioural net in the target repo. Build it with the
`golden-master` bot first.
