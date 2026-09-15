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

## Variables

| var | default | |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | The repo being modernised (the run's worktree — do not override) |
| `plan_path` | `.modernize/plan.yaml` | The programme contract, versioned in the TARGET repo |
| `only_lot` | `""` | Restrict the run to one lot id; empty = take the first ready lot |
| `max_passes` | `4` | Bounded repair loop for a single lot. Exhausting it ships what is banked and says so, rather than grinding |
| `reanchor` | `true` | Repair a mutant whose foothold this lot legitimately removed, by running the net's own bot as a subbot |
| `extend` | `true` | Act the lot's EXTENSION requests — new observation points, by pure addition — by running the net's own bot as a subbot |
| `source_issue_ref` | `""` | Issue this programme run answers, for the PR lineage |

## The two net-repair subbots

A lot is entitled to rename a method or restructure a template, and a lot's
intent may need a surface the net does not cover yet. Neither entitles the lot
to write under the net — that is the separation of powers the `golden-master`
bundle exists to hold. Two child runs of that bundle close the gap on the
lot's own checkout, each held by a deterministic check rather than by its
prompt, and each entered from `lot_gate` for exactly one pass:

- **`reanchor`** (`../golden-master/reanchor.bot`, `reanchor_loop("1")`) runs
  when `lot_verify` reports invalidated mutants. `--var reanchor=false` leaves
  them in the report and the surface they probed uncovered — the status quo,
  which nothing goes red about.
- **`extend`** (`../golden-master/extend.bot`, `extend_loop("1")`) runs when
  the lot filed extension requests in the ledger. `--var extend=false` leaves
  the requests pending, where the oracle gate refuses until a human acts them
  — pending is loud by design, never a silent skip.

Both return through `lot_verify`, so their work faces the same gate the lot's
does. Set both to `false` to keep a run purely gate-to-gate.

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
