# Morphy 🧱 — `modernize` run bilans

Carries a repository through a programme of modernisation lots — steps whose
entry and exit are both deterministic gates — against a behavioural oracle it
is forbidden to rewrite. See [bots/modernize/](../../bots/modernize/).

## 2026-09-05 — only_lot on a blocked lot exits green as nothing_to_do: a confirmed-success no-op, fixed (native:670, run 01a06d77-4d06 + a relaunch)

- Status: **ENGINE DEFECT, fixed.** Production: an operator relaunched two
  lots from bank branches whose ledger carried them as `status: blocked`
  (parked awaiting an oracle re-seal), naming each explicitly with
  `--var only_lot=<id>`. `plan_read` returned
  `{nothing_to_do: true, lot_id: "", exit_gate: ""}` for BOTH, `work_gate`
  routed straight to `done`, and both runs FINISHED green in ~5 minutes —
  no gate replayed, no work attempted, no warning. One of the two is run
  `01a06d77-4d06`.
- Root cause: `plan_read`'s lot-selection loop filters `done`/`blocked`
  lots out BEFORE checking whether they match `only_lot` — so a
  `only_lot`-named lot that is already `done`/`blocked` was skipped by the
  status filter without ever being compared against `only`, no OTHER lot
  could be chosen either (every other lot fails the `only` match), and the
  reader fell through to its "every lot in the contract is done" message —
  true of the PROGRAMME, false of what the operator actually asked for. A
  green `finished` on an explicitly requested lot that was never verified
  is the no-op-confirmed-as-success class: the operator reads convergence
  where nothing was measured.
- Fix: `plan_read` now resolves `only_lot`'s ACTUAL status FIRST, before
  the unfiltered selection loop runs at all. When the named lot is
  `done`, `blocked`, or absent from the contract entirely, it calls a new
  `not_actionable(status, notice)` terminal (distinct from the existing
  `emit()`/`refuse()` pair: `nothing_to_do=False`, `lot_not_actionable=
  True`, `lot_status=<done|blocked|absent>`, exit 0 so `work_gate` can
  route on the field) instead of falling into the unfiltered scan's
  `nothing_to_do` path. `work_gate` gained a `lot_not_actionable ->
  fail` edge, checked BEFORE the pre-existing `nothing_to_do -> done` /
  `not nothing_to_do -> upgrade_campaign` pair — the two states are
  mutually exclusive by construction (`not_actionable()` always sets
  `nothing_to_do=False`), so there is no reliance on edge declaration
  order beyond `fail` being checked first. The unfiltered "pick next"
  mode (`only_lot` empty) is byte-for-byte unchanged: an already-finished
  programme still exits `done` silently, which is the correct outcome
  there (nobody named a specific lot).
- Value: closes a class of false-confidence run, not just this one
  report — any future `only_lot` targeting a done/blocked/absent lot now
  fails loud with the lot's real status, instead of a green that means
  "the whole programme is finished" leaking onto a request that asked
  about ONE lot.
- Engine hardening: same DSL gap as native:695 (see
  [branch-improve-loop.md](branch-improve-loop.md)'s 2026-09-05 entry) —
  the `-> fail` terminal cannot carry a custom `RuntimeError` code, so
  `LOT_NOT_ACTIONABLE` and the lot's status/notice live on `work_gate`'s
  own persisted output (`iterion report` / the checkpoint's per-node
  outputs), not on the run's top-level failure message.
- Lessons for next run: `plan_read`'s python is not exercised for real by
  the e2e suite (no python3/git dependency in the hermetic Go tests, same
  convention as every other python-based tool node in this repo) — the
  new e2e coverage (`e2e/modernize_test.go`) proves the GRAPH routing from
  a stubbed `plan_read` verdict, not the python's own done/blocked/absent
  classification logic. A live dogfood against a real `.modernize/plan.yaml`
  with a genuinely blocked lot would be the next validation step.

## 2026-08-25 — the reader manufactures a red: a scalar exit_gate runs one letter at a time (run 01a033f9)

- Status: **ENGINE DEFECT, fixed.** First lot of a cloud campaign whose
  contract declared its gates as YAML scalars.
- `plan_read` joined the field with `"\n".join(gate)` unconditionally: a bare
  string was iterated character by character, the verifier's first command was
  the single letter `t` (exit 127: `t: not found`), and the fail_log opened
  with a failure no declared gate ever rendered. The lot itself had landed its
  work and reported `blocked` for its own, correct reason — the manufactured
  red was stacked on top of a real one, which is how it was noticed at all.
- Fix: scalar → one-command list at read time; any other shape is refused as
  an unreadable contract (a mapping would survive the join too — as its KEYS).
  Campaign's blocked-lot requalification reads the same field and got the same
  normalisation. Pinned by a bot-level test that executes the real `plan_read`
  against both legitimate contract forms plus the mapping refusal.

## 2026-07-28 — the modernisation converges: green gate, green net, verified twice (runs 019fa8e7, 019fa8fd)

- Status: **VALIDATED.** Two major build-tool versions and four framework lines
  crossed, with observable behaviour identical — proven, not asserted.
- Campaigns: 13 min / $0.78 (aborted on a baseline defect) then 24 min / $2.07.

### The verdict, and the second opinion

```
gate_passed true   oracle_passed true   refs_untouched true   log_tail (empty)
```

Replayed independently afterwards from the working repository, outside any run,
against a jar the new toolchain built: 7/7 mutants detected, negative control
silent across all 18 entries, collateral 0, exit 0. A green from the gate that
produced the work is worth less than a green obtained somewhere else.

### What the build said the whole time: nothing

It was green from the moment the tooling landed, and stayed green while **seven
behavioural divergences existed** — among them template security attributes
passing through uninterpreted, so an anonymous visitor received the menu entries
reserved for authenticated roles.

No build check can see that. It is not what a build does. Every argument for
putting a behavioural net underneath a modernisation reduces to this run.

### Two shortcuts refused, and why they matter more than the fixes

This was the first lot in the programme where **code was written to satisfy the
net**, which is exactly when a net becomes dangerous: the pull is to treat red as
something to remove rather than a symptom to understand. The intent carried the
test — *would this change still be right if the reference did not exist?* — and
the campaign applied it against itself twice.

- **Pinning a library back to its previous version** would have closed one
  divergence without touching a template. Refused: undoing the upgrade at the
  precise point the judge is looking is not finishing it.
- **A generally recommended code form** was refused because it added an
  attribute to the rendered HTML. A fix that changes recorded behaviour to
  repair a cause that does not affect it is not a fix.

It also flagged its own borderline case rather than burying it among the
obvious ones — a source-formatting change, kept because it states something true
about the template independently of the reference, and carrying a comment so a
later reader does not undo it by reflex. Flagging the limit case instead of
drowning it is the difference between a report and a plea.

### Lesson: entry count was the wrong metric

The preceding pass expected the divergent-entry count to fall sharply after
closing two of four causes. It did not move — 14 before, 14 after — and the pass
diagnosed why better than the operator had: several independent causes were
hiding behind entries already counted as divergent, and closing one cause on an
entry carrying two does not turn it green. The measurement that meant something
was the cause inventory: three configuration-only, four needing code.

An expectation stated in an intent is falsifiable, which is the point. This one
was falsified, and the correction came back as a finding rather than as noise.

## 2026-07-28 — second lot: a perfectly green build that does not run (run 019fa826)

- Status: **the result the whole approach exists to produce.** The lot is
  blocked, and the finding is worth more than a completed lot would have been.
- Versions: bot v0.1.0 + the declared-block stop · iterion built from HEAD ·
  `only_lot=L1b`, `max_passes=4`.
- Campaign: **27 min, 65 339 tokens, $1.63**. Gate: 9 seconds.

### The verdict

```
gate_passed     true     both exit_gate commands exited 0
refs_untouched  true     nothing moved under the oracle's references
oracle_passed   FALSE    the application no longer starts
lot_blocked     true     declared, with the cause committed
stop            true     the run ended instead of looping
```

The lot reached the programme's target build-tool version. The build is green,
the packaging task is green, there is not one warning to read. **Reported
without a behavioural net, this is a milestone**: target tooling reached,
framework plugin raised, zero regressions detected — because nothing looked.

The application does not start. Two independent causes, both in application
configuration: a bean-definition override that the older framework line
tolerated silently and the newer one refuses by default, and a JDBC driver
class the newer line derives from the connection URL which does not exist in
the pinned connector version.

Neither is exotic. Both are the ordinary consequence of a framework line moving,
and both are invisible to every check a build performs. This is the failure mode
that surfaces at deploy time on a good day, and in production on a bad one.

The gate took **nine seconds** to establish it, because the application died on
startup and the net could not even begin. The cheapest possible discovery.

### What the campaign did right, and refused to do

It **refused to fix them.** Both corrections require touching application
configuration, which the lot's intent excludes. It marked the lot blocked with
the cause written and committed, and the run stopped — the declared-block stop
added earlier the same day doing its job on its first real occasion.

It **established the minimum by measurement**, not by assumption: a table of
plugin versions actually run against the new build tool, showing which two
fail on removed APIs, which is the first that passes, and which later ones also
pass and were therefore *not* taken. "The lowest line that works" is a claim
that can be checked, unlike "the recommended version".

It **declared every version that moved** — 105 resolved dependencies, all
dictated by the platform BOM, none chosen. One artefact needed an explicit
version because the newer BOM stopped managing it; the report states the
version is unchanged from what the previous BOM resolved, and that was verified
independently afterwards.

It caught that regenerating the build wrapper **overwrites a locale block** a
previous lot had added, and re-applied it. Without that, resource processing
breaks on two files whose names carry decomposed accents — a trap that cost an
hour of diagnosis the first time a human hit it.

### Lessons

- **A green build is not a green lot, and this run is the proof.** Every
  argument for putting a behavioural net underneath a modernisation rests on
  this being real rather than theoretical. It cost $1.63 to make it real.
- A blocked lot with a committed cause is a deliverable. The next lot writes
  itself from it: the two failures are application-configuration changes, which
  is a different intent and therefore a different lot.
- Nine-second gates are suspicious and worth reading: here it was honest (the
  application died immediately), but the same shape would appear if the gate
  never reached the application at all.

## 2026-07-28 — first lot on a legacy Java/Spring target: gate green, lot blocked with cause, and a defect found in OUR net (run 019fa7b5)

- Status: **the bot behaved exactly as designed, including by refusing.** The
  lot did not complete, and that outcome is the run's main success.
- Versions: bot v0.1.0 · iterion built from HEAD · `--sandbox none` ·
  `only_lot=L1`, `max_passes=4`, `--max-duration 3h`.
- Campaign: **24 min, 38 794 tokens, $0.97**.

### What the lot produced

The build wrapper rose two major versions on the unchanged runtime. Both
`exit_gate` commands exited 0, the application built and packaged, and **not one
line changed under the oracle's reference directory** — the campaign checked
that itself before the gate did.

It then stopped short of the version the programme targets, with a precise
reason: the next major makes strict task validation blocking and rejects a task
contributed by the framework plugin, so going further would mean raising the
plugin, therefore the dependency BOM, therefore application dependency
versions — which the lot's stated intent excludes. It marked the lot `blocked`
with the cause committed, rather than overflowing.

That is the "stay inside the lot" rule holding under pressure, with no human
arbitrating. A green lot that had quietly bumped a dozen libraries would have
been worth less than this refusal.

### The finding that mattered: a hole in the behavioural net

The gate went red on `collateral: 3`, and the campaign proved the cause was
older than the lot and external to it — by replaying on the pre-lot tree,
wrapper restored and artifact rebuilt, and obtaining a character-identical diff.

One reference rendered a seeded creation date, and the seed used the database's
`current_date()`. The reference had frozen on the day the fixture was first
applied; every database seeded on another day diverged permanently, for a
reason with no business meaning.

**Two guards could not see it, structurally**, and that is the part worth
carrying forward:

- **Stability** compares captures to EACH OTHER — A/B on one boot, C after a
  restart, all the same day. A constant offset from the *reference* is
  invariant across all three, so it is invisible by construction.
- **The negative control** sampled the first six entries. Every later entry was
  therefore never once confronted with its own reference unless a mutant
  happened to target it. A reference could be stale and nothing would say so.

It surfaced only as collateral noise under three unrelated mutants, one step
away from being attributed to a lot that had not caused it. Fixed: the negative
control now covers the whole corpus and names the entry directly. Cost: one
extra pass of plain requests against an application already up.

**The general lesson.** A net records the world as it was on the day it was
recorded. Replaying it on a *different day*, from a *different path*, is a
distinct verification mode from running it again — and it is the only one that
catches this class. It has now caught six defects in a row that no additional
run would have found.

### The re-baseline, and what was deliberately not done

The fixture now pins the seeded dates, alongside the password reset and
publication-window widening it already performed. One reference was re-recorded
in consequence — two lines — with the cause written in `REBASELINE.md` and the
full counter-test replayed after: stable, noop_silent, revert_clean, 7/7
detected, collateral 0, exit 0.

The column was **not** canonicalised away. A creation date rendered in a
back-office list is business data, and neutralising it would blind the net to a
genuine regression on it. What had to be removed is that its value depended on
the day the environment was built. That distinction is the whole of the
`canonicalization` discipline in one case.

### Bot fixes this run produced

- The repair loop sent the campaign back against a wall it had already
  described and proven external to its lot. A declared **block** now stops the
  run. The asymmetry is what makes it safe: a status claiming completion could
  be self-serving and stays worthless, while one claiming failure cannot cheat
  its way to green. It is read from the contract, which the campaign must
  commit to declare it, so the claim is reviewable in a diff.
- The bundle pinned a package version that does not exist, and named the wrong
  implementation — the script uses the Go `yq`'s flag while nixpkgs `yq` is the
  Python wrapper. Install failed, the declared packages never reached PATH, and
  the run survived **only because the operator had yq on the host**.

### Lessons for next run

- L1 is `blocked` by design now. Reaching the programme's target version needs a
  lot that explicitly allows the framework plugin to move — i.e. a *different*
  lot, with its own intent and its own gate, which is exactly what the lot
  granularity is for.
- Budget by duration; the campaign cost under a dollar and the gate under three
  minutes. This bot is cheap compared to the net it verifies against.
