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

## Running

```sh
iterion run bots/modernize/main.bot --var only_lot=L1
```

An explicit `only_lot` is answered explicitly: a lot the contract carries as
`done` or `blocked`, does not declare, or holds behind an unmet dependency is
**refused, typed** (`LOT_NOT_ACTIONABLE`, run failed), never reported as a
green no-op. The unfiltered mode keeps its legitimate no-op on an exhausted
programme.

Prerequisite: a behavioural net in the target repo. Build it with the
`golden-master` bot first.
