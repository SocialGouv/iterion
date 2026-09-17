# The local adversarial round — what iterion requires, and who pays for it

**The protocol itself lives in
[`skills/adversarial-review-loop/SKILL.md`](../../skills/adversarial-review-loop/SKILL.md)**:
scoping a round, the seven things the subagent prompt must carry, verifying the
fix harder than the finding, the two blocking conditions (class, mutation), the
three exits from a loop that stopped converging, the upstream plan review, and
the journal/retro discipline that keeps it honest. It is written to be portable
— iterion publishes it as an agent skill, and it is installable in any
repository ([docs/skill.md](../skill.md)).

This page carries what is true **here**: that the round is required, how many
rounds you are authorized to spend, who pays for them, and where the loop meets
the Revi gate. The merge contract it serves is in
[review-and-merge.md](review-and-merge.md).

## Why it is required rather than encouraged

Measured on this repo: a fresh line pushed straight to the gate took **five
consecutive `revi/review` verdicts at ≥ 1 medium, about six hours of
merge-queue time**, against **one 15-minute local round followed by a first
verdict at 0 findings**. The gate is a slow, shared, serialized reviewer.
Spending its cycles on findings a local round would have caught costs everyone
else's queue time too.

## When — a feature is delivered *through* the loop, not reviewed at the end

The round before the push is the last one, not the only one.

1. **Upstream, on a substantial plan** (multi-file, a migration, a new
   surface): a plan review by a model of **another family**, its feedback
   integrated with an explicit adopted / adjusted / dismissed disposition. The
   automated embodiment of the same idea lives in this repo:
   `plan_review` in [bots/feature-dev/main.bot](../../bots/feature-dev/main.bot),
   resolved by [`pkg/reviewtopology`](../../pkg/reviewtopology) (ADR-052) —
   which also fails *loudly* when only one model family is credentialed,
   rather than shipping an unreviewed plan in silence.
2. **While the diff grows**: a round per coherent slice, so a defect is
   attacked while its context is still loaded.
3. **Before the push**: a round on the whole diff, then a re-attack of the
   fixes' own diff (§ 5 of the skill) — the fixes are new surface, and two
   consecutive red gate verdicts on this repo landed on fixes, not on the
   original change.

## The round budget

Announce it at round 1 from the file count, never as a silent default:

```sh
root=$(git rev-parse --show-toplevel) &&
base=$(git merge-base origin/main HEAD) &&
{ git -C "$root" diff --name-only "$base"
  git -C "$root" ls-files --others --exclude-standard
} | sort -u | wc -l
```

Every shorter spelling was measured wrong on the change that introduced this
page: `origin/main...HEAD` answered **0** (three dots compare commits, so
nothing uncommitted counts), the local `main...HEAD` answered **1554** (a
seven-day-old local branch), `@{upstream}` is *fatal* on a worktree branch, and
`git diff` alone never sees an untracked file — here that hid the new skill,
which was the point of the change. Without the `&&` chain a failed `merge-base`
still prints a plausible count on stdout; without `git -C "$root"`, running
from a package subdirectory drops every untracked file outside it.

Read the list, not just the count: incidental churn (a lockfile a tool rewrote)
counts as a file and is not part of your change.

| Changed files at round 1 | Opening estimate | Ceiling of **local** rounds |
|---|---|---|
| ≤ 8 | ~3 | **5** |
| 9–25 | ~6 | **20** |
| > 25 | ~8 | **50** |

The bands are read on the file count alone, so exactly one row matches. One
adjustment, and only one: **blocking code in a small diff — a hook, a lint, a
guard, a filter — raises the first row's ceiling from 5 to 10**, because a
false positive there breaks as much as a hole and is harder to see. The wider
bands already have the room.

Two numbers, two jobs, and neither is a schedule.

The **estimate is an opening prediction**, not a rule: how many rounds a change
needs depends on much more than its file count — how blocking the code is, how
new the surface is, how much of it the loop just rewrote. Announcing one makes
the cost visible; being wrong about it is normal. Observed convergence across
the loops behind this protocol, this repo among them: **1, 3, 4, 5, 5, 6, 8**
local rounds.

**What decides the next round is the findings, not the counter.** While rounds
keep returning high or critical findings that survive verification, another
round is worth its cost — stopping on the estimate would ship what the next
round would have caught. The **ceiling** is the cost bound: how many local
rounds you may spend before this change becomes everyone else's problem.

Four rules keep the ceiling honest at both ends:

- **It counts LOCAL rounds only.** A `revi/review` verdict is logged like a
  round but never budgeted — on one long branch, 6 local rounds were announced
  and held while **20 gate cycles** were paid. That branch is what the tripwire
  below now forbids: it predates it, and today it would have stopped at 3.
- **Passing the estimate closes nothing, and neither does reaching it**: say
  where things stand, what is left, and extend (+4 rounds) toward the ceiling.
- **The stop criterion never moves** — no unassumed high or critical left, and
  then the gate's verdict is what closes (§ 8 of the skill).
- **The ceiling never authorizes a non-converging loop.** Two rounds in a row
  whose highs land on the previous round's fixes means the loop is auditing its
  own output: requalify the scope, change the approach, or switch to a test of
  the guarantee. Never "one more round".

## Who pays what — the reason the ceiling is generous

A local round is paid by the plan of whoever is coding. A gate cycle is paid by
the **shared forfait / platform credential** that funds this repo's runs, and
by the merge queue everyone shares. Spending 20 local rounds to save 10 gate
cycles is a good trade for every other contributor.

It is the same economics that put the fixer campaign on pause: `/billy`'s
verify gate re-runs the full build+test (~10 min a pass) on that shared
credential, so `auto_fix_on_gate_failure` is **off** here — see
[the pause and its re-arm procedure](review-and-merge.md#billy-is-paused).
**Findings are the developer's to fix**, by hand or through another local
round.

**Tripwire on the other side of the ladder:** at the **third** `revi/review`
verdict with findings on the same PR, stop pushing. What remains is local-round
work — on one lot, 67 % of the gate's findings were regressions of earlier
fixes, against 16 % locally.

## Say what the review cost, in the commit

Two git trailers, on every commit that ships reviewed work:

```
Adversarial-Rounds: 3 local (announced 3, ceiling 5)
Adversarial-Model: claude-opus-5[1m]
```

`Adversarial-Rounds` counts the **local** rounds actually run on this change,
with the announcement and the ceiling in parentheses. `Adversarial-Model` names
the model that ran them — several, comma-separated, when a cross-family plan
review took part; `Co-Authored-By` already names the *authoring* model, which is
a different fact.

A change nobody attacked writes it explicitly:

```
Adversarial-Rounds: 0 (trivial: typo)
```

**What survives the squash, exactly.** GitHub special-cases `Co-Authored-By:`:
it dedupes those lines and re-emits them as the squashed message's last
paragraph. Nothing else gets that treatment, so on a multi-commit PR these two
trailers land mid-body, inside the `* ` bullets, where `git interpret-trailers`
no longer parses them — measured on `7e67663f5`, whose body carries four
inline `Co-Authored-By:` lines and whose `%(trailers)` prints one. They stay
**greppable** (`git log --grep='Adversarial-Rounds:' origin/main`) and readable
by a human, and they parse as real trailers only when the PR squashes a single
commit. If you want the count to parse on `main`, put it in the squash message
too.

An absent trailer is indistinguishable from an oversight — which is exactly the
ambiguity the signal exists to remove. The trailers are a claim, greppable and
falsifiable, not a decoration: a `3` that no journal line backs is the next
round's finding.

## Where the loop meets the gate

**The gate's verdict closes the loop, not the round that declares it done.** A
sterile local round is the signal that it is time to push, never a stop
criterion: rounds a "sterile" criterion called finished have been followed by
real findings from the reviewer — the criterion counted rounds, the gate counts
defects. Logged here: 8 gate verdicts, 0 false positives, **4 classes** the
internal rounds had skirted, including a HIGH after a closure had been
declared.

Revi's `questions` channel is non-blocking, and that is not permission to
ignore it: each question is **executed** (≤ 5 min) before any decision, and
gets a fix in code or docs, or a written refusal in the recap — never silence.
On PR #1292, whose 20 verdicts raised 34 questions between them, 2 were real
code defects — found by executing them, not by reading them.

Gate mechanics, merge queue, admin bypass and the release path:
[review-and-merge.md](review-and-merge.md).
